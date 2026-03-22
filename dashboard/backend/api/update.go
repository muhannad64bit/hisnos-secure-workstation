// api/update.go — Update readiness and control API handlers
//
// Routes:
//   GET  /api/update/status    — state file + rpm-ostree deployment summary
//   POST /api/update/check     — hisnos-update check (read-only, no download)
//   POST /api/update/prepare   — hisnos-update prepare (SSE stream; stages download)
//   POST /api/update/apply     — hisnos-update apply --defer (confirm required; locks vault, NO reboot)
//   POST /api/update/rollback  — hisnos-update rollback (confirm required; stages rollback, NO reboot)
//   POST /api/update/validate  — hisnos-update validate (post-reboot health check)
//
// Two-step apply design:
//   apply always uses --defer (vault locked, staged deployment ready, no reboot).
//   The dashboard presents a separate "Reboot Now" button that the user must
//   explicitly confirm. This prevents accidental reboots during work sessions.
//
// SSE format (prepare endpoint):
//   data: "line of output"\n\n     — output line (JSON-encoded string)
//   event: done\ndata: {...}\n\n   — command finished with exit_code
//   event: error\ndata: "..."\n\n  — Go-level exec error

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	execpkg "hisnos.local/dashboard/exec"
	"hisnos.local/dashboard/state"
)

// UpdateStatus returns the state file contents merged with live rpm-ostree status.
func (h *Handler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	stateData, _ := state.ReadFile(updateStatePath)

	resp := map[string]any{
		"state": stateData,
	}

	// Add live deployment info from rpm-ostree (best-effort; not fatal if unavailable)
	result, err := execpkg.Run(
		[]string{rpmOstreeBin, "status", "--json"},
		execpkg.Options{Timeout: 20 * time.Second},
	)
	if err == nil && result.ExitCode == 0 {
		var deployments any
		if err2 := json.Unmarshal([]byte(result.Stdout), &deployments); err2 == nil {
			resp["deployments"] = deployments
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// UpdateCheck runs hisnos-update check (read-only; no download).
func (h *Handler) UpdateCheck(w http.ResponseWriter, r *http.Request) {
	if !scriptAvailable(w, h.scripts.Update) {
		return
	}

	result, err := execpkg.Run(
		[]string{h.scripts.Update, "check"},
		execpkg.Options{Timeout: 3 * time.Minute},
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("exec error: %s", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"exit_code": result.ExitCode,
		"output":    result.Stdout,
	})
}

// UpdatePrepare streams hisnos-update prepare output via Server-Sent Events.
// This is the long-running download+stage operation.
// Does NOT require confirmation — prepare is non-destructive (no reboot, no vault lock).
func (h *Handler) UpdatePrepare(w http.ResponseWriter, r *http.Request) {
	if !scriptAvailable(w, h.scripts.Update) {
		return
	}

	// State guard: rollback-mode blocks update-preparing.
	s, err := h.stateMgr.GetSnapshot()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "control plane state unavailable")
		return
	}
	if s.Mode == state.ModeRollbackMode {
		writeGuardError(w, state.GuardError{
			Code:    state.ErrRollbackBlocksPrepare,
			Message: "update-preparing is forbidden while rollback-mode is active",
		})
		return
	}
	if s.Mode == state.ModeUpdatePreparing || s.Mode == state.ModeUpdatePending {
		writeGuardError(w, state.GuardError{
			Code:    state.ErrConcurrentUpdate,
			Message: "an update is already in progress (concurrent update blocked)",
		})
		return
	}
	prevMode := s.Mode

	// Transition immediately so other stateful actions (e.g. firewall reload)
	// are blocked consistently during the long-running prepare step.
	if err := h.stateMgr.TransitionMode(state.ModeUpdatePreparing, "update_prepare_start", map[string]any{}); err != nil {
		if ge, ok := err.(state.GuardError); ok {
			writeGuardError(w, ge)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update control-plane state")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "response streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering if proxied

	lines, exitCh, err := execpkg.StreamWithExitCode(r.Context(), []string{h.scripts.Update, "prepare"})
	if err != nil {
		// Prepare did not start; explicitly revert to pre-action mode.
		_ = h.stateMgr.TransitionMode(prevMode, "update_prepare_start_failed", map[string]any{})

		fmt.Fprintf(w, "event: error\ndata: %s\n\n", sseString(err.Error()))
		flusher.Flush()
		return
	}

	exitCode := 1
	for line := range lines {
		fmt.Fprintf(w, "data: %s\n\n", sseString(line))
		flusher.Flush()
	}

	// Retrieve final exit code.
	if code, ok := <-exitCh; ok {
		exitCode = code
	}

	// Update persisted facts and/or revert mode after the prepare finishes.
	if exitCode == 0 {
		factsMap, _ := state.ReadFile(updateStatePath)
		facts := state.UpdateFacts{
			StagedDeployment:  factsMap["staged_deployment"],
			StagedPrepareTime: factsMap["staged_prepare_time"],
		}
		_ = h.stateMgr.SetUpdateFacts(facts, "update_prepare_completed", map[string]any{})
		_ = h.stateMgr.TransitionMode(state.ModeUpdatePending, "update_prepare_to_pending_reboot", map[string]any{})
	} else {
		_ = h.stateMgr.TransitionMode(prevMode, "update_prepare_failed", map[string]any{"exit_code": exitCode})
	}

	fmt.Fprintf(w, "event: done\ndata: {\"exit_code\":%d}\n\n", exitCode)
	flusher.Flush()
}

// UpdateApply stages the update for reboot using --defer (vault locked, no immediate reboot).
// Requires confirmation header.
func (h *Handler) UpdateApply(w http.ResponseWriter, r *http.Request) {
	if !h.requireConfirm(w, r) {
		return
	}
	if !scriptAvailable(w, h.scripts.Update) {
		return
	}

	// State guards (deterministic safety checks).
	s, err := h.stateMgr.GetSnapshot()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "control plane state unavailable")
		return
	}
	if s.Mode == state.ModeRollbackMode {
		writeGuardError(w, state.GuardError{
			Code:    state.ErrForbiddenByState,
			Message: "update apply is forbidden while rollback-mode is active",
		})
		return
	}
	if s.Mode == state.ModeUpdatePending {
		writeGuardError(w, state.GuardError{
			Code:    state.ErrConcurrentUpdate,
			Message: "update-pending-reboot is already active (apply blocked)",
		})
		return
	}

	vaultMounted, _ := vaultMountedFromFacts()
	if vaultMounted {
		writeGuardError(w, state.GuardError{
			Code:    state.ErrVaultMustBeLocked,
			Message: "vault must be locked before update apply",
		})
		return
	}

	if err := kernelValidationOKForBooted(); err != nil {
		if ge, ok := err.(state.GuardError); ok {
			writeGuardError(w, ge)
			return
		}
		writeError(w, http.StatusInternalServerError, "kernel validation check failed")
		return
	}

	if err := firewallCompatibilityOK(); err != nil {
		if ge, ok := err.(state.GuardError); ok {
			writeGuardError(w, ge)
			return
		}
		writeError(w, http.StatusInternalServerError, "firewall compatibility check failed")
		return
	}
	prevMode := s.Mode

	result, err := execpkg.Run(
		[]string{h.scripts.Update, "apply", "--defer"},
		execpkg.Options{Timeout: 30 * time.Second},
	)
	if err != nil {
		_ = h.stateMgr.TransitionMode(prevMode, "update_apply_exec_failed", map[string]any{})

		writeError(w, http.StatusInternalServerError, fmt.Sprintf("exec error: %s", err))
		return
	}

	if result.ExitCode != 0 {
		_ = h.stateMgr.TransitionMode(prevMode, "update_apply_failed", map[string]any{"exit_code": result.ExitCode})

		writeJSON(w, http.StatusOK, map[string]any{
			"success":         false,
			"exit_code":       result.ExitCode,
			"output":          result.Stdout,
			"reboot_required": true,
		})
		return
	}

	// Update persisted update facts.
	factsMap, _ := state.ReadFile(updateStatePath)
	facts := state.UpdateFacts{
		StagedDeployment: factsMap["staged_deployment"],
		StagedPrepareTime: factsMap["staged_prepare_time"],
		LastApplyTime: factsMap["last_apply_time"],
	}
	_ = h.stateMgr.SetUpdateFacts(facts, "update_apply_completed", map[string]any{})
	_ = h.stateMgr.TransitionMode(state.ModeUpdatePending, "update_apply_to_pending_reboot", map[string]any{})

	writeJSON(w, http.StatusOK, map[string]any{
		"success":         result.ExitCode == 0,
		"exit_code":       result.ExitCode,
		"output":          result.Stdout,
		"reboot_required": true, // always: user must separately confirm reboot
	})
}

// UpdateRollback stages a rollback to the previous deployment (no immediate reboot).
// Requires confirmation header.
func (h *Handler) UpdateRollback(w http.ResponseWriter, r *http.Request) {
	if !h.requireConfirm(w, r) {
		return
	}
	if !scriptAvailable(w, h.scripts.Update) {
		return
	}

	// Guard: disallow concurrent update modes.
	s, err := h.stateMgr.GetSnapshot()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "control plane state unavailable")
		return
	}
	if s.Mode == state.ModeUpdatePreparing || s.Mode == state.ModeUpdatePending {
		writeGuardError(w, state.GuardError{
			Code:    state.ErrConcurrentUpdate,
			Message: "update rollback is forbidden while an update is in progress",
		})
		return
	}
	if s.Mode == state.ModeRollbackMode {
		writeGuardError(w, state.GuardError{
			Code:    state.ErrConcurrentUpdate,
			Message: "rollback-mode is already active (rollback blocked)",
		})
		return
	}
	prevMode := s.Mode

	if err := h.stateMgr.TransitionMode(state.ModeRollbackMode, "update_rollback_start", map[string]any{}); err != nil {
		if ge, ok := err.(state.GuardError); ok {
			writeGuardError(w, ge)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update control-plane state")
		return
	}

	result, err := execpkg.Run(
		[]string{h.scripts.Update, "rollback"},
		execpkg.Options{Timeout: 30 * time.Second},
	)
	if err != nil {
		_ = h.stateMgr.TransitionMode(prevMode, "update_rollback_exec_failed", map[string]any{})

		writeError(w, http.StatusInternalServerError, fmt.Sprintf("exec error: %s", err))
		return
	}

	if result.ExitCode != 0 {
		_ = h.stateMgr.TransitionMode(prevMode, "update_rollback_failed", map[string]any{"exit_code": result.ExitCode})
		writeJSON(w, http.StatusOK, map[string]any{
			"success":         false,
			"exit_code":       result.ExitCode,
			"output":          result.Stdout,
			"reboot_required": false,
		})
		return
	}

	// Update persisted update facts.
	factsMap, _ := state.ReadFile(updateStatePath)
	facts := state.UpdateFacts{
		LastRollbackTime: factsMap["last_rollback_time"],
	}
	_ = h.stateMgr.SetUpdateFacts(facts, "update_rollback_completed", map[string]any{})

	writeJSON(w, http.StatusOK, map[string]any{
		"success":         result.ExitCode == 0,
		"exit_code":       result.ExitCode,
		"output":          result.Stdout,
		"reboot_required": result.ExitCode == 0,
	})
}

// UpdateValidate runs hisnos-update validate and writes the result to the state file.
func (h *Handler) UpdateValidate(w http.ResponseWriter, r *http.Request) {
	if !scriptAvailable(w, h.scripts.Update) {
		return
	}

	result, err := execpkg.Run(
		[]string{h.scripts.Update, "validate"},
		execpkg.Options{Timeout: 2 * time.Minute},
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("exec error: %s", err))
		return
	}

	// Persist kernel validation result + transition out of update modes.
	factsMap, _ := state.ReadFile(updateStatePath)
	k := state.KernelValidation{
		LastResult:     factsMap["last_validate_result"],
		LastTime:       factsMap["last_validate_time"],
		LastDeployment: factsMap["last_validate_deployment"],
	}
	_ = h.stateMgr.SetKernelValidation(k, "update_validate_completed", map[string]any{
		"exit_code": result.ExitCode,
	})
	// Explicit deterministic post-validate transition:
	// - validation pass -> normal
	// - validation fail -> rollback-mode
	if result.ExitCode == 0 {
		_ = h.stateMgr.TransitionMode(state.ModeNormal, "update_validate_pass_to_normal", map[string]any{})
	} else {
		_ = h.stateMgr.TransitionMode(state.ModeRollbackMode, "update_validate_fail_to_rollback_mode", map[string]any{})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"passed":    result.ExitCode == 0,
		"exit_code": result.ExitCode,
		"output":    result.Stdout,
	})
}

// sseString JSON-encodes a string for safe embedding in SSE data lines.
// This prevents SSE framing from breaking on literal newlines in the string.
func sseString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
