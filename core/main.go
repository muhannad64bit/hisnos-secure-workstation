// core/main.go — hisnosd: HisnOS Core Control Runtime
//
// hisnosd is the authoritative governance brain for the HisnOS secure workstation.
// It provides:
//   - Persistent system state (/var/lib/hisnos/core-state.json)
//   - Internal event bus connecting all subsystems
//   - Deterministic policy evaluation (pure functions, no side effects)
//   - Orchestrators that execute system operations on behalf of policy
//   - Supervision loop (15s) that monitors child daemons and attempts recovery
//   - IPC Unix socket (/run/user/$UID/hisnosd.sock) for dashboard integration
//
// Systemd remains the init system. hisnosd is the governance layer above it.
//
// Environment variables:
//   HISNOS_DIR           — base dir for scripts (default: $HOME/.local/share/hisnos)
//   HISNOS_STATE_FILE    — core-state.json path (default: /var/lib/hisnos/core-state.json)
//   HISNOS_THREAT_STATE  — threat-state.json path (default: /var/lib/hisnos/threat-state.json)
//   HISNOS_IPC_SOCKET    — Unix socket path (default: /run/user/$UID/hisnosd.sock)
//   LOG_LEVEL            — debug|info|warn (default: info)

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"hisnos.local/hisnosd/eventbus"
	"hisnos.local/hisnosd/ipc"
	"hisnos.local/hisnosd/orchestrator"
	"hisnos.local/hisnosd/policy"
	"hisnos.local/hisnosd/state"
	"hisnos.local/hisnosd/supervisor"
)

func main() {
	log.SetPrefix("[hisnosd] ")
	log.SetFlags(log.Ldate | log.Ltime | log.Lmsgprefix)

	cfg := loadConfig()

	log.Printf("starting — state=%s socket=%s", cfg.stateFile, cfg.ipcSocket)

	// ── State manager ─────────────────────────────────────────────────────────
	mgr, err := state.NewManager(cfg.stateFile)
	if err != nil {
		// Corruption or new install — log warning and continue with defaults.
		log.Printf("WARN: %v", err)
	}

	// ── Event bus ─────────────────────────────────────────────────────────────
	bus := eventbus.New()

	// ── Orchestrators ─────────────────────────────────────────────────────────
	vaultOrch := orchestrator.NewVaultOrchestrator(cfg.hisnosDir)
	fwOrch := orchestrator.NewFirewallOrchestrator()
	labOrch := orchestrator.NewLabOrchestrator(cfg.hisnosDir)
	gamingOrch := orchestrator.NewGamingOrchestrator(cfg.hisnosDir)
	updateOrch := orchestrator.NewUpdateOrchestrator(cfg.hisnosDir)
	threatOrch := orchestrator.NewThreatOrchestrator()

	// ── Policy engine ─────────────────────────────────────────────────────────
	pe := &policy.Engine{}

	// ── IPC server ────────────────────────────────────────────────────────────
	ipcServer := ipc.New(
		cfg.ipcSocket,
		mgr,
		bus,
		pe,
		vaultOrch,
		fwOrch,
		labOrch,
		gamingOrch,
		updateOrch,
	)

	// ── Supervisor ────────────────────────────────────────────────────────────
	sup := supervisor.New(mgr, bus, threatOrch)

	// ── Policy dispatch loop ──────────────────────────────────────────────────
	// Subscribe to all events and evaluate policy on each one.
	allEvents := bus.Subscribe("")

	// ── Context and signal handling ───────────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// ── Start goroutines ──────────────────────────────────────────────────────
	errCh := make(chan error, 2)

	go func() {
		if err := ipcServer.Run(ctx); err != nil {
			errCh <- fmt.Errorf("ipc server: %w", err)
		}
	}()

	go sup.Run(ctx)

	go runPolicyLoop(ctx, allEvents, mgr, bus, pe, vaultOrch, fwOrch)

	// Emit a startup event for observability.
	bus.Emit(eventbus.EventModeChanged, map[string]any{
		"from":      "startup",
		"to":        string(mgr.Get().Mode),
		"hisnosd":   "started",
	})
	log.Printf("running — mode=%s risk=%d", mgr.Get().Mode, mgr.Get().Risk.Score)

	// ── Wait for shutdown ─────────────────────────────────────────────────────
	select {
	case sig := <-sigCh:
		log.Printf("received %s — shutting down", sig)
	case err := <-errCh:
		log.Printf("fatal: %v", err)
	}

	cancel()

	// Allow goroutines to drain (2s grace period).
	time.Sleep(2 * time.Second)

	// Persist final state.
	if err := mgr.Update(func(_ *state.SystemState) {}); err != nil {
		log.Printf("WARN: final state persist: %v", err)
	}

	log.Printf("stopped")
}

// runPolicyLoop evaluates the policy engine on every event and dispatches actions.
func runPolicyLoop(
	ctx context.Context,
	events <-chan eventbus.Event,
	mgr *state.Manager,
	bus *eventbus.Bus,
	pe *policy.Engine,
	vault *orchestrator.VaultOrchestrator,
	fw *orchestrator.FirewallOrchestrator,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			_ = ev // event received; re-evaluate current state

			st := mgr.Get()
			actions := pe.Evaluate(st)

			for _, action := range actions {
				if err := dispatchAction(action, mgr, bus, vault, fw); err != nil {
					log.Printf("WARN: action %s failed: %v", action.Type, err)
					bus.Emit(eventbus.EventSubsystemCrashed, map[string]any{
						"action": string(action.Type),
						"error":  err.Error(),
					})
				} else if action.Type != policy.ActionRejectLabStart &&
					action.Type != policy.ActionRejectGamingStart {
					// Log non-trivial actions.
					log.Printf("INFO: policy action %s — %s", action.Type, action.Reason)
					bus.Emit(eventbus.EventPolicyAction, map[string]any{
						"action": string(action.Type),
						"reason": action.Reason,
					})
				}
			}
		}
	}
}

// dispatchAction routes an Action to the appropriate orchestrator or state mutation.
func dispatchAction(
	action policy.Action,
	mgr *state.Manager,
	bus *eventbus.Bus,
	vault *orchestrator.VaultOrchestrator,
	fw *orchestrator.FirewallOrchestrator,
) error {
	switch action.Type {

	case policy.ActionForceVaultLock:
		if err := vault.Execute(action); err != nil {
			return err
		}
		_ = mgr.Update(func(s *state.SystemState) {
			s.Vault.Mounted = false
		})
		bus.Emit(eventbus.EventVaultLocked, map[string]any{"source": "policy"})

	case policy.ActionFirewallStrictProfile:
		return fw.Execute(action)

	case policy.ActionFirewallRestore:
		return fw.Execute(action)

	case policy.ActionEnterSafeMode:
		st := mgr.Get()
		if st.Mode == state.ModeSafeMode {
			return nil // already in safe mode
		}
		// Force vault lock if mounted.
		if st.Vault.Mounted {
			_ = vault.Execute(policy.Action{Type: policy.ActionForceVaultLock})
		}
		// Apply strict firewall.
		_ = fw.Execute(policy.Action{Type: policy.ActionFirewallStrictProfile})
		// Transition mode.
		_ = mgr.Update(func(s *state.SystemState) {
			s.Mode = state.ModeSafeMode
		})
		bus.Emit(eventbus.EventSafeModeEntered, map[string]any{"reason": action.Reason})
		log.Printf("[hisnosd] SAFE MODE ENTERED: %s", action.Reason)

	case policy.ActionExitSafeMode:
		_ = mgr.Update(func(s *state.SystemState) {
			s.Mode = state.ModeNormal
		})
		bus.Emit(eventbus.EventSafeModeExited, map[string]any{"reason": action.Reason})
		log.Printf("[hisnosd] safe mode exited: %s", action.Reason)

	case policy.ActionIncreaseRiskScore:
		delta := 10
		if d, ok := action.Payload["delta"].(float64); ok {
			delta = int(d)
		}
		_ = mgr.Update(func(s *state.SystemState) {
			s.Risk.Score = min(s.Risk.Score+delta, 100)
			s.Risk.Level = riskLevel(s.Risk.Score)
		})

	case policy.ActionRestartSubsystem:
		// The supervisor handles restarts; this action is informational here.
		// Log and emit so the supervisor's own restart logic can act on it.
		unit, _ := action.Payload["unit"].(string)
		log.Printf("[hisnosd] policy requests restart of %s: %s", unit, action.Reason)

	case policy.ActionRejectLabStart, policy.ActionRejectGamingStart:
		// These are admission guard hints; handled synchronously in IPC handlers.

	case policy.ActionNotify:
		log.Printf("[hisnosd] notify: %v", action.Payload)
	}
	return nil
}

// ── Configuration ─────────────────────────────────────────────────────────────

type config struct {
	hisnosDir  string
	stateFile  string
	ipcSocket  string
}

func loadConfig() config {
	home := os.Getenv("HOME")
	uid := strconv.Itoa(os.Getuid())

	hisnosDir := os.Getenv("HISNOS_DIR")
	if hisnosDir == "" {
		hisnosDir = filepath.Join(home, ".local", "share", "hisnos")
	}

	stateFile := os.Getenv("HISNOS_STATE_FILE")
	if stateFile == "" {
		stateFile = state.DefaultStateFile
	}

	ipcSocket := os.Getenv("HISNOS_IPC_SOCKET")
	if ipcSocket == "" {
		ipcSocket = fmt.Sprintf("/run/user/%s/hisnosd.sock", uid)
	}

	// Ensure state directory exists.
	stateDir := filepath.Dir(stateFile)
	if err := os.MkdirAll(stateDir, 0750); err != nil {
		log.Printf("WARN: cannot create state dir %s: %v", stateDir, err)
	}

	return config{
		hisnosDir: hisnosDir,
		stateFile: stateFile,
		ipcSocket: ipcSocket,
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func riskLevel(score int) string {
	switch {
	case score >= 81:
		return "critical"
	case score >= 51:
		return "high"
	case score >= 21:
		return "medium"
	default:
		return "low"
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
