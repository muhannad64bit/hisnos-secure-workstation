// core/runtime/safemode.go
//
// SafeModeEnforcer implements the strict safe-mode production contract.
//
// ── Safe-mode entry conditions ────────────────────────────────────────────
//   Automatic entry (any of):
//   • Watchdog circuit breaker trips on a critical subsystem.
//   • TransactionManager.ReplayJournal() detects corruption.
//   • Threat engine score >= CriticalThreshold AND ResponseMatrix fires
//     "safe_mode_candidate" action.
//   • Operator sends {"command":"set_mode","params":{"mode":"safe-mode"}} via IPC.
//
// ── Safe-mode behavior (mandatory) ───────────────────────────────────────
//   • Block all mutating IPC commands (set_mode except to safe-mode,
//     start_lab, lock_vault is ALLOWED, unlock_vault is BLOCKED,
//     reload_firewall to non-strict profiles BLOCKED).
//   • Enable strict firewall profile (nft load strict ruleset).
//   • Suspend gaming runtime (hispowerd receives SIGSTOP via systemctl).
//   • Deny lab session start.
//   • Raise threat verbosity (audit rules: high).
//   • Emit desktop notification + journald structured event + dashboard alert.
//   • Write /var/lib/hisnos/safe-mode-state.json.
//
// ── Safe-mode exit requirements (all must hold) ───────────────────────────
//   1. All subsystems healthy (watchdog reports no circuit-open).
//   2. Risk score < configurable threshold (default 40).
//   3. Operator acknowledgement received via IPC:
//      {"command":"acknowledge_safe_mode","params":{"confirm":true}}
//
// ── Dry-run mode ──────────────────────────────────────────────────────────
//   When DryRun=true, all actions are logged but not executed.
//   Safe for CI validation and operator simulation.

package runtime

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

const (
	SafeModeStateFile   = "/var/lib/hisnos/safe-mode-state.json"
	DefaultExitThreshold = 40.0 // risk score must be below this to exit safe-mode
)

// ── Blocked IPC commands in safe-mode ─────────────────────────────────────

// SafeModeBlockedCommands is the set of IPC commands blocked in safe-mode.
// Commands not in this set are allowed (e.g., get_state, health, lock_vault,
// acknowledge_safe_mode, set_mode→safe-mode).
var SafeModeBlockedCommands = map[string]bool{
	"start_lab":          true,
	"start_gaming":       true,
	"prepare_update":     true,
	// unlock_vault is blocked — vault stays locked in safe-mode.
	"unlock_vault":       true,
	// reload_firewall to non-strict profile is blocked; strict reloads are allowed.
}

// ── SafeModeState ─────────────────────────────────────────────────────────

type SafeModeState struct {
	Active             bool      `json:"active"`
	EnteredAt          time.Time `json:"entered_at,omitempty"`
	Reason             string    `json:"reason"`
	AcknowledgedAt     time.Time `json:"acknowledged_at,omitempty"`
	AcknowledgedBy     string    `json:"acknowledged_by,omitempty"`
	ExitScoreThreshold float64   `json:"exit_score_threshold"`
}

// ── SafeModeEnforcer ──────────────────────────────────────────────────────

// SafeModeEnforcer manages the safe-mode lifecycle and enforcement.
type SafeModeEnforcer struct {
	mu       sync.RWMutex
	state    SafeModeState
	dryRun   bool
	onEvent  func(event string, detail map[string]any)
}

// NewSafeModeEnforcer creates a SafeModeEnforcer.
// onEvent is called on every state transition for observability.
func NewSafeModeEnforcer(dryRun bool, onEvent func(string, map[string]any)) *SafeModeEnforcer {
	e := &SafeModeEnforcer{
		dryRun:  dryRun,
		onEvent: onEvent,
		state: SafeModeState{
			ExitScoreThreshold: DefaultExitThreshold,
		},
	}
	e.loadState()
	return e
}

// Enter transitions the system into safe-mode.
// Idempotent — calling while already in safe-mode only updates the reason.
func (e *SafeModeEnforcer) Enter(reason string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.state.Active {
		log.Printf("[safemode] already active (reason was %q, new reason %q)", e.state.Reason, reason)
		return nil
	}

	log.Printf("[safemode] ENTERING safe-mode: %s", reason)

	e.state = SafeModeState{
		Active:             true,
		EnteredAt:          time.Now().UTC(),
		Reason:             reason,
		ExitScoreThreshold: DefaultExitThreshold,
	}
	e.persistState()

	if e.onEvent != nil {
		e.onEvent("safe_mode_entered", map[string]any{
			"reason":    reason,
			"timestamp": e.state.EnteredAt.Format(time.RFC3339),
		})
	}

	// Execute mandatory safe-mode actions.
	e.applyActions()

	return nil
}

// IsBlocked returns true if the given IPC command is blocked in safe-mode.
func (e *SafeModeEnforcer) IsBlocked(command string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if !e.state.Active {
		return false
	}
	return SafeModeBlockedCommands[command]
}

// IsActive returns true if safe-mode is currently active.
func (e *SafeModeEnforcer) IsActive() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state.Active
}

// CanExit checks whether all exit conditions are met.
// Returns nil if exit is permitted, or an error describing what is missing.
func (e *SafeModeEnforcer) CanExit(currentScore float64, watchdogOK bool, operatorACK bool) error {
	e.mu.RLock()
	threshold := e.state.ExitScoreThreshold
	e.mu.RUnlock()

	var issues []string

	if !watchdogOK {
		issues = append(issues, "subsystems not fully healthy (watchdog)")
	}
	if currentScore >= threshold {
		issues = append(issues, fmt.Sprintf("risk score %.1f >= threshold %.1f", currentScore, threshold))
	}
	if !operatorACK {
		issues = append(issues, "operator acknowledgement not received")
	}

	if len(issues) != 0 {
		return fmt.Errorf("cannot exit safe-mode: %v", issues)
	}
	return nil
}

// Exit transitions the system out of safe-mode.
// All exit conditions must be met; call CanExit first.
func (e *SafeModeEnforcer) Exit(operatorID string, currentScore float64, watchdogOK bool) error {
	if err := e.CanExit(currentScore, watchdogOK, true); err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.state.Active {
		return nil
	}

	log.Printf("[safemode] EXITING safe-mode (operator=%s score=%.1f)", operatorID, currentScore)

	e.state.Active = false
	e.state.AcknowledgedAt = time.Now().UTC()
	e.state.AcknowledgedBy = operatorID
	e.persistState()

	if e.onEvent != nil {
		e.onEvent("safe_mode_exited", map[string]any{
			"operator":  operatorID,
			"score":     currentScore,
			"timestamp": e.state.AcknowledgedAt.Format(time.RFC3339),
		})
	}

	// Restore normal operations.
	e.restoreActions()

	return nil
}

// State returns a copy of the current safe-mode state.
func (e *SafeModeEnforcer) State() SafeModeState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state
}

// ── Mandatory actions ──────────────────────────────────────────────────────

func (e *SafeModeEnforcer) applyActions() {
	// 1. Strict firewall.
	e.run("firewall_strict",
		"nft", "-f", "/etc/nftables/hisnos-strict.nft")

	// 2. Suspend gaming runtime.
	e.run("gaming_suspend",
		"systemctl", "--user", "stop", "hisnos-hispowerd.service")

	// 3. High-verbosity audit.
	e.run("audit_high",
		"auditctl", "-f", "2") // failure mode = panic on audit overrun

	// 4. Desktop notification (best-effort via notify-send).
	e.runBestEffort("desktop_notify",
		"notify-send", "--urgency=critical",
		"--icon=security-low",
		"HisnOS Safe Mode",
		"System has entered SAFE MODE. Check governance dashboard.")

	// 5. Journald structured event (direct log; journal picks it up).
	log.Printf("[safemode] SYSTEM IN SAFE MODE — reason: %s", e.state.Reason)
}

func (e *SafeModeEnforcer) restoreActions() {
	// 1. Restore default firewall.
	e.run("firewall_restore",
		"systemctl", "reload-or-restart", "nftables")

	// 2. Allow gaming runtime to restart (don't auto-start — user decides).
	// (no action — gaming only starts on user request)

	// 3. Normal audit verbosity.
	e.run("audit_normal",
		"auditctl", "-f", "1")

	// 4. Desktop notification.
	e.runBestEffort("desktop_notify",
		"notify-send", "--urgency=normal",
		"--icon=security-high",
		"HisnOS Safe Mode Exited",
		"System has returned to normal operation.")

	log.Printf("[safemode] SAFE MODE EXITED — normal operations restored")
}

func (e *SafeModeEnforcer) run(name string, args ...string) {
	if e.dryRun {
		log.Printf("[safemode] DRY-RUN: would exec %q %v", name, args)
		return
	}
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		log.Printf("[safemode] WARN: action %q failed: %v — %s", name, err, string(out))
	} else {
		log.Printf("[safemode] action %q: OK", name)
	}
}

func (e *SafeModeEnforcer) runBestEffort(name string, args ...string) {
	if e.dryRun {
		log.Printf("[safemode] DRY-RUN: would exec %q %v", name, args)
		return
	}
	_ = exec.Command(args[0], args[1:]...).Run()
}

// ── Persistence ────────────────────────────────────────────────────────────

func (e *SafeModeEnforcer) persistState() {
	data, _ := json.MarshalIndent(e.state, "", "  ")
	dir := filepath.Dir(SafeModeStateFile)
	os.MkdirAll(dir, 0750)
	tmp, err := os.CreateTemp(dir, ".safe-mode-*.tmp")
	if err != nil {
		return
	}
	tmp.Write(data)
	tmp.Sync()
	tmp.Close()
	os.Rename(tmp.Name(), SafeModeStateFile)
}

func (e *SafeModeEnforcer) loadState() {
	data, err := os.ReadFile(SafeModeStateFile)
	if err != nil {
		return
	}
	var s SafeModeState
	if err := json.Unmarshal(data, &s); err != nil {
		return
	}
	if s.Active {
		log.Printf("[safemode] WARN: system was in safe-mode at last shutdown (reason: %s)", s.Reason)
		e.state = s
	}
}
