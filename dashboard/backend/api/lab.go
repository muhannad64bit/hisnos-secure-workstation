// api/lab.go — Lab isolation runtime lifecycle API
//
// Routes:
//   GET  /api/lab/status  — active session info; no confirm required
//   POST /api/lab/start   — create disposable bwrap environment; confirm required
//   POST /api/lab/stop    — stop active session + transition mode; confirm required
//
// Isolation model (Phase 5 — no VM):
//   Network   : bwrap --unshare-net (new kernel network namespace, loopback only)
//   Filesystem: bwrap --tmpfs / (ephemeral root, zero persistence)
//   Resources : systemd-run --user (CPUQuota + MemoryMax via cgroup v2)
//   Tracking  : JSON session lock file in $XDG_RUNTIME_DIR
//
// Control plane integration:
//   Start  : transitions mode → lab-active (blocked during update-preparing, rollback-mode)
//   Stop   : transitions mode back from lab-active → normal
//   Guards : rejects start if another session is already active
//
// Journal events (from runtime script):
//   HISNOS_LAB_STARTED, HISNOS_LAB_STOPPED, HISNOS_LAB_CLEANUP
//
// Security notes:
//   - sample_dir is validated (must exist, must be directory, path cleaned)
//   - cmd is passed as a single string to bwrap's shell; no host shell interpolation
//   - No sudo, no setuid — relies on unprivileged user namespaces (bwrap)
//   - systemd-run --user communicates over XDG user D-Bus (/run/user/<uid>/bus)

package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	execpkg "hisnos.local/dashboard/exec"
	cpstate "hisnos.local/dashboard/state"
)

const (
	labSystemdRunBin  = "/usr/bin/systemd-run"
	labSystemctlBin   = "/usr/bin/systemctl"
	labLoggerBin      = "/usr/bin/logger"
	labUnitPrefix     = "hisnos-lab-"
	labSessionFilename = "hisnos-lab-session.json"

	labDefaultCPUQuota  = "25%"
	labDefaultMemoryMax = "512M"
)

// labSessionPath returns the path to the session lock file.
// Uses XDG_RUNTIME_DIR, which is set by systemd --user for user services.
func labSessionPath() string {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		// Fallback: standard path; uid sourced from process UID.
		runtimeDir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return filepath.Join(runtimeDir, labSessionFilename)
}

// LabNftHandles stores nftables rule handles returned by the netd at session start.
// Passed back to the netd on teardown for precise rule deletion (no chain flush).
type LabNftHandles struct {
	FwdAllow string `json:"fwd_allow,omitempty"` // lab_forward: allowlist accept rule
	FwdDrop  string `json:"fwd_drop,omitempty"`  // lab_forward: catch-all drop rule
	Masq     string `json:"masq,omitempty"`      // postrouting: masquerade rule
	InDrop   string `json:"in_drop,omitempty"`   // input: drop container→host
	InDNS    string `json:"in_dns,omitempty"`    // input: allow DNS (dns-sinkhole only)
}

// LabSession is the JSON structure of the session lock file.
// Written by the API handler before systemd-run is invoked so that
// GET /api/lab/status works immediately after POST /api/lab/start.
// Network facts (VethHostIface, NftHandles) are populated by the runtime
// script after netd setup completes.
type LabSession struct {
	SessionID           string        `json:"session_id"`
	StartTime           string        `json:"start_time"`
	Profile             string        `json:"profile"`
	UnitName            string        `json:"unit_name"`
	SampleDir           string        `json:"sample_dir,omitempty"`
	CPUQuota            string        `json:"cpu_quota"`
	MemoryMax           string        `json:"memory_max"`
	// Network facts — set at start based on stored cpstate.Lab profile
	NetProfile          string        `json:"net_profile"`
	AllowedCIDRs        []string      `json:"allowed_cidrs,omitempty"`
	ProxyAddr           string        `json:"proxy_addr,omitempty"`
	// Runtime-populated by hisnos-lab-runtime.sh after netd completes
	EffectiveNetProfile string        `json:"effective_net_profile,omitempty"`
	VethHostIface       string        `json:"veth_host_iface,omitempty"`
	NftSessionSet       string        `json:"nft_session_set,omitempty"`
	NftHandles          LabNftHandles `json:"nft_handles,omitempty"`
}

// LabStartRequest is the JSON body for POST /api/lab/start.
type LabStartRequest struct {
	Profile   string `json:"profile"`             // "isolated" (only supported profile)
	SampleDir string `json:"sample_dir,omitempty"`
	Cmd       string `json:"cmd,omitempty"`        // command inside; default: sleep infinity
	CPUQuota  string `json:"cpu_quota,omitempty"`  // e.g. "25%" — default if empty
	MemoryMax string `json:"memory_max,omitempty"` // e.g. "512M" — default if empty
	// NetProfile overrides the stored cpstate profile for this session only.
	// If empty, the stored cpstate.Lab.NetworkProfile is used.
	NetProfile   string   `json:"net_profile,omitempty"`
	AllowedCIDRs []string `json:"allowed_cidrs,omitempty"` // overrides stored CIDRs
	ProxyAddr    string   `json:"proxy_addr,omitempty"`    // overrides stored proxy_addr
}

// LabStatusResponse is the JSON payload for GET /api/lab/status.
type LabStatusResponse struct {
	Active    bool        `json:"active"`
	Session   *LabSession `json:"session,omitempty"`
	UnitState string      `json:"unit_state,omitempty"` // systemd unit state
}

// labRuntimeScript returns the absolute path to hisnos-lab-runtime.sh.
func (h *Handler) labRuntimeScript() string {
	return filepath.Join(h.hisnos, "lab", "runtime", "hisnos-lab-runtime.sh")
}

// labStopScript returns the absolute path to hisnos-lab-stop.sh.
func (h *Handler) labStopScript() string {
	return filepath.Join(h.hisnos, "lab", "runtime", "hisnos-lab-stop.sh")
}

// readLabSession reads the session lock file and returns nil if it doesn't exist.
func readLabSession() (*LabSession, error) {
	data, err := os.ReadFile(labSessionPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read lab session: %w", err)
	}
	var s LabSession
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse lab session: %w", err)
	}
	return &s, nil
}

// writeLabSession atomically writes the session lock file.
func writeLabSession(s *LabSession) error {
	path := labSessionPath()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d", path, time.Now().UnixNano())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// labUnitIsActive returns true if the systemd user unit is active.
func labUnitIsActive(unitName string) bool {
	r, err := execpkg.Run(
		[]string{labSystemctlBin, "--user", "is-active", "--quiet", unitName},
		execpkg.Options{Timeout: 5 * time.Second},
	)
	return err == nil && r.ExitCode == 0
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// LabStatus returns the current lab session state.
// Never requires confirmation — read-only.
func (h *Handler) LabStatus(w http.ResponseWriter, r *http.Request) {
	session, err := readLabSession()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("read lab session: %s", err))
		return
	}

	if session == nil {
		writeJSON(w, http.StatusOK, LabStatusResponse{Active: false})
		return
	}

	// Verify the unit is still alive (session file may be stale after a crash).
	unitActive := labUnitIsActive(session.UnitName)
	if !unitActive {
		// Unit is gone but lock file was not cleaned up (crash recovery).
		// Remove the stale lock file and return inactive.
		_ = os.Remove(labSessionPath())
		writeJSON(w, http.StatusOK, LabStatusResponse{
			Active:    false,
			UnitState: "dead (stale lock file removed)",
		})
		return
	}

	writeJSON(w, http.StatusOK, LabStatusResponse{
		Active:    true,
		Session:   session,
		UnitState: "active",
	})
}

// LabStart creates a disposable isolation environment and transitions the
// control plane mode to lab-active.
//
// Guard conditions that block start:
//   - update-preparing: rpm-ostree download in progress
//   - rollback-mode: system is in recovery posture
//   - existing active session: only one lab session at a time
func (h *Handler) LabStart(w http.ResponseWriter, r *http.Request) {
	if !h.requireConfirm(w, r) {
		return
	}

	var req LabStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Default and validate isolation profile
	if req.Profile == "" {
		req.Profile = "isolated"
	}
	if req.Profile != "isolated" {
		writeError(w, http.StatusBadRequest,
			`unsupported isolation profile — only "isolated" is supported`)
		return
	}

	// Default resource limits
	cpuQuota := req.CPUQuota
	if cpuQuota == "" {
		cpuQuota = labDefaultCPUQuota
	}
	memoryMax := req.MemoryMax
	if memoryMax == "" {
		memoryMax = labDefaultMemoryMax
	}

	// Validate sample_dir if provided
	sampleDir := ""
	if req.SampleDir != "" {
		clean := filepath.Clean(req.SampleDir)
		info, err := os.Stat(clean)
		if err != nil || !info.IsDir() {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("sample_dir does not exist or is not a directory: %s", req.SampleDir))
			return
		}
		sampleDir = clean
	}

	// Control plane guard
	s, err := h.stateMgr.GetSnapshot()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "control plane state unavailable")
		return
	}

	switch s.Mode {
	case cpstate.ModeUpdatePreparing:
		writeGuardError(w, cpstate.GuardError{
			Code:    cpstate.ErrForbiddenByState,
			Message: "lab start is forbidden while update-preparing is active — wait for prepare to complete",
		})
		return
	case cpstate.ModeRollbackMode:
		writeGuardError(w, cpstate.GuardError{
			Code:    cpstate.ErrForbiddenByState,
			Message: "lab start is forbidden in rollback-mode — validate or recover the system first",
		})
		return
	}

	// Reject if another session is already active
	existing, err := readLabSession()
	if err == nil && existing != nil && labUnitIsActive(existing.UnitName) {
		writeErrorCode(w, http.StatusConflict,
			fmt.Sprintf("lab session %s is already active — stop it first", existing.SessionID),
			string(cpstate.ErrForbiddenByState))
		return
	}

	// Verify runtime script is present and executable
	runtimeScript := h.labRuntimeScript()
	if info, err := os.Stat(runtimeScript); err != nil || info.Mode()&0111 == 0 {
		writeError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("lab runtime script not available: %s", runtimeScript))
		return
	}

	// Generate session ID
	sessionID, err := generateLabSessionID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate session ID")
		return
	}

	unitName := labUnitPrefix + sessionID + ".service"

	// Resolve effective network profile:
	// Use per-request override if provided; otherwise use stored cpstate profile.
	netProfile := req.NetProfile
	allowedCIDRs := req.AllowedCIDRs
	proxyAddr := req.ProxyAddr
	if netProfile == "" {
		// Fall back to stored control plane profile
		netProfile = string(s.Lab.NetworkProfile)
		if netProfile == "" {
			netProfile = string(cpstate.LabNetOffline)
		}
		if len(allowedCIDRs) == 0 {
			allowedCIDRs = s.Lab.AllowedCIDRs
		}
		if proxyAddr == "" {
			proxyAddr = s.Lab.ProxyAddr
		}
	}
	// Validate the resolved network profile
	if _, err := cpstate.ParseLabNetworkProfile(netProfile); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid net_profile: %s", err))
		return
	}

	// Write session lock file BEFORE starting the unit so GET /api/lab/status
	// is consistent immediately after this call returns.
	session := &LabSession{
		SessionID:    sessionID,
		StartTime:    time.Now().UTC().Format(time.RFC3339),
		Profile:      req.Profile,
		UnitName:     unitName,
		SampleDir:    sampleDir,
		CPUQuota:     cpuQuota,
		MemoryMax:    memoryMax,
		NetProfile:   netProfile,
		AllowedCIDRs: allowedCIDRs,
		ProxyAddr:    proxyAddr,
	}
	if err := writeLabSession(session); err != nil {
		writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("failed to write session file: %s", err))
		return
	}

	// Transition control plane mode to lab-active
	if err := h.stateMgr.TransitionMode(cpstate.ModeLabActive, "lab_start", map[string]any{
		"session_id": sessionID,
		"profile":    req.Profile,
	}); err != nil {
		_ = os.Remove(labSessionPath()) // rollback lock file
		if ge, ok := err.(cpstate.GuardError); ok {
			writeGuardError(w, ge)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to transition mode to lab-active")
		return
	}

	// Build systemd-run args.
	// systemd-run --user creates a transient user service unit and returns
	// immediately (asynchronous — unit starts in the background).
	// The runtime script runs bwrap; cgroup limits are enforced by systemd.
	sdArgs := []string{
		labSystemdRunBin,
		"--user",
		"--unit=" + unitName,
		"--service-type=simple",
		"--no-ask-password",
		"--property=CPUQuota=" + cpuQuota,
		"--property=MemoryMax=" + memoryMax,
		"--property=Restart=no",
		"--setenv=HISNOS_LAB_SESSION_ID=" + sessionID,
		"--",
		runtimeScript,
		"--session-id", sessionID,
		"--profile", req.Profile,
		"--net-profile", netProfile,
	}
	if len(allowedCIDRs) > 0 {
		sdArgs = append(sdArgs, "--net-cidrs", strings.Join(allowedCIDRs, ","))
	}
	if proxyAddr != "" {
		sdArgs = append(sdArgs, "--net-proxy", proxyAddr)
	}
	if sampleDir != "" {
		sdArgs = append(sdArgs, "--sample-dir", sampleDir)
	}
	if req.Cmd != "" {
		sdArgs = append(sdArgs, "--cmd", req.Cmd)
	}

	result, execErr := execpkg.Run(sdArgs, execpkg.Options{Timeout: 15 * time.Second})
	startFailed := execErr != nil || (result != nil && result.ExitCode != 0)
	if startFailed {
		// Roll back: remove lock file and revert mode transition
		_ = os.Remove(labSessionPath())
		_ = h.stateMgr.TransitionMode(s.Mode, "lab_start_failed", map[string]any{
			"session_id": sessionID,
			"error":      fmt.Sprintf("%v", execErr),
		})
		detail := fmt.Sprintf("%v", execErr)
		if result != nil && result.Stderr != "" {
			detail = strings.TrimSpace(result.Stderr)
		}
		writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("systemd-run failed: %s", detail))
		return
	}

	// Emit HISNOS_LAB_STARTED journal event from API side (runtime also logs it)
	_ = execpkg.Run(
		[]string{labLoggerBin, "-t", "hisnos-lab", "-p", "user.notice",
			fmt.Sprintf("HISNOS_LAB_STARTED session=%s profile=%s cpu=%s mem=%s",
				sessionID, req.Profile, cpuQuota, memoryMax)},
		execpkg.Options{Timeout: 3 * time.Second},
	)

	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"session_id": sessionID,
		"unit":       unitName,
		"profile":    req.Profile,
		"cpu_quota":  cpuQuota,
		"memory_max": memoryMax,
	})
}

// LabStop stops the active lab session and transitions mode back to normal.
//
// If no session is active, returns 409.
// If the unit is already dead (crash recovery), cleans up and returns success.
func (h *Handler) LabStop(w http.ResponseWriter, r *http.Request) {
	if !h.requireConfirm(w, r) {
		return
	}

	session, err := readLabSession()
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("read lab session: %s", err))
		return
	}

	if session == nil {
		writeErrorCode(w, http.StatusConflict,
			"no active lab session",
			string(cpstate.ErrForbiddenByState))
		return
	}

	sessionID := session.SessionID
	unitName := session.UnitName

	// Attempt to stop the systemd unit.
	// Use the stop script for clean shutdown (it handles SIGTERM + wait).
	stopScript := h.labStopScript()
	if info, err := os.Stat(stopScript); err == nil && info.Mode()&0111 != 0 {
		// Stop via script (handles wait + cleanup)
		execpkg.Run( //nolint:errcheck — failure is non-fatal; we clean up anyway
			[]string{stopScript, "--session-id", sessionID},
			execpkg.Options{Timeout: 30 * time.Second},
		)
	} else {
		// Fallback: systemctl --user stop directly
		execpkg.Run( //nolint:errcheck
			[]string{labSystemctlBin, "--user", "stop", unitName},
			execpkg.Options{Timeout: 15 * time.Second},
		)
	}

	// Remove lock file (belt+suspenders; runtime EXIT trap also removes it)
	_ = os.Remove(labSessionPath())

	// Transition control plane mode back from lab-active to normal.
	// This is non-fatal if the mode was already changed by another path.
	currentSnap, _ := h.stateMgr.GetSnapshot()
	if currentSnap.Mode == cpstate.ModeLabActive {
		_ = h.stateMgr.TransitionMode(cpstate.ModeNormal, "lab_stop", map[string]any{
			"session_id": sessionID,
		})
	}

	// Emit HISNOS_LAB_STOPPED journal event
	_ = execpkg.Run(
		[]string{labLoggerBin, "-t", "hisnos-lab", "-p", "user.notice",
			fmt.Sprintf("HISNOS_LAB_STOPPED session=%s reason=operator_stop", sessionID)},
		execpkg.Options{Timeout: 3 * time.Second},
	)

	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"session_id": sessionID,
	})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// generateLabSessionID generates a short random hex session identifier.
func generateLabSessionID() (string, error) {
	b := make([]byte, 6) // 12 hex chars — short enough for unit names
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
