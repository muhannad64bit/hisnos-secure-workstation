// api/gaming.go — Gaming Mode Status API
//
// Routes:
//   GET  /api/gaming/status — current gaming mode state + tuning status
//   POST /api/gaming/start  — activate gaming mode (confirm token required)
//   POST /api/gaming/stop   — deactivate gaming mode (confirm token required)
//
// The dashboard does not directly manipulate the gaming profile — it delegates
// to hisnos-gaming.sh via systemctl --user start/stop hisnos-gaming.service.
// This keeps the privilege model consistent: user service calls user script.
//
// GET /api/gaming/status reads:
//   - $XDG_RUNTIME_DIR/hisnos-gaming.lock (set by hisnos-gaming.sh)
//   - systemctl --user is-active hisnos-gaming.service
//   - systemctl is-active hisnos-gaming-tuned-start.service (system)

package api

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GamingStatus is returned by GET /api/gaming/status.
type GamingStatus struct {
	GamingActive        bool   `json:"gaming_active"`
	TunedActive         bool   `json:"tuned_active"`
	VaultIdleTimerOn    bool   `json:"vault_idle_timer_active"`
	StartedAt           string `json:"started_at,omitempty"`
	TunedStatus         string `json:"tuned_status,omitempty"`
	Timestamp           string `json:"timestamp"`
}

func (h *Handler) GamingStatus(w http.ResponseWriter, r *http.Request) {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = "/run/user/1000" // fallback — real value comes from env
	}

	lockFile := filepath.Join(runtimeDir, "hisnos-gaming.lock")
	active := false
	startedAt := ""

	if data, err := os.ReadFile(lockFile); err == nil {
		active = true
		// Parse started_at= from lock file
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "started_at=") {
				startedAt = strings.TrimPrefix(line, "started_at=")
				startedAt = strings.TrimSpace(startedAt)
				break
			}
		}
	}

	tunedActive := isSystemServiceActive("hisnos-gaming-tuned-start.service")
	vaultTimerOn := isUserServiceActive("hisnos-vault-idle.timer")

	tunedStatus := "inactive"
	if tunedActive {
		tunedStatus = "active"
	}

	writeJSON(w, http.StatusOK, GamingStatus{
		GamingActive:     active,
		TunedActive:      tunedActive,
		VaultIdleTimerOn: vaultTimerOn,
		StartedAt:        startedAt,
		TunedStatus:      tunedStatus,
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
	})
}

func (h *Handler) GamingStart(w http.ResponseWriter, r *http.Request) {
	if !h.requireConfirm(w, r) {
		return
	}

	// Route through hisnosd — policy admission check (safe-mode, update-preparing) enforced there.
	if h.hisnosdAvailable() {
		if err := h.hisnosd.StartGaming(); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("hisnosd: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "gaming_mode_starting", "via": "hisnosd"})
		return
	}

	// Fallback: direct exec.
	if err := runGamingService("start"); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start gaming mode: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "gaming_mode_starting"})
}

func (h *Handler) GamingStop(w http.ResponseWriter, r *http.Request) {
	if !h.requireConfirm(w, r) {
		return
	}

	// Route through hisnosd.
	if h.hisnosdAvailable() {
		if err := h.hisnosd.StopGaming(); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("hisnosd: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "gaming_mode_stopping", "via": "hisnosd"})
		return
	}

	// Fallback: direct exec.
	if err := runGamingService("stop"); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to stop gaming mode: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "gaming_mode_stopping"})
}

// runGamingService activates or deactivates hisnos-gaming.service (user service).
func runGamingService(action string) error {
	var cmd *exec.Cmd
	switch action {
	case "start":
		cmd = exec.Command("/usr/bin/systemctl", "--user", "start", "hisnos-gaming.service")
	case "stop":
		cmd = exec.Command("/usr/bin/systemctl", "--user", "stop", "hisnos-gaming.service")
	default:
		return nil
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

