// core/policy/policy.go — Deterministic policy evaluation engine.
//
// The Engine is a pure function: Evaluate(SystemState) → []Action.
// It reads nothing from disk and calls no external commands.
// All side effects are delegated to orchestrators that receive the returned
// Action objects.
//
// Policy rules are numbered (P1–P8) to make the evaluation order explicit
// and testable. Multiple actions may be returned per evaluation cycle.

package policy

import (
	"hisnos.local/hisnosd/state"
)

// ActionType identifies the recommended operation.
type ActionType string

const (
	ActionForceVaultLock        ActionType = "ForceVaultLock"
	ActionFirewallStrictProfile ActionType = "FirewallStrictProfile"
	ActionFirewallRestore       ActionType = "FirewallRestore"
	ActionRejectLabStart        ActionType = "RejectLabStart"
	ActionRejectGamingStart     ActionType = "RejectGamingStart"
	ActionEnterSafeMode         ActionType = "EnterSafeMode"
	ActionExitSafeMode          ActionType = "ExitSafeMode"
	ActionRestartSubsystem      ActionType = "RestartSubsystem"
	ActionIncreaseRiskScore     ActionType = "IncreaseRiskScore"
	ActionNotify                ActionType = "Notify"
)

// Action is a policy recommendation. Orchestrators execute actions; the
// policy engine only describes what should happen and why.
type Action struct {
	Type    ActionType
	Payload map[string]any
	Reason  string // human-readable trigger explanation
}

// Engine evaluates all active policies against a state snapshot.
type Engine struct{}

// Evaluate returns all recommended actions for the given state.
// Rules are evaluated independently (not short-circuited) so that all
// relevant actions are surfaced in one pass.
func (e *Engine) Evaluate(s state.SystemState) []Action {
	var out []Action

	// P1: Critical risk → force vault lock + strict firewall.
	if s.Risk.Score >= 80 && s.Mode != state.ModeSafeMode {
		if s.Vault.Mounted {
			out = append(out, Action{
				Type:   ActionForceVaultLock,
				Reason: "risk_score >= 80",
			})
		}
		out = append(out, Action{
			Type:   ActionFirewallStrictProfile,
			Reason: "risk_score >= 80",
		})
	}

	// P2: Update-preparing mode → block lab and gaming starts.
	if s.Mode == state.ModeUpdatePreparing {
		out = append(out, Action{
			Type:   ActionRejectLabStart,
			Reason: "mode=update-preparing",
		})
		out = append(out, Action{
			Type:   ActionRejectGamingStart,
			Reason: "mode=update-preparing",
		})
	}

	// P3: Lab active with vault mounted → elevate risk score.
	if s.Lab.Active && s.Vault.Mounted {
		out = append(out, Action{
			Type:    ActionIncreaseRiskScore,
			Payload: map[string]any{"delta": 10},
			Reason:  "lab_active && vault_mounted: concurrent access risk",
		})
	}

	// P4: Firewall inactive → enter safe mode.
	if !s.Firewall.Active && s.Mode != state.ModeSafeMode {
		out = append(out, Action{
			Type:   ActionEnterSafeMode,
			Reason: "firewall_inactive",
		})
	}

	// P5: Audit pipeline (logd) dead → enter safe mode.
	// Logd is non-fatal for normal operation but its absence means
	// threat signals are no longer being collected.
	if !s.Subsystems.LogdAlive && s.Mode != state.ModeSafeMode {
		out = append(out, Action{
			Type:   ActionEnterSafeMode,
			Reason: "logd_dead: audit pipeline offline",
		})
	}

	// P6: Safe mode active → block lab and gaming starts.
	if s.Mode == state.ModeSafeMode {
		out = append(out, Action{
			Type:   ActionRejectLabStart,
			Reason: "mode=safe-mode",
		})
		out = append(out, Action{
			Type:   ActionRejectGamingStart,
			Reason: "mode=safe-mode",
		})
	}

	// P7: Safe mode — clear conditions → exit safe mode.
	// All three conditions must hold before auto-exit.
	if s.Mode == state.ModeSafeMode &&
		s.Firewall.Active &&
		s.Subsystems.LogdAlive &&
		s.Risk.Score < 80 {
		out = append(out, Action{
			Type:   ActionExitSafeMode,
			Reason: "firewall_active && logd_alive && risk_score < 80",
		})
	}

	// P8: Repeated subsystem crashes → restart attempt.
	// The supervisor emits SubsystemCrashed events; the orchestrator
	// layer handles actual restarts. This action ensures the policy
	// loop surfaces the need.
	if !s.Subsystems.ThreatdAlive {
		out = append(out, Action{
			Type:    ActionRestartSubsystem,
			Payload: map[string]any{"unit": "hisnos-threatd.service", "scope": "user"},
			Reason:  "threatd_dead",
		})
	}

	return out
}

// ── Admission guard helpers ───────────────────────────────────────────────────
// These are called synchronously by the IPC command handlers to gate
// commands before they are dispatched to orchestrators.

// CanStartLab returns (true, "") if policy permits a lab start, or
// (false, reason) if a policy blocks it.
func CanStartLab(s state.SystemState) (bool, string) {
	switch {
	case s.Mode == state.ModeUpdatePreparing:
		return false, "mode=update-preparing"
	case s.Mode == state.ModeSafeMode:
		return false, "mode=safe-mode"
	case s.Lab.Active:
		return false, "lab_already_active"
	case s.Risk.Score >= 80:
		return false, "risk_score_critical"
	}
	return true, ""
}

// CanStartGaming returns (true, "") if policy permits gaming mode start.
func CanStartGaming(s state.SystemState) (bool, string) {
	switch {
	case s.Mode == state.ModeUpdatePreparing:
		return false, "mode=update-preparing"
	case s.Mode == state.ModeSafeMode:
		return false, "mode=safe-mode"
	case s.Risk.Score >= 80:
		return false, "risk_score_critical"
	}
	return true, ""
}

// CanLockVault always returns true — vault lock is never blocked by policy.
func CanLockVault(_ state.SystemState) (bool, string) {
	return true, ""
}

// CanReloadFirewall returns false only during active update lifecycle phases.
func CanReloadFirewall(s state.SystemState) (bool, string) {
	switch s.Mode {
	case state.ModeUpdatePreparing, state.ModeUpdatePendingReboot:
		return false, "mode=" + string(s.Mode)
	}
	return true, ""
}
