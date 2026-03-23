// gaming/hispowerd/main.go — HisnOS Gaming Performance Runtime
//
// hispowerd is the production gaming performance daemon for HisnOS.
// It transforms the workstation into a high-FPS low-latency gaming environment
// without relaxing the security model — it dynamically adapts subsystem behavior.
//
// Runtime lifecycle:
//   1. Load config → initialize all subsystem controllers
//   2. Scan /proc every ScanIntervalSeconds for gaming workloads (Phase 1)
//   3. On gaming_active=true: apply all 6 performance phases atomically
//   4. On gaming_active=false: restore all subsystems in reverse order
//   5. On SIGTERM/SIGINT: force stopGaming(), then exit
//   6. On panic/crash: deferred safetyNet() restores all subsystems
//
// State machine (Phase 7):
//   normal → gaming-performance    (startGaming)
//   gaming-performance → normal    (stopGaming)
//   any → normal                   (safetyNet, crash recovery)
//
// Safety guarantees (Phase 10):
//   - Never leaves cores isolated after crash (BroadReset in safetyNet)
//   - Never leaves IRQ masks modified (EmergencyRestore in safetyNet)
//   - Never leaves nftables fast path loaded (EmergencyRestore in safetyNet)
//   - Blocks if control plane is in update-preparing or safe-mode
//   - Never stops vault watcher (only vault idle timer is paused)
//   - Never touches lab-active isolation

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

	"hisnos.local/hispowerd/config"
	"hisnos.local/hispowerd/cpu"
	"hisnos.local/hispowerd/detect"
	"hisnos.local/hispowerd/firewall"
	"hisnos.local/hispowerd/irq"
	"hisnos.local/hispowerd/observe"
	"hisnos.local/hispowerd/state"
	"hisnos.local/hispowerd/throttle"
	"hisnos.local/hispowerd/tuning"
)

// applied tracks which phases were successfully activated this session.
// Used to ensure we only roll back what was actually applied.
type applied struct {
	cpuIsolation bool
	irqAffinity  bool
	throttled    bool
	firewallFast bool
	tuned        bool
	modeChanged  bool
}

// Daemon is the top-level runtime controller.
type Daemon struct {
	cfg       *config.Config
	log       *observe.Logger
	stateMgr  *state.Manager
	detector  *detect.Detector
	isolator  *cpu.Isolator
	irqOpt    *irq.Optimizer
	throttler *throttle.Throttler
	fastPath  *firewall.FastPath
	tuner     *tuning.Tuner

	gaming   bool    // current gaming state
	phases   applied // which phases are active
	session  detect.Session
}

func main() {
	log.SetPrefix("[hispowerd] ")
	log.SetFlags(log.Ltime | log.Lshortfile)

	cfgPath := envOr("HISPOWERD_CONFIG", config.DefaultConfigPath)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Printf("config load error (%s): %v — using defaults", cfgPath, err)
	}

	obs := observe.New()
	obs.Info("hispowerd starting (scan_interval=%ds)", cfg.ScanIntervalSeconds)

	uid := os.Getenv("UID")
	if uid == "" {
		uid = strconv.Itoa(os.Getuid())
	}
	// Resolve hisnosd socket with actual UID if needed.
	if cfg.HisnosdSocket == "" {
		cfg.HisnosdSocket = fmt.Sprintf("/run/user/%s/hisnosd.sock", uid)
	}

	stateMgr := state.NewManager(cfg.GamingStateFile, cfg.ControlPlaneStateFile, cfg.HisnosdSocket)
	stateMgr.Load() // warm-load previous state (non-fatal)

	d := &Daemon{
		cfg:       cfg,
		log:       obs,
		stateMgr:  stateMgr,
		detector:  detect.NewDetector(cfg, obs),
		isolator:  cpu.NewIsolator(cfg, obs),
		irqOpt:    irq.NewOptimizer(cfg, obs),
		throttler: throttle.NewThrottler(cfg, obs),
		fastPath:  firewall.NewFastPath(cfg, obs),
		tuner:     tuning.NewTuner(cfg, obs),
	}

	// Phase 10 Safety: register crash cleanup BEFORE any subsystem changes.
	defer d.safetyNet()

	// Ensure any leftover gaming state from a previous crash is cleared.
	d.ensureCleanStart()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Signal handling.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sig
		obs.Info("received %v — initiating shutdown", s)
		cancel()
	}()

	d.run(ctx)
	obs.Info("hispowerd stopped cleanly")
}

// run is the main scan-and-orchestrate loop.
func (d *Daemon) run(ctx context.Context) {
	interval := time.Duration(d.cfg.ScanIntervalSeconds) * time.Second
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			if d.gaming {
				d.stopGaming()
			}
			return
		case <-tick.C:
			d.cycle()
		}
	}
}

// cycle runs one detection + orchestration cycle.
func (d *Daemon) cycle() {
	// Phase 10 Safety: never apply gaming mode during update or safe-mode.
	if blocked, reason := d.isBlockedByControlPlane(); blocked {
		if d.gaming {
			d.log.Info("main: control plane state blocks gaming — stopping: %s", reason)
			d.stopGaming()
		}
		return
	}

	session := d.detector.Detect()

	if session.Active && !d.gaming {
		d.log.Info("main: gaming session detected: pid=%d type=%s name=%s",
			session.PID, session.SessionType, session.Name)
		d.startGaming(session)
	} else if !session.Active && d.gaming {
		d.log.Info("main: gaming session ended")
		d.stopGaming()
	}
}

// startGaming applies all performance phases in order.
// Each phase is independent: failure in one does not abort the others.
func (d *Daemon) startGaming(session detect.Session) {
	d.log.Info("main: entering gaming-performance mode")
	d.phases = applied{}
	d.session = session

	// Collect game PIDs (parent + all descendants).
	gamePIDs := detect.FindGameProcesses(session.PID)
	d.log.Info("main: game process tree: %d PID(s)", len(gamePIDs))

	// Phase 2: CPU isolation.
	if err := d.isolator.Apply(gamePIDs); err != nil {
		d.log.Warn("main: CPU isolation partial: %v", err)
	} else {
		d.phases.cpuIsolation = true
		d.log.Emit(observe.EventCPUIsolationApplied,
			fmt.Sprintf("CPU isolation applied: game_pids=%d gaming_cores=%v system_cores=%v",
				len(gamePIDs), d.cfg.GamingCores, d.cfg.SystemCores),
			map[string]string{
				"HISNOS_GAME_PID":     strconv.Itoa(session.PID),
				"HISNOS_SESSION_TYPE": session.SessionType,
			})
	}

	// Phase 3: IRQ affinity.
	if err := d.irqOpt.Apply(); err != nil {
		d.log.Warn("main: IRQ tuning: %v", err)
	} else {
		d.phases.irqAffinity = true
		d.log.Emit(observe.EventIRQTuned, "IRQ affinity pinned to gaming cores",
			map[string]string{"HISNOS_GAME_PID": strconv.Itoa(session.PID)})
	}

	// Phase 4: daemon throttle.
	if err := d.throttler.Apply(); err != nil {
		d.log.Warn("main: daemon throttle partial: %v", err)
	} else {
		d.phases.throttled = true
		d.log.Emit(observe.EventDaemonsThrottled, "Non-gaming daemons throttled",
			map[string]string{"HISNOS_GAME_PID": strconv.Itoa(session.PID)})
	}

	// Phase 5: firewall fast path.
	if err := d.fastPath.Apply(); err != nil {
		d.log.Warn("main: firewall fast path: %v", err)
	} else {
		d.phases.firewallFast = true
		d.log.Emit(observe.EventFirewallFastPathOn,
			"Gaming firewall fast path loaded",
			map[string]string{"HISNOS_NFT_FILE": d.cfg.FastNFTFile})
	}

	// Phase 6: GPU + scheduler tuning.
	if err := d.tuner.Apply(gamePIDs); err != nil {
		d.log.Warn("main: tuning: %v", err)
	} else {
		d.phases.tuned = true
	}

	// Phase 7: control plane mode transition.
	if err := d.stateMgr.SetControlPlaneMode("gaming-performance"); err != nil {
		d.log.Warn("main: mode transition to gaming-performance: %v", err)
	} else {
		d.phases.modeChanged = true
	}

	// Persist gaming state.
	startTS := session.DetectedAt.Format(time.RFC3339)
	if startTS == (time.Time{}).Format(time.RFC3339) {
		startTS = time.Now().UTC().Format(time.RFC3339)
	}
	_ = d.stateMgr.Update(func(s *state.GamingState) {
		s.GamingActive = true
		s.GamePID = session.PID
		s.GameName = session.Name
		s.StartTimestamp = startTS
		s.SessionType = session.SessionType
		s.CPUIsolationApplied = d.phases.cpuIsolation
		s.IRQTuned = d.phases.irqAffinity
		s.FirewallFastPath = d.phases.firewallFast
		s.DaemonsThrottled = d.phases.throttled
		if d.phases.tuned {
			s.GovernorSet = d.cfg.CPUGovernor
		}
	})

	d.gaming = true

	// Phase 8: GAMING_START event.
	d.log.Emit(observe.EventGamingStart,
		fmt.Sprintf("Gaming session started: %s (pid=%d, type=%s)",
			session.Name, session.PID, session.SessionType),
		map[string]string{
			"HISNOS_GAME_NAME":       session.Name,
			"HISNOS_GAME_PID":        strconv.Itoa(session.PID),
			"HISNOS_SESSION_TYPE":    session.SessionType,
			"HISNOS_CPU_ISOLATED":    boolStr(d.phases.cpuIsolation),
			"HISNOS_IRQ_TUNED":       boolStr(d.phases.irqAffinity),
			"HISNOS_FIREWALL_FAST":   boolStr(d.phases.firewallFast),
			"HISNOS_DAEMONS_THROTTLED": boolStr(d.phases.throttled),
			"HISNOS_TUNING":          d.tuner.AppliedSummary(),
		})
}

// stopGaming restores all subsystems in reverse phase order.
// Every step is attempted regardless of prior errors.
func (d *Daemon) stopGaming() {
	if !d.gaming {
		return
	}

	d.log.Info("main: exiting gaming-performance mode")

	// Phase 6: restore tuning.
	gamePIDs := detect.FindGameProcesses(d.session.PID)
	if d.phases.tuned {
		if err := d.tuner.Restore(gamePIDs); err != nil {
			d.log.Warn("main: tuning restore: %v", err)
		}
		d.log.Emit(observe.EventGovernorRestored, "CPU governor restored",
			map[string]string{"HISNOS_GAME_PID": strconv.Itoa(d.session.PID)})
	}

	// Phase 5: remove firewall fast path.
	if d.phases.firewallFast {
		if err := d.fastPath.Restore(); err != nil {
			d.log.Warn("main: firewall restore: %v", err)
		}
		d.log.Emit(observe.EventFirewallFastPathOff, "Gaming firewall fast path removed", nil)
		// Verify base policy is intact (Phase 10 safety).
		if err := d.fastPath.VerifyBasePolicy(); err != nil {
			d.log.EmitWarning(observe.EventFirewallFastPathOff,
				"CRITICAL: "+err.Error(), nil)
		}
	}

	// Phase 4: restore daemon throttle.
	if d.phases.throttled {
		if err := d.throttler.Restore(); err != nil {
			d.log.Warn("main: throttle restore: %v", err)
		}
		d.log.Emit(observe.EventDaemonsRestored, "Daemon throttles removed", nil)
	}

	// Phase 3: restore IRQ affinity.
	if d.phases.irqAffinity {
		if err := d.irqOpt.Restore(); err != nil {
			d.log.Warn("main: IRQ restore: %v", err)
			d.irqOpt.EmergencyRestore()
		}
		d.log.Emit(observe.EventIRQRestored, "IRQ affinity restored to all CPUs", nil)
	}

	// Phase 2: restore CPU isolation.
	if d.phases.cpuIsolation {
		if err := d.isolator.Restore(); err != nil {
			d.log.Warn("main: CPU affinity restore: %v", err)
			d.log.Warn("main: falling back to broad CPU affinity reset")
			d.isolator.BroadReset()
		}
		d.log.Emit(observe.EventCPUIsolationRestored, "CPU isolation removed", nil)
	}

	// Phase 7: restore control plane mode.
	if d.phases.modeChanged {
		if err := d.stateMgr.SetControlPlaneMode("normal"); err != nil {
			d.log.Warn("main: mode transition to normal: %v", err)
		}
	}

	// Clear gaming state.
	_ = d.stateMgr.ClearGamingState()

	d.gaming = false
	d.phases = applied{}

	// Phase 8: GAMING_STOP event.
	d.log.Emit(observe.EventGamingStop,
		fmt.Sprintf("Gaming session ended: %s (pid=%d)", d.session.Name, d.session.PID),
		map[string]string{
			"HISNOS_GAME_NAME": d.session.Name,
			"HISNOS_GAME_PID":  strconv.Itoa(d.session.PID),
		})

	d.session = detect.Session{}
}

// safetyNet is deferred in main(). Called on panic, SIGTERM, or normal exit.
// It performs a best-effort cleanup of all subsystems.
// Phase 10: never leave system in degraded state.
func (d *Daemon) safetyNet() {
	if r := recover(); r != nil {
		d.log.Warn("main: PANIC recovered: %v — running safety net", r)
		d.log.Emit(observe.EventCrashRecovery,
			fmt.Sprintf("hispowerd panic recovery: %v", r), nil)
	}

	if !d.gaming {
		return
	}

	d.log.Info("main: safety net: restoring all subsystems after unclean exit")

	// Broad CPU reset — unconditionally allow all processes on all CPUs.
	d.isolator.BroadReset()

	// IRQ emergency restore — write all-CPUs mask.
	d.irqOpt.EmergencyRestore()

	// Firewall emergency restore — remove gaming table.
	d.fastPath.EmergencyRestore()

	// Throttle restore (best effort).
	_ = d.throttler.Restore()

	// Tuning restore (best effort).
	_ = d.tuner.Restore(nil)

	// Transition mode → normal.
	_ = d.stateMgr.SetControlPlaneMode("normal")
	_ = d.stateMgr.ClearGamingState()

	d.log.Emit(observe.EventCrashRecovery, "hispowerd safety net complete — system restored", nil)
}

// ensureCleanStart detects a leftover gaming state from a previous crash
// and applies safetyNet cleanup before the first detection cycle.
func (d *Daemon) ensureCleanStart() {
	prev := d.stateMgr.Get()
	if !prev.GamingActive {
		return
	}
	d.log.Warn("main: detected leftover gaming state from previous run — running cleanup")
	d.log.Emit(observe.EventCrashRecovery,
		"hispowerd startup: clearing stale gaming state from previous session", nil)

	// Best-effort cleanup of whatever was left active.
	d.isolator.BroadReset()
	d.irqOpt.EmergencyRestore()
	d.fastPath.EmergencyRestore()
	_ = d.throttler.Restore()
	_ = d.tuner.Restore(nil)
	_ = d.stateMgr.SetControlPlaneMode("normal")
	_ = d.stateMgr.ClearGamingState()
}

// isBlockedByControlPlane reads core-state.json mode and returns true
// if gaming is not permitted (update-preparing, rollback, safe-mode).
// Phase 10 safety: never interfere with update or vault lock operations.
func (d *Daemon) isBlockedByControlPlane() (bool, string) {
	data, err := os.ReadFile(d.cfg.ControlPlaneStateFile)
	if err != nil {
		return false, "" // file missing = no restriction
	}

	// Quick substring check without full parse.
	s := string(data)
	blockedModes := []string{
		`"update-preparing"`,
		`"update_preparing"`,
		`"rollback"`,
		`"rollback-mode"`,
		`"safe-mode"`,
		`"safe_mode"`,
	}
	for _, mode := range blockedModes {
		if containsString(s, mode) {
			reason := "control plane mode: " + mode
			return true, reason
		}
	}
	return false, ""
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func (d *Daemon) log_Emit(event, message string, extra map[string]string) {
	d.log.Emit(event, message, extra)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func containsString(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub ||
		len(s) > 0 && containsSubstring(s, sub))
}

func containsSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// stateFilePath resolves a path relative to the binary's config dir.
func stateFilePath(name string) string {
	return filepath.Join("/var/lib/hisnos", name)
}
