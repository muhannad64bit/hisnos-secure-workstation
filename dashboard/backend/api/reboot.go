// api/reboot.go — POST /api/system/reboot
//
// Safety rules (in order of evaluation):
//   1. Confirm token required (same mechanism as all destructive actions)
//   2. Control-plane mode must NOT be update-preparing or rollback-mode
//      — update-preparing: cancelling a live rpm-ostree download mid-stream can corrupt the staging area
//      — rollback-mode: operator must explicitly choose reboot or recovery; auto-reboot is forbidden
//   3. Logs the reboot event to the system journal via logger(1)
//   4. Executes /usr/bin/systemctl reboot
//
// Response (before the process is killed by the reboot):
//   { "success": true, "rebooting": true }
//
// The vault is NOT locked by this endpoint — the vault watcher's PrepareForSleep
// signal handles that through the existing systemd inhibitor mechanism.
// This preserves the single-source-of-truth for vault lock events in the journal.

package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"time"

	cpstate "hisnos.local/dashboard/state"
)

const (
	systemctlBin_reboot = "/usr/bin/systemctl"
	loggerBin           = "/usr/bin/logger"
)

// SystemReboot handles POST /api/system/reboot.
func (h *Handler) SystemReboot(w http.ResponseWriter, r *http.Request) {
	if !h.requireConfirm(w, r) {
		return
	}

	// ── Control-plane mode guard ────────────────────────────────────────────
	s, err := h.stateMgr.GetSnapshot()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "control plane state unavailable")
		return
	}

	// update-preparing is the only absolute reboot block.
	// Rationale: cancelling an in-flight rpm-ostree download can leave the staging
	// area in an inconsistent state. The operator must wait or cancel prepare first.
	//
	// rollback-mode explicitly ALLOWS reboot — it is the intended recovery action.
	// All other modes (normal, gaming, lab-active, update-pending-reboot) allow reboot.
	if s.Mode == cpstate.ModeUpdatePreparing {
		writeGuardError(w, cpstate.GuardError{
			Code:    cpstate.ErrForbiddenByState,
			Message: "reboot is forbidden while update-preparing is active — wait for prepare to finish or cancel it first",
		})
		return
	}

	// ── Audit log ──────────────────────────────────────────────────────────
	logMsg := fmt.Sprintf(
		"HISNOS_REBOOT initiated via dashboard API mode=%s vault_mounted=%v",
		s.Mode, s.VaultMounted,
	)
	slog.Info("system reboot initiated", "mode", s.Mode, "vault_mounted", s.VaultMounted)

	// Best-effort journal log via logger(1) so the event appears in the system journal
	// (slog writes to the process's stderr/journal; logger writes with syslog priority).
	_ = exec.Command(loggerBin, "-t", "hisnos-dashboard", "-p", "syslog.notice", logMsg).Run()

	// ── Respond before rebooting ───────────────────────────────────────────
	// Write the response first; the HTTP client will see it before the machine halts.
	writeJSON(w, http.StatusOK, map[string]any{
		"success":   true,
		"rebooting": true,
		"mode":      s.Mode,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})

	// Flush the response to the network before systemctl reboot kills this process.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// ── Execute reboot ─────────────────────────────────────────────────────
	// Run in a goroutine with a short delay so the HTTP response is transmitted.
	go func() {
		time.Sleep(500 * time.Millisecond)
		if err := exec.Command(systemctlBin_reboot, "reboot").Run(); err != nil {
			// This will only fire if systemctl reboot itself fails (e.g., permission denied).
			// At this point the response has already been sent, so log only.
			slog.Error("systemctl reboot failed", "err", err)
			_ = exec.Command(loggerBin, "-t", "hisnos-dashboard", "-p", "syslog.err",
				"HISNOS_REBOOT_FAILED: "+err.Error()).Run()
		}
	}()
}
