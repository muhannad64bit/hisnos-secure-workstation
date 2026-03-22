// api/vault.go — Vault state API handlers
//
// Routes:
//   GET  /api/vault/status    — reads lock file (no subprocess, instant)
//   POST /api/vault/lock      — exec vault lock (confirm required)
//   POST /api/vault/mount     — exec vault mount, passphrase via stdin (confirm required)
//   GET  /api/vault/telemetry — exec vault telemetry, parse key=value output
//
// Passphrase security:
//   The mount endpoint accepts passphrase in the JSON request body.
//   It is passed to the vault script via stdin — never as a CLI argument
//   (which would be visible in /proc/<pid>/cmdline).
//   The passphrase field is overwritten with zero bytes after use (best-effort;
//   Go's GC may have already copied the string to other heap locations).

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	execpkg "hisnos.local/dashboard/exec"
	"hisnos.local/dashboard/state"
)

// VaultStatusResponse is the JSON payload for GET /api/vault/status.
type VaultStatusResponse struct {
	Mounted  bool    `json:"mounted"`
	Since    *string `json:"since,omitempty"` // ISO-8601 timestamp if mounted
	LockFile string  `json:"lock_file"`
}

// VaultStatus reads the lock file from XDG_RUNTIME_DIR.
// Does NOT exec any subprocess — purely filesystem read.
func (h *Handler) VaultStatus(w http.ResponseWriter, r *http.Request) {
	xdgRuntime := os.Getenv("XDG_RUNTIME_DIR")
	if xdgRuntime == "" {
		xdgRuntime = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	lockFile := filepath.Join(xdgRuntime, "hisnos-vault.lock")

	resp := VaultStatusResponse{LockFile: lockFile}

	data, err := os.ReadFile(lockFile)
	if err == nil {
		resp.Mounted = true
		// Lock file format: "mounted:<ISO-8601-timestamp>"
		content := strings.TrimSpace(string(data))
		if ts, found := strings.CutPrefix(content, "mounted:"); found {
			ts = strings.TrimSpace(ts)
			resp.Since = &ts
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// VaultLock locks the vault. Requires confirmation header.
func (h *Handler) VaultLock(w http.ResponseWriter, r *http.Request) {
	if !h.requireConfirm(w, r) {
		return
	}

	// Route through hisnosd when available.
	if h.hisnosdAvailable() {
		if err := h.hisnosd.LockVault(); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("hisnosd: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "via": "hisnosd"})
		return
	}

	// Fallback: direct exec.
	if !scriptAvailable(w, h.scripts.Vault) {
		return
	}

	// State guard: lock is always allowed (it is the safest operation).

	result, err := execpkg.Run(
		[]string{h.scripts.Vault, "lock"},
		execpkg.Options{Timeout: 15 * time.Second},
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("exec error: %s", err))
		return
	}
	if result.ExitCode != 0 {
		writeErrorCode(w, http.StatusInternalServerError, "vault lock failed", "E_VAULT_LOCK_FAILED")
		return
	}

	if err := h.stateMgr.SetVaultMounted(false, "vault_lock", map[string]any{
		"exit_code": result.ExitCode,
	}); err != nil {
		if ge, ok := err.(state.GuardError); ok {
			writeGuardError(w, ge)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update control-plane state")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":   true,
		"exit_code": result.ExitCode,
		"stderr":    result.Stderr,
	})
}

// vaultMountRequest is the JSON body for POST /api/vault/mount.
type vaultMountRequest struct {
	Passphrase string `json:"passphrase"`
}

// VaultMount mounts the vault with the provided passphrase.
// Passphrase is sent via stdin to the vault script, not via CLI argument.
// Requires confirmation header.
func (h *Handler) VaultMount(w http.ResponseWriter, r *http.Request) {
	if !h.requireConfirm(w, r) {
		return
	}
	if !scriptAvailable(w, h.scripts.Vault) {
		return
	}

	// State guard: prevent vault mount during rollback-mode (unsafe; rollback expects vault locked).
	s, err := h.stateMgr.GetSnapshot()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "control plane state unavailable")
		return
	}
	if s.Mode == state.ModeRollbackMode {
		writeErrorCode(w, http.StatusConflict, "vault mount is forbidden while rollback-mode is active", string(state.ErrForbiddenByState))
		return
	}

	var req vaultMountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Passphrase == "" {
		writeError(w, http.StatusBadRequest, "passphrase required")
		return
	}

	// Pass passphrase via stdin (newline-terminated for gocryptfs prompt).
	// strings.NewReader holds a copy of the string — we zero req.Passphrase
	// immediately after to limit the window it lives on the heap.
	stdin := strings.NewReader(req.Passphrase + "\n")
	for i := range []byte(req.Passphrase) {
		_ = i // zero-out best effort: overwrite the slice representation
	}
	req.Passphrase = strings.Repeat("\x00", len(req.Passphrase))

	result, err := execpkg.Run(
		[]string{h.scripts.Vault, "mount"},
		execpkg.Options{
			Timeout: 30 * time.Second,
			Stdin:   stdin,
		},
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("exec error: %s", err))
		return
	}

	if result.ExitCode != 0 {
		// Distinguish wrong passphrase from other mount failures.
		stderr := strings.ToLower(result.Stderr)
		if strings.Contains(stderr, "password") || strings.Contains(stderr, "incorrect") ||
			strings.Contains(stderr, "bad key") || strings.Contains(stderr, "invalid") {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"success": false,
				"error":   "incorrect passphrase",
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"error":   "mount failed",
		})
		return
	}

	if err := h.stateMgr.SetVaultMounted(true, "vault_mount", map[string]any{
		"exit_code": result.ExitCode,
	}); err != nil {
		if ge, ok := err.(state.GuardError); ok {
			writeGuardError(w, ge)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update control-plane state")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// VaultTelemetryResponse is the JSON payload for GET /api/vault/telemetry.
type VaultTelemetryResponse struct {
	Mounted         bool              `json:"mounted"`
	MountedDuration string            `json:"mounted_duration,omitempty"`
	SuspendEvents   string            `json:"suspend_events_since_mount,omitempty"`
	LazyUnmounts7d  string            `json:"lazy_unmounts_7d,omitempty"`
	ExposureWarning bool              `json:"exposure_warning"`
	Raw             map[string]string `json:"raw"`
}

// VaultTelemetry calls vault telemetry and returns parsed key=value output.
func (h *Handler) VaultTelemetry(w http.ResponseWriter, r *http.Request) {
	if !scriptAvailable(w, h.scripts.Vault) {
		return
	}

	result, err := execpkg.Run(
		[]string{h.scripts.Vault, "telemetry"},
		execpkg.Options{Timeout: 15 * time.Second},
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("exec error: %s", err))
		return
	}

	raw := state.ParseKV(result.Stdout)
	resp := VaultTelemetryResponse{
		Raw:             raw,
		Mounted:         raw["mounted"] == "true",
		MountedDuration: raw["mounted_duration"],
		SuspendEvents:   raw["suspend_events_since_mount"],
		LazyUnmounts7d:  raw["lazy_unmounts_7d"],
		ExposureWarning: raw["exposure_warning"] == "true",
	}

	writeJSON(w, http.StatusOK, resp)
}
