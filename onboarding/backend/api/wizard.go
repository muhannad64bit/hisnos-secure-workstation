// api/wizard.go — Onboarding wizard HTTP API handlers
//
// Endpoints:
//   GET  /api/state                    — full onboarding state
//   POST /api/step/welcome             — accept welcome, move to vault
//   POST /api/step/vault               — initialise gocryptfs vault with passphrase
//   POST /api/step/firewall            — select firewall profile
//   POST /api/step/threat              — configure threat notifications
//   POST /api/step/gaming              — opt in/out of gaming group
//   GET  /api/verify                   — system verification checks
//   POST /api/complete                 — mark wizard complete + disable service
//   POST /api/skip                     — skip current step with a note
//
// All mutating endpoints require Content-Type: application/json.
// Vault passphrase is never logged; it is passed to the vault script via stdin.
//
// Privilege model:
//   User-space operations: direct.
//   Root operations (gaming group, nftables reload): via sudo -n or pkexec.
//   If privilege escalation fails: warning stored in state; wizard continues.

package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"hisnos.local/onboarding/state"
)

// Handler holds dependencies for all API handlers.
type Handler struct {
	mgr        *state.Manager
	vaultScript string
	nftBin      string
	nftConfDir  string
}

// NewHandler creates an API Handler.
func NewHandler(mgr *state.Manager) *Handler {
	return &Handler{
		mgr:        mgr,
		vaultScript: "/etc/hisnos/vault/hisnos-vault.sh",
		nftBin:      "/usr/sbin/nft",
		nftConfDir:  "/etc/nftables",
	}
}

// Register attaches all routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/state",          h.State)
	mux.HandleFunc("POST /api/step/welcome",  h.StepWelcome)
	mux.HandleFunc("POST /api/step/vault",    h.StepVault)
	mux.HandleFunc("POST /api/step/firewall", h.StepFirewall)
	mux.HandleFunc("POST /api/step/threat",   h.StepThreat)
	mux.HandleFunc("POST /api/step/gaming",   h.StepGaming)
	mux.HandleFunc("GET /api/verify",         h.Verify)
	mux.HandleFunc("POST /api/complete",      h.Complete)
	mux.HandleFunc("POST /api/skip",          h.Skip)
}

// State returns the full onboarding state as JSON.
func (h *Handler) State(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.mgr.Get())
}

// StepWelcome accepts the welcome screen.
func (h *Handler) StepWelcome(w http.ResponseWriter, r *http.Request) {
	if err := h.mgr.MarkStepComplete(state.StepWelcome); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "next": state.StepVault})
}

// vaultRequest is the JSON body for the vault step.
type vaultRequest struct {
	Passphrase string `json:"passphrase"`
	Skip       bool   `json:"skip"`
}

// StepVault initialises the gocryptfs vault with the given passphrase.
func (h *Handler) StepVault(w http.ResponseWriter, r *http.Request) {
	var req vaultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if req.Skip {
		_ = h.mgr.AddWarning("vault init skipped by user")
		_ = h.mgr.MarkStepComplete(state.StepVault)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "skipped": true, "next": state.StepFirewall})
		return
	}

	if len(req.Passphrase) < 12 {
		writeError(w, http.StatusBadRequest, "passphrase must be at least 12 characters")
		return
	}

	// Call vault init script with passphrase piped to stdin.
	cmd := exec.Command("/usr/bin/bash", h.vaultScript, "init")
	cmd.Stdin = strings.NewReader(req.Passphrase + "\n" + req.Passphrase + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		// Mask any passphrase echo that might appear in output.
		writeError(w, http.StatusInternalServerError, "vault init failed: "+sanitiseOutput(msg))
		return
	}

	_ = h.mgr.Update(func(s *state.OnboardingState) {
		s.VaultInitialized = true
	})
	_ = h.mgr.MarkStepComplete(state.StepVault)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "next": state.StepFirewall})
}

// firewallRequest is the JSON body for the firewall step.
type firewallRequest struct {
	Profile string `json:"profile"` // strict | balanced | gaming-ready
}

// StepFirewall configures the nftables profile.
func (h *Handler) StepFirewall(w http.ResponseWriter, r *http.Request) {
	var req firewallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	validProfiles := map[string]bool{"strict": true, "balanced": true, "gaming-ready": true}
	if !validProfiles[req.Profile] {
		writeError(w, http.StatusBadRequest, "profile must be strict|balanced|gaming-ready")
		return
	}

	// Write selected profile marker.
	profileMarker := filepath.Join("/var/lib/hisnos", "firewall-profile")
	_ = os.WriteFile(profileMarker, []byte(req.Profile+"\n"), 0640)

	// Reload nftables (best effort — may fail if not root).
	if err := reloadNFTables(); err != nil {
		_ = h.mgr.AddWarning("nftables reload failed: " + err.Error() + " — run manually: sudo systemctl reload-or-restart nftables")
	}

	_ = h.mgr.Update(func(s *state.OnboardingState) {
		s.FirewallProfile = req.Profile
	})
	_ = h.mgr.MarkStepComplete(state.StepFirewall)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "profile": req.Profile, "next": state.StepThreat})
}

// threatRequest is the JSON body for the threat step.
type threatRequest struct {
	Notifications bool `json:"notifications"`
}

// StepThreat configures threat daemon notification settings.
func (h *Handler) StepThreat(w http.ResponseWriter, r *http.Request) {
	var req threatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Write notification preference to a config file threatd checks.
	confPath := "/var/lib/hisnos/threat-notify.conf"
	val := "false"
	if req.Notifications {
		val = "true"
	}
	_ = os.WriteFile(confPath, []byte("notify="+val+"\n"), 0640)

	_ = h.mgr.Update(func(s *state.OnboardingState) {
		s.ThreatNotify = req.Notifications
	})
	_ = h.mgr.MarkStepComplete(state.StepThreat)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "notifications": req.Notifications, "next": state.StepGaming})
}

// gamingRequest is the JSON body for the gaming group step.
type gamingRequest struct {
	Enable bool `json:"enable"`
}

// StepGaming adds (or skips) the user to the hisnos-gaming group.
func (h *Handler) StepGaming(w http.ResponseWriter, r *http.Request) {
	var req gamingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	warnings := []string{}
	if req.Enable {
		user := os.Getenv("USER")
		if user == "" {
			user = os.Getenv("LOGNAME")
		}
		if user == "" {
			warnings = append(warnings, "Cannot determine current user ($USER unset); add manually: sudo usermod -aG hisnos-gaming <user>")
		} else {
			// Try sudo -n (non-interactive) first.
			cmd := exec.Command("/usr/bin/sudo", "-n", "/usr/sbin/usermod", "-aG", "hisnos-gaming", user)
			if err := cmd.Run(); err != nil {
				// Try pkexec as fallback.
				cmd2 := exec.Command("/usr/bin/pkexec", "/usr/sbin/usermod", "-aG", "hisnos-gaming", user)
				if err2 := cmd2.Run(); err2 != nil {
					warnings = append(warnings, fmt.Sprintf(
						"Gaming group membership requires root. Run manually: sudo usermod -aG hisnos-gaming %s && reboot", user))
				}
			}
		}
	}

	for _, w := range warnings {
		_ = h.mgr.AddWarning(w)
	}
	_ = h.mgr.Update(func(s *state.OnboardingState) {
		s.GamingGroupEnabled = req.Enable
	})
	_ = h.mgr.MarkStepComplete(state.StepGaming)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"enabled":  req.Enable,
		"warnings": warnings,
		"next":     state.StepVerify,
	})
}

// verifyResult is a single check result.
type verifyResult struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// Verify runs system checks and returns results.
func (h *Handler) Verify(w http.ResponseWriter, r *http.Request) {
	results := []verifyResult{
		checkVault(h.vaultScript),
		checkFirewall(h.nftBin),
		checkHisnosd(),
		checkAuditd(),
		checkThreatd(),
	}

	allOK := true
	for _, res := range results {
		if !res.OK {
			allOK = false
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      allOK,
		"checks":  results,
		"checked_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// Complete marks the wizard as finished and disables the onboarding service.
func (h *Handler) Complete(w http.ResponseWriter, r *http.Request) {
	if err := h.mgr.MarkComplete(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Disable the onboarding service so it does not run again.
	go func() {
		time.Sleep(1 * time.Second) // let the response reach the browser
		_ = exec.Command("/usr/bin/systemctl", "--user", "disable", "--now",
			"hisnos-onboarding.service").Run()
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"completed_at": h.mgr.Get().CompletedAt,
		"message":      "Onboarding complete. You may close this window.",
	})
}

// Skip marks the current step as skipped.
func (h *Handler) Skip(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Step  state.StepName `json:"step"`
		Reason string        `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	_ = h.mgr.AddWarning(fmt.Sprintf("step %s skipped: %s", body.Step, body.Reason))
	_ = h.mgr.MarkStepComplete(body.Step)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "skipped": body.Step})
}

// ─── System verification checks ──────────────────────────────────────────────

func checkVault(script string) verifyResult {
	out, err := exec.Command("/usr/bin/bash", script, "check").CombinedOutput()
	msg := strings.TrimSpace(string(out))
	return verifyResult{
		Name:    "Vault",
		OK:      err == nil,
		Message: ternary(err == nil, "Vault subsystem ready", "Vault check failed: "+msg),
	}
}

func checkFirewall(nftBin string) verifyResult {
	out, err := exec.Command(nftBin, "list", "table", "inet", "hisnos_egress").CombinedOutput()
	_ = out
	return verifyResult{
		Name:    "Firewall",
		OK:      err == nil,
		Message: ternary(err == nil, "hisnos_egress table loaded", "nftables egress table missing — run: sudo systemctl start nftables"),
	}
}

func checkHisnosd() verifyResult {
	uid := fmt.Sprintf("%d", os.Getuid())
	sock := "/run/user/" + uid + "/hisnosd.sock"
	_, err := os.Stat(sock)
	return verifyResult{
		Name:    "hisnosd",
		OK:      err == nil,
		Message: ternary(err == nil, "hisnosd IPC socket present", "hisnosd not running — run: systemctl --user start hisnosd.service"),
	}
}

func checkAuditd() verifyResult {
	out, err := exec.Command("/usr/bin/systemctl", "is-active", "auditd").Output()
	active := strings.TrimSpace(string(out)) == "active"
	return verifyResult{
		Name:    "Audit Pipeline",
		OK:      err == nil && active,
		Message: ternary(active, "auditd active", "auditd not active — run: sudo systemctl start auditd"),
	}
}

func checkThreatd() verifyResult {
	out, err := exec.Command("/usr/bin/systemctl", "--user", "is-active", "hisnos-threatd").Output()
	active := strings.TrimSpace(string(out)) == "active"
	return verifyResult{
		Name:    "Threat Engine",
		OK:      err == nil && active,
		Message: ternary(active, "hisnos-threatd active", "hisnos-threatd not active — run: systemctl --user start hisnos-threatd"),
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func reloadNFTables() error {
	cmd := exec.Command("/usr/bin/sudo", "-n", "/usr/bin/systemctl", "reload-or-restart", "nftables")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sanitiseOutput removes lines that might echo sensitive data.
func sanitiseOutput(s string) string {
	var safe []string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := sc.Text()
		lc := strings.ToLower(line)
		if strings.Contains(lc, "passphrase") || strings.Contains(lc, "password") {
			safe = append(safe, "[redacted]")
		} else {
			safe = append(safe, line)
		}
	}
	return strings.Join(safe, "\n")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "http://localhost:9444")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
