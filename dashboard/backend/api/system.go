package api

import (
	"encoding/json"
	"net/http"
	"time"

	cpstate "hisnos.local/dashboard/state"
)

// SystemStateResponse is the JSON payload for GET /api/system/state.
type SystemStateResponse struct {
	Mode          cpstate.Mode `json:"mode"`
	VaultMounted  bool         `json:"vault_mounted"`
	Update        any          `json:"update,omitempty"`
	Kernel        any          `json:"kernel_validation,omitempty"`
	LastTransition any         `json:"last_transition,omitempty"`
	UpdatedAt     string       `json:"updated_at"`
}

func (h *Handler) SystemState(w http.ResponseWriter, r *http.Request) {
	// Prefer the authoritative hisnosd state when available.
	if h.hisnosdAvailable() {
		data, err := h.hisnosd.GetState()
		if err == nil {
			// Merge hisnosd authoritative mode + risk into the response.
			writeJSON(w, http.StatusOK, map[string]any{
				"hisnosd_state": data,
				"source":        "hisnosd",
				"updated_at":    time.Now().UTC().Format(time.RFC3339),
			})
			return
		}
		// hisnosd call failed; fall through to local state.
	}

	s, err := h.stateMgr.GetSnapshot()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "control plane state unavailable")
		return
	}

	resp := SystemStateResponse{
		Mode:           s.Mode,
		VaultMounted:   s.VaultMounted,
		Update:         s.UpdateFacts,
		Kernel:         s.KernelValidation,
		LastTransition: s.LastTransition,
		UpdatedAt:      s.UpdatedAt.Format(time.RFC3339Nano),
	}
	writeJSON(w, http.StatusOK, resp)
}

type systemModeTransitionRequest struct {
	Mode string `json:"mode"`
}

// SystemModeTransition performs explicit operator-driven control-plane mode changes.
// This endpoint is authoritative by design: mode changes are never inferred.
func (h *Handler) SystemModeTransition(w http.ResponseWriter, r *http.Request) {
	if !h.requireConfirm(w, r) {
		return
	}

	var req systemModeTransitionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Route through hisnosd — it is the authoritative mode manager.
	if h.hisnosdAvailable() {
		if err := h.hisnosd.SetMode(req.Mode); err != nil {
			writeError(w, http.StatusConflict, "hisnosd: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"mode":    req.Mode,
			"via":     "hisnosd",
		})
		return
	}

	// Fallback: local state manager.
	target, err := cpstate.ParseMode(req.Mode)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid mode")
		return
	}

	// Only allow explicit operator-mode transitions here.
	switch target {
	case cpstate.ModeNormal, cpstate.ModeGaming, cpstate.ModeLabActive:
		// allowed
	default:
		writeErrorCode(w, http.StatusConflict,
			"mode can only be transitioned explicitly to normal/gaming/lab-active; other modes are subsystem-managed",
			string(cpstate.ErrForbiddenByState))
		return
	}

	current, err := h.stateMgr.GetSnapshot()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "control plane state unavailable")
		return
	}
	if current.Mode == cpstate.ModeUpdatePreparing || current.Mode == cpstate.ModeUpdatePending || current.Mode == cpstate.ModeRollbackMode {
		writeErrorCode(w, http.StatusConflict,
			"operator mode transitions are blocked while update/rollback lifecycle modes are active",
			string(cpstate.ErrForbiddenByState))
		return
	}

	if err := h.stateMgr.TransitionMode(target, "system_mode_transition", map[string]any{
		"source": "api",
	}); err != nil {
		if ge, ok := err.(cpstate.GuardError); ok {
			writeGuardError(w, ge)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to transition system mode")
		return
	}

	s, _ := h.stateMgr.GetSnapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"mode":    s.Mode,
	})
}

