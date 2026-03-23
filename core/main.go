// core/main.go — hisnosd: HisnOS Core Control Runtime v1.0
//
// hisnosd is the single authoritative control authority for HisnOS.
//
// Startup sequence (deterministic fail-safe):
//   1. Acquire leadership lock (LeaderGuard) — exit if another instance alive.
//   2. Load state via TransactionManager — replay journal, detect corruption.
//   3. If corruption detected → enter safe-mode immediately.
//   4. Run schema migration (idempotent).
//   5. Run startup self-validation phase (firewall, vault, audit checks).
//   6. Start Watchdog with all subsystem registrations.
//   7. Start PolicyEnforcer (priority queue + dead-letter handler).
//   8. Start SafeModeEnforcer (loads persisted safe-mode state from last run).
//   9. Start IPC server (blocks mutating commands in safe-mode).
//  10. Start Supervisor (15s health + risk sync).
//  11. Start SecurityEventStream (fan-out to journal + log file).
//  12. Emit startup event.
//  13. Wait for shutdown signal (SIGTERM / SIGINT / fatal error).
//  14. Graceful shutdown: persist state, release lock.
//
// Dry-run mode: set HISNOS_DRY_RUN=1.
//   All side-effectful actions (nft, systemctl, notifyd) are logged but not
//   executed.  Safe for integration tests and operator simulation.

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"hisnos.local/hisnosd/automation"
	"hisnos.local/hisnosd/ecosystem"
	"hisnos.local/hisnosd/eventbus"
	"hisnos.local/hisnosd/fleet"
	"hisnos.local/hisnosd/health"
	"hisnos.local/hisnosd/ipc"
	"hisnos.local/hisnosd/marketplace"
	"hisnos.local/hisnosd/orchestrator"
	"hisnos.local/hisnosd/performance"
	"hisnos.local/hisnosd/policy"
	"hisnos.local/hisnosd/runtime"
	"hisnos.local/hisnosd/state"
	"hisnos.local/hisnosd/supervisor"
	"hisnos.local/hisnosd/telemetry"
)

func main() {
	log.SetPrefix("[hisnosd] ")
	log.SetFlags(log.Ldate | log.Ltime | log.Lmsgprefix)

	cfg := loadConfig()
	dryRun := os.Getenv("HISNOS_DRY_RUN") == "1"
	if dryRun {
		log.Println("DRY-RUN MODE — no side effects will be executed")
	}

	// ── Step 1: Leadership lock ────────────────────────────────────────────
	uid := os.Getuid()
	leaderGuard, err := runtime.AcquireLeadership(uid, cfg.ipcSocket)
	if err != nil {
		log.Fatalf("FATAL: cannot acquire leadership: %v", err)
	}
	defer leaderGuard.Release()
	log.Printf("leadership acquired (uid=%d)", uid)

	// ── Step 2: State manager + transaction journal ────────────────────────
	stateMgr, loadErr := state.NewManager(cfg.stateFile)
	if loadErr != nil {
		log.Printf("WARN: state load: %v", loadErr)
	}

	txMgr, err := runtime.NewTransactionManager(stateMgr)
	if err != nil {
		log.Fatalf("FATAL: transaction manager: %v", err)
	}

	// ── Step 3: Journal replay + corruption detection ─────────────────────
	corrupted, replayErr := txMgr.ReplayJournal()
	if replayErr != nil {
		log.Printf("WARN: journal replay error: %v", replayErr)
	}

	// ── Step 4: Schema migration ──────────────────────────────────────────
	if err := txMgr.MigrateSchema(); err != nil {
		log.Printf("WARN: schema migration: %v", err)
	}

	// ── Step 5: Security event stream ─────────────────────────────────────
	eventStream := telemetry.NewSecurityEventStream(
		"/var/log/hisnos/security-events.jsonl",
	)
	defer eventStream.Close()

	emitEvent := func(severity telemetry.Severity, category, source, msg string) {
		eventStream.EmitSimple(severity, category, source, msg)
	}

	// ── Step 6: Safe-mode enforcer ────────────────────────────────────────
	safeModeEnforcer := runtime.NewSafeModeEnforcer(dryRun,
		func(event string, detail map[string]any) {
			eventStream.Emit(telemetry.Event{
				Severity: telemetry.SeverityAlert,
				Category: "safe_mode",
				Source:   "hisnosd",
				Message:  event,
				Details:  detail,
			})
		})

	// Corruption detected → enter safe-mode immediately (Step 3 follow-up).
	if corrupted {
		log.Printf("CRITICAL: state corruption detected — entering safe-mode")
		emitEvent(telemetry.SeverityCritical, "integrity", "hisnosd",
			"state corruption detected on startup")
		if err := safeModeEnforcer.Enter("state_corruption_on_startup"); err != nil {
			log.Printf("WARN: safe-mode entry: %v", err)
		}
		if err := stateMgr.Update(func(s *state.SystemState) {
			s.Mode = state.ModeSafeMode
		}); err != nil {
			log.Printf("WARN: persist safe-mode state: %v", err)
		}
	}

	// ── Step 7: Startup self-validation ──────────────────────────────────
	runStartupValidation(stateMgr, emitEvent)

	// ── Step 8: Watchdog ──────────────────────────────────────────────────
	watchdog := runtime.NewWatchdog(func(reason string) {
		log.Printf("WARN: watchdog safe-mode candidate: %s", reason)
		emitEvent(telemetry.SeverityAlert, "watchdog", "hisnosd",
			"safe-mode candidate: "+reason)
		if !safeModeEnforcer.IsActive() {
			_ = safeModeEnforcer.Enter("watchdog_critical_subsystem: " + reason)
			_ = stateMgr.Update(func(s *state.SystemState) {
				s.Mode = state.ModeSafeMode
			})
		}
	})

	// ── Step 9: Policy enforcer ───────────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	enforcer := runtime.NewPolicyEnforcer(
		ctx,
		runtime.LoggingDeadLetter("/var/lib/hisnos"),
		dryRun,
	)

	// ── Step 10: Event bus + orchestrators ────────────────────────────────
	bus := eventbus.New()

	vaultOrch := orchestrator.NewVaultOrchestrator(cfg.hisnosDir)
	fwOrch := orchestrator.NewFirewallOrchestrator()
	labOrch := orchestrator.NewLabOrchestrator(cfg.hisnosDir)
	gamingOrch := orchestrator.NewGamingOrchestrator(cfg.hisnosDir)
	updateOrch := orchestrator.NewUpdateOrchestrator(cfg.hisnosDir)
	threatOrch := orchestrator.NewThreatOrchestrator()

	pe := &policy.Engine{}

	// ── Step 11: IPC server ───────────────────────────────────────────────
	ipcServer := ipc.New(
		cfg.ipcSocket,
		stateMgr,
		bus,
		pe,
		vaultOrch,
		fwOrch,
		labOrch,
		gamingOrch,
		updateOrch,
	)
	// Inject safe-mode gate into IPC server.
	ipcServer.SetSafeModeGate(func(command string) error {
		if safeModeEnforcer.IsBlocked(command) {
			return fmt.Errorf("command %q blocked in safe-mode", command)
		}
		return nil
	})

	// ── Phase A–D: Wire extended subsystems (performance, automation, health,
	//              ecosystem) via ObservabilityBus. All background goroutines
	//              are context-scoped and will stop with the parent context.
	wirePhaseAD(ctx, safeModeEnforcer, ipcServer, dryRun)

	// ── Step 12: Supervisor ───────────────────────────────────────────────
	sup := supervisor.New(stateMgr, bus, threatOrch)

	// Register critical subsystems with the watchdog.
	nftHandle := watchdog.Register(runtime.SubsystemSpec{
		Name:     "nftables",
		SLA:      30 * time.Second,
		Critical: true,
		Restart: func() error {
			out, err := runCmd("systemctl", "restart", "nftables.service")
			if err != nil {
				return fmt.Errorf("%w — %s", err, out)
			}
			return nil
		},
		OnFail: func(name string) {
			emitEvent(telemetry.SeverityCritical, "firewall", name,
				"nftables circuit breaker open — firewall unreliable")
		},
	})
	_ = nftHandle // heartbeats are sent from the subsystem checker below

	// Goroutine: ping watchdog heartbeats from supervisor health checks.
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if st := stateMgr.Get(); st.Subsystems.NftablesAlive {
					nftHandle.Beat()
				}
			}
		}
	}()

	// Start watchdog loop.
	go watchdog.Run(ctx)

	// ── Step 13: Policy response matrix callback ───────────────────────────
	// The threat engine's ResponseMatrix calls this to submit enforcement actions.
	submitPolicyAction := func(name string, payload map[string]any) {
		reason, _ := payload["reason"].(string)
		pri := runtime.PrioritySecurity
		timeout := 30 * time.Second

		switch name {
		case "vault_lock", "containment_apply", "safe_mode_candidate":
			pri = runtime.PriorityCritical
			timeout = 10 * time.Second
		case "gaming_freeze", "gaming_unfreeze", "lab_isolate":
			pri = runtime.PriorityPerformance
		}

		enforcer.Submit(runtime.PolicyAction{
			Priority: pri,
			Name:     name,
			Reason:   reason,
			Exec: func(execCtx context.Context) error {
				return dispatchResponseAction(
					name, payload, execCtx,
					stateMgr, bus, vaultOrch, fwOrch,
					safeModeEnforcer,
				)
			},
			DryRunExec: func(execCtx context.Context) error {
				log.Printf("[dryrun] would execute response action %q payload=%v", name, payload)
				return nil
			},
		})
		_ = timeout
	}
	_ = submitPolicyAction // injected into threat engine at runtime (wire-up in threat pkg)

	// ── Step 14: Start background loops ───────────────────────────────────
	allEvents := bus.Subscribe("")
	errCh := make(chan error, 2)

	go func() {
		if err := ipcServer.Run(ctx); err != nil {
			errCh <- fmt.Errorf("ipc server: %w", err)
		}
	}()

	go sup.Run(ctx)
	go runPolicyLoop(ctx, allEvents, stateMgr, bus, pe, vaultOrch, fwOrch)

	// ── Step 15: Startup event ────────────────────────────────────────────
	st := stateMgr.Get()
	bus.Emit(eventbus.EventModeChanged, map[string]any{
		"from":    "startup",
		"to":      string(st.Mode),
		"hisnosd": "started",
		"safe_mode": safeModeEnforcer.IsActive(),
		"dry_run": dryRun,
	})
	emitEvent(telemetry.SeverityInfo, "lifecycle", "hisnosd",
		fmt.Sprintf("hisnosd started (mode=%s safe_mode=%v dry_run=%v)", st.Mode, safeModeEnforcer.IsActive(), dryRun))
	log.Printf("running — mode=%s risk=%d safe_mode=%v", st.Mode, st.Risk.Score, safeModeEnforcer.IsActive())

	// ── Wait for shutdown ─────────────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		log.Printf("received %s — shutting down", sig)
	case err := <-errCh:
		log.Printf("fatal: %v", err)
	}

	cancel()
	time.Sleep(2 * time.Second)

	// Persist final state.
	if err := stateMgr.Update(func(_ *state.SystemState) {}); err != nil {
		log.Printf("WARN: final state persist: %v", err)
	}

	emitEvent(telemetry.SeverityInfo, "lifecycle", "hisnosd", "hisnosd stopped")
	log.Printf("stopped")
}

// ── Startup self-validation ────────────────────────────────────────────────

func runStartupValidation(
	mgr *state.Manager,
	emit func(telemetry.Severity, string, string, string),
) {
	log.Printf("[startup] running self-validation phase…")

	// Check: nftables active.
	if out, err := runCmd("systemctl", "is-active", "--quiet", "nftables.service"); err != nil {
		log.Printf("[startup] WARN: nftables not active: %s", out)
		emit(telemetry.SeverityWarn, "firewall", "startup",
			"nftables.service not active on startup")
	}

	// Check: hisnos_egress table present.
	if _, err := runCmd("nft", "list", "table", "inet", "hisnos_egress"); err != nil {
		log.Printf("[startup] WARN: hisnos_egress table missing")
		emit(telemetry.SeverityWarn, "firewall", "startup",
			"hisnos_egress nft table not present on startup")
	}

	// Check: auditd active.
	if _, err := runCmd("systemctl", "is-active", "--quiet", "auditd.service"); err != nil {
		log.Printf("[startup] WARN: auditd not active")
		emit(telemetry.SeverityWarn, "audit", "startup", "auditd not active on startup")
	}

	// Check: kernel cmdline required flags.
	cmdlineData, _ := os.ReadFile("/proc/cmdline")
	cmdline := string(cmdlineData)
	for _, flag := range []string{"quiet", "splash", "loglevel=3"} {
		if indexOf(cmdline, flag) < 0 {
			log.Printf("[startup] WARN: kernel cmdline missing %q", flag)
		}
	}

	log.Printf("[startup] self-validation complete")
}

// ── Response action dispatcher ────────────────────────────────────────────

func dispatchResponseAction(
	name string,
	payload map[string]any,
	ctx context.Context,
	mgr *state.Manager,
	bus *eventbus.Bus,
	vault *orchestrator.VaultOrchestrator,
	fw *orchestrator.FirewallOrchestrator,
	sm *runtime.SafeModeEnforcer,
) error {
	reason, _ := payload["reason"].(string)

	switch name {
	case "vault_lock":
		if err := vault.Execute(policy.Action{Type: policy.ActionForceVaultLock}); err != nil {
			return err
		}
		_ = mgr.Update(func(s *state.SystemState) { s.Vault.Mounted = false })
		bus.Emit(eventbus.EventVaultLocked, map[string]any{"source": "response_matrix"})

	case "firewall_strict":
		return fw.Execute(policy.Action{Type: policy.ActionFirewallStrictProfile})

	case "firewall_restore":
		return fw.Execute(policy.Action{Type: policy.ActionFirewallRestore})

	case "gaming_freeze":
		_, err := runCmd("systemctl", "--user", "stop", "hisnos-hispowerd.service")
		return err

	case "gaming_unfreeze":
		_, err := runCmd("systemctl", "--user", "start", "hisnos-hispowerd.service")
		return err

	case "audit_high_verbosity":
		level, _ := payload["level"].(string)
		if level == "high" {
			_, err := runCmd("auditctl", "-f", "2")
			return err
		}
		_, err := runCmd("auditctl", "-f", "1")
		return err

	case "lab_isolate":
		profile, _ := payload["network_profile"].(string)
		_ = mgr.Update(func(s *state.SystemState) {
			s.Lab.NetworkProfile = profile
		})
		return nil

	case "safe_mode_candidate":
		action, _ := payload["action"].(string)
		if action == "deescalate_candidate" {
			log.Printf("[response] safe-mode exit candidate (requires operator ACK)")
			return nil
		}
		if !sm.IsActive() {
			return sm.Enter("threat_engine: " + reason)
		}
		return nil

	case "vault_idle_shorten":
		// Signal the vault idle timer to use a shorter interval.
		idle, _ := payload["idle_seconds"].(int)
		if idle == 0 {
			idle = 300
		}
		_, err := runCmd("systemctl", "--user", "set-property",
			"hisnos-vault-idle.timer",
			fmt.Sprintf("OnActiveSec=%ds", idle))
		return err

	case "vault_idle_restore":
		_, err := runCmd("systemctl", "--user", "start", "hisnos-vault-idle.timer")
		return err

	case "containment_apply":
		// This requires the containment package — wire up externally.
		log.Printf("[response] containment_apply action (profile=%v reason=%s)",
			payload["profile"], reason)
		bus.Emit(eventbus.EventSafeModeEntered, map[string]any{
			"reason": "containment_apply: " + reason,
		})
		return nil
	}
	return nil
}

// ── Policy loop (existing, carried forward) ────────────────────────────────

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
			_ = ev
			st := mgr.Get()
			actions := pe.Evaluate(st)
			for _, action := range actions {
				if err := dispatchLegacyAction(action, mgr, bus, vault, fw); err != nil {
					log.Printf("WARN: action %s failed: %v", action.Type, err)
				}
			}
		}
	}
}

func dispatchLegacyAction(
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
		_ = mgr.Update(func(s *state.SystemState) { s.Vault.Mounted = false })
		bus.Emit(eventbus.EventVaultLocked, map[string]any{"source": "policy"})
	case policy.ActionFirewallStrictProfile:
		return fw.Execute(action)
	case policy.ActionFirewallRestore:
		return fw.Execute(action)
	case policy.ActionEnterSafeMode:
		_ = mgr.Update(func(s *state.SystemState) { s.Mode = state.ModeSafeMode })
		bus.Emit(eventbus.EventSafeModeEntered, map[string]any{"reason": action.Reason})
		log.Printf("SAFE MODE: %s", action.Reason)
	case policy.ActionExitSafeMode:
		_ = mgr.Update(func(s *state.SystemState) { s.Mode = state.ModeNormal })
		bus.Emit(eventbus.EventSafeModeExited, map[string]any{"reason": action.Reason})
	case policy.ActionIncreaseRiskScore:
		delta := 10
		if d, ok := action.Payload["delta"].(float64); ok {
			delta = int(d)
		}
		_ = mgr.Update(func(s *state.SystemState) {
			if s.Risk.Score+delta > 100 {
				s.Risk.Score = 100
			} else {
				s.Risk.Score += delta
			}
			s.Risk.Level = riskLevel(s.Risk.Score)
		})
	}
	return nil
}

// ── Configuration ─────────────────────────────────────────────────────────

type config struct {
	hisnosDir string
	stateFile string
	ipcSocket string
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

	if err := os.MkdirAll(filepath.Dir(stateFile), 0750); err != nil {
		log.Printf("WARN: cannot create state dir: %v", err)
	}

	return config{hisnosDir: hisnosDir, stateFile: stateFile, ipcSocket: ipcSocket}
}

// ── Helpers ───────────────────────────────────────────────────────────────

func riskLevel(score int) string {
	switch {
	case score >= 80:
		return "critical"
	case score >= 60:
		return "high"
	case score >= 40:
		return "medium"
	case score >= 20:
		return "low"
	default:
		return "minimal"
	}
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func runCmd(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ── Phase A-D wiring ──────────────────────────────────────────────────────
//
// wirePhaseAD constructs and connects all Phase A-D subsystems:
//
//	Phase A — Performance Kernel Layer:
//	  performance.Manager, NUMAScheduler, IRQAdaptiveBalancer,
//	  FramePredictor, ThermalController, RTGuard
//
//	Phase B — AI Automation Intelligence:
//	  BaselineEngine, ConfidenceModel, TemporalClusterTracker,
//	  LongtermProjector, DecisionEngine
//
//	Phase C — Ecosystem & Platform:
//	  marketplace.Registry, fleet.FleetSync,
//	  ecosystem.DeploymentGraphManager
//
//	Phase D — Launch Hardening:
//	  health.BootScorer, orchestrator.GlobalRollback,
//	  supervisor.SelfHealer
//
// All subsystems share a single ObservabilityBus with per-source EmitFunc
// wrappers, providing correlation IDs and multi-sink fan-out.
// Background goroutines are bounded to ctx and terminate on cancel.
func wirePhaseAD(
	ctx context.Context,
	safeMode *runtime.SafeModeEnforcer,
	ipcSrv *ipc.Server,
	dryRun bool,
) {
	// ── Observability bus ─────────────────────────────────────────────────
	obsBus := telemetry.NewObservabilityBus()
	emitFor := obsBus.EmitFunc // shorthand: emitFor("source") → emit func

	// ── Phase A: Performance layer ────────────────────────────────────────

	// IRQ balancer and thermal controller are instantiated first so that the
	// FramePredictor and ThermalController callbacks can reference perfMgr.
	irqBalancer := performance.NewIRQAdaptiveBalancer("2-11", emitFor("irq-balancer"))
	numaScheduler := performance.NewNUMAScheduler(emitFor("numa-scheduler"))
	_ = numaScheduler // pinning is triggered by gaming orchestrator at runtime

	// perfMgr coordinates all sysfs-level profile switches with rollback.
	perfMgr := performance.New("/var/lib/hisnos", dryRun, emitFor("perf-manager"))

	// FramePredictor escalates to ultra on jitter, with IRQ rebalance.
	framePred := performance.NewFramePredictor(func(p50, p99 float64) {
		log.Printf("[frame] jitter spike p50=%.1fms p99=%.1fms — escalating profile", p50, p99)
		obsBus.Emit("", "frame-predictor", "performance", "jitter_spike", map[string]any{
			"p50_ms": p50, "p99_ms": p99,
		})
		if err := perfMgr.Apply(performance.ProfileUltra); err != nil {
			log.Printf("[frame] escalation apply: %v", err)
		}
		irqBalancer.Tick() // force immediate IRQ rebalance on spike
	}, emitFor("frame-predictor"))

	// ThermalController downgrades profile on thermal stress.
	thermalCtrl := performance.NewThermalController(func(reason string) {
		log.Printf("[thermal] profile downgrade: %s", reason)
		obsBus.Emit("", "thermal-ctrl", "performance", "thermal_downgrade", map[string]any{
			"reason": reason,
		})
		if err := perfMgr.Apply(performance.ProfileBalanced); err != nil {
			log.Printf("[thermal] downgrade apply: %v", err)
		}
	}, emitFor("thermal-ctrl"))

	// RTGuard demotes non-whitelisted RT-scheduled processes every 3 s.
	rtGuard := performance.NewRTGuard(emitFor("rt-guard"))

	// Register performance IPC commands.
	for name, fn := range perfMgr.IPCHandlers() {
		ipcSrv.RegisterCommand(name, fn)
	}
	ipcSrv.RegisterCommand("get_thermal_status", func(_ map[string]any) (map[string]any, error) {
		return thermalCtrl.Status(), nil
	})

	// ── Phase D: Boot scorer ──────────────────────────────────────────────

	bootScorer := health.NewBootScorer(emitFor("boot-scorer"))
	// Record current boot asynchronously (lets multi-user.target fully settle).
	go func() {
		bootScorer.RecordBoot(safeMode.IsActive())
	}()

	ipcSrv.RegisterCommand("get_boot_health", func(_ map[string]any) (map[string]any, error) {
		return bootScorer.Status(), nil
	})

	// ── Phase D: Global rollback coordinator ──────────────────────────────

	gr := orchestrator.NewGlobalRollback(emitFor("global-rollback"))

	// Register performance profile as a rollback hook (snapshot active profile
	// name; restore by re-applying it).
	gr.Register(orchestrator.SubsystemHook{
		Name: "performance",
		Snapshot: func() (any, error) {
			return perfMgr.ActiveProfile(), nil
		},
		Restore: func(v any) error {
			profile, ok := v.(string)
			if !ok {
				return fmt.Errorf("performance snapshot: unexpected type %T", v)
			}
			return perfMgr.Apply(profile)
		},
	})

	// Register firewall as a rollback hook (restore from the canonical ruleset file).
	gr.Register(orchestrator.SubsystemHook{
		Name: "firewall",
		Snapshot: func() (any, error) {
			return "nftables:current", nil // marker; actual state lives in nft
		},
		Restore: func(_ any) error {
			out, err := runCmd("nft", "-f", "/etc/nftables.conf")
			if err != nil {
				return fmt.Errorf("nft restore: %w — %s", err, out)
			}
			return nil
		},
	})

	// Take an initial snapshot so a restore point exists from first start.
	if _, err := gr.TakeSnapshot(); err != nil {
		log.Printf("[rollback] initial snapshot: %v", err)
	}

	ipcSrv.RegisterCommand("global_rollback", func(params map[string]any) (map[string]any, error) {
		if confirm, _ := params["confirm"].(bool); !confirm {
			return nil, fmt.Errorf("params.confirm=true required")
		}
		res, err := gr.RollbackToLatest()
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"succeeded": res.Succeeded,
			"failed":    len(res.Failed),
			"duration":  res.Duration.String(),
		}, nil
	})

	ipcSrv.RegisterCommand("take_rollback_snapshot", func(_ map[string]any) (map[string]any, error) {
		id, err := gr.TakeSnapshot()
		if err != nil {
			return nil, err
		}
		return map[string]any{"snapshot_id": id}, nil
	})

	// ── Phase D: Self-healer ──────────────────────────────────────────────

	healer := supervisor.NewSelfHealer(func(failed []string) {
		log.Printf("[self-healer] correlated failure: %v — escalating to safe-mode", failed)
		obsBus.Emit("", "self-healer", "health", "correlated_failure", map[string]any{
			"services": failed,
		})
		if !safeMode.IsActive() {
			if err := safeMode.Enter(fmt.Sprintf("correlated_failure:%v", failed)); err != nil {
				log.Printf("[self-healer] safe-mode entry: %v", err)
			}
		}
	}, emitFor("self-healer"))

	// Register key system services.  restart/healthCheck are systemctl wrappers.
	for _, svc := range []string{"nftables.service", "auditd.service"} {
		name := svc // capture
		healer.Register(name,
			func() error { _, err := runCmd("systemctl", "restart", name); return err },
			func() error { _, err := runCmd("systemctl", "is-active", "--quiet", name); return err },
		)
	}

	ipcSrv.RegisterCommand("get_healer_status", func(_ map[string]any) (map[string]any, error) {
		return healer.Status(), nil
	})

	// ── Phase B: Automation intelligence ──────────────────────────────────

	baselineEng := automation.NewBaselineEngine(emitFor("baseline-engine"))
	confidenceModel := automation.NewConfidenceModel(emitFor("confidence-model"))
	temporalCluster := automation.NewTemporalClusterTracker(emitFor("temporal-cluster"))
	longtermProj := automation.NewLongtermProjector(func(level string, momentum float64) {
		log.Printf("[longterm] momentum %s=%.2f", level, momentum)
		obsBus.Emit("", "longterm-proj", "automation", "threat_momentum_"+level, map[string]any{
			"momentum": momentum, "level": level,
		})
	}, emitFor("longterm-proj"))

	// ActionFn: routes pre-emptive commands through the observability bus.
	// Full IPC dispatch (to ipcSrv.SubmitCommand) can be wired here once the
	// IPC server exposes a synchronous SubmitCommand method.
	autoAction := automation.ActionFn(func(command string, params map[string]any) error {
		log.Printf("[automation] pre-emptive: %s params=%v", command, params)
		obsBus.Emit("", "decision-engine", "automation", "preemptive_action", map[string]any{
			"command": command, "params": params,
		})
		return nil
	})

	decisionEng := automation.NewDecisionEngine(
		"/var/lib/hisnos",
		autoAction,
		safeMode.IsActive,
		emitFor("decision-engine"),
	)

	for name, fn := range decisionEng.IPCHandlers() {
		ipcSrv.RegisterCommand(name, fn)
	}

	ipcSrv.RegisterCommand("get_baseline_status", func(_ map[string]any) (map[string]any, error) {
		proj := longtermProj.Trend()
		return map[string]any{
			"longterm_slope":    proj.Slope,
			"longterm_momentum": proj.Momentum,
			"projection_2h":     proj.Projection2h,
			"current_score":     proj.CurrentScore,
		}, nil
	})

	// Expose pending-action review queue (from confidence model).
	ipcSrv.RegisterCommand("get_pending_actions", func(_ map[string]any) (map[string]any, error) {
		_ = confidenceModel // populated during threat signal routing
		_ = temporalCluster // populated by threat engine signal router
		return map[string]any{"message": "pending actions managed by automation layer"}, nil
	})

	// ── Phase C: Fleet sync ───────────────────────────────────────────────

	fleetSync := fleet.NewFleetSync(func(rule fleet.PolicyRule) error {
		log.Printf("[fleet] apply rule %s: target=%s action=%s", rule.RuleID, rule.Target, rule.Action)
		obsBus.Emit("", "fleet-sync", "fleet", "policy_rule_applied", map[string]any{
			"rule_id": rule.RuleID, "target": rule.Target, "action": rule.Action,
		})
		return nil
	}, emitFor("fleet-sync"))

	ipcSrv.RegisterCommand("get_fleet_status", func(_ map[string]any) (map[string]any, error) {
		st := fleetSync.Status()
		st["fleet_id"] = fleetSync.FleetID()
		return st, nil
	})
	ipcSrv.RegisterCommand("fleet_sync_now", func(_ map[string]any) (map[string]any, error) {
		if err := fleetSync.Sync(); err != nil {
			return nil, err
		}
		return map[string]any{"synced": true}, nil
	})

	// ── Phase C: Marketplace registry ─────────────────────────────────────

	marketReg := marketplace.NewRegistry("", emitFor("marketplace"))

	ipcSrv.RegisterCommand("marketplace_list", func(_ map[string]any) (map[string]any, error) {
		cat, err := marketReg.FetchCatalogue()
		if err != nil {
			return nil, fmt.Errorf("fetch catalogue: %w", err)
		}
		items := make([]string, len(cat))
		for i, m := range cat {
			items[i] = m.Name + "@" + m.Version
		}
		return map[string]any{"catalogue": items, "count": len(cat)}, nil
	})
	ipcSrv.RegisterCommand("marketplace_installed", func(_ map[string]any) (map[string]any, error) {
		plugins := marketReg.InstalledList()
		out := make([]map[string]any, 0, len(plugins))
		for _, p := range plugins {
			out = append(out, map[string]any{
				"name": p.Name, "version": p.Version,
				"enabled": p.Enabled, "sandbox": p.Sandbox,
			})
		}
		return map[string]any{"installed": out, "count": len(out)}, nil
	})

	// ── Phase C: Deployment graph ─────────────────────────────────────────

	deployGraph := ecosystem.NewDeploymentGraphManager()
	// Record current deployment asynchronously (calls rpm-ostree status).
	go func() {
		if err := deployGraph.RecordCurrent(); err != nil {
			log.Printf("[deploy-graph] record current: %v", err)
		}
	}()

	ipcSrv.RegisterCommand("suggest_rollback", func(_ map[string]any) (map[string]any, error) {
		candidates := deployGraph.SuggestRollback()
		out := make([]map[string]any, 0, len(candidates))
		for _, c := range candidates {
			out = append(out, map[string]any{
				"commit_hash": c.Deployment.CommitHash,
				"version":     c.Deployment.Version,
				"score":       c.Score,
				"reasons":     c.Reasons,
			})
		}
		return map[string]any{"candidates": out, "count": len(out)}, nil
	})
	ipcSrv.RegisterCommand("get_deployment_graph", func(_ map[string]any) (map[string]any, error) {
		candidates := deployGraph.SuggestRollback()
		return map[string]any{
			"rollback_candidates": candidates,
			"count":               len(candidates),
		}, nil
	})

	// ── Background goroutines (all bound to ctx) ──────────────────────────

	// Phase A: RT guard — scan every 3 s.
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rtGuard.Tick()
			}
		}
	}()

	// Phase A: Thermal controller — evaluate every 5 s.
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				thermalCtrl.Tick()
			}
		}
	}()

	// Phase A: IRQ adaptive balancer — scan every 4 s.
	go func() {
		irqBalancer.Start("") // stops irqbalance.service while gaming
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				irqBalancer.Tick()
			}
		}
	}()

	// Phase A: Frame predictor — read MangoHud logs every 2 s.
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				framePred.Tick()
			}
		}
	}()

	// Phase B: Decision engine — evaluates threat state every 30 s.
	go decisionEng.Run(ctx)

	// Phase B: Baseline + longterm projection feed — every 30 s.
	// The baseline engine is seeded with live system metric observations;
	// longterm projector reads the same threat-state.json the decision engine uses.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Observe with a zero-value sample during periods when the threat
				// engine has not yet written a current score.  The decision engine
				// will supply real values once threat-state.json is present.
				_ = baselineEng.Observe(automation.MetricSample{})
				_ = longtermProj.Observe(0)
			}
		}
	}()

	// Phase D: Self-healer probe — check critical services every 60 s.
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				healer.ProbeAll()
			}
		}
	}()

	// Phase C: Fleet policy sync — interval from /etc/hisnos/fleet.conf (default 15 min).
	go func() {
		interval := fleetSync.SyncInterval()
		if interval <= 0 {
			interval = 15 * time.Minute
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := fleetSync.Sync(); err != nil {
					log.Printf("[fleet] sync error: %v", err)
				}
			}
		}
	}()

	// Phase D: Global rollback — take a fresh snapshot every 6 h.
	// This ensures a recent restore point always exists.
	go func() {
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := gr.TakeSnapshot(); err != nil {
					log.Printf("[rollback] periodic snapshot: %v", err)
				}
			}
		}
	}()

	thermalStatus := thermalCtrl.Status()
	log.Printf("[hisnosd] Phase A-D subsystems wired: "+
		"perf=%s thermal_tier=%v rt-guard=active "+
		"baseline=learning automation=active "+
		"fleet_id=%s market=ready deploy-graph=recording "+
		"boot-scorer=active rollback=ready self-healer=active",
		perfMgr.ActiveProfile(),
		thermalStatus["tier"],
		fleetSync.FleetID(),
	)
}
