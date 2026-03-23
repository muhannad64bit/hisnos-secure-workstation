// core/performance/manager.go — Top-level performance profile coordinator.
//
// Manager is the single entry point for all runtime performance tuning.
// It coordinates cpu_runtime, irq_runtime, io_runtime, memory_runtime,
// scheduler_runtime, and cmdline_profile under one atomic Apply/Revert contract.
//
// Rollback contract:
//   - Snapshot all subsystems before any writes.
//   - Apply each subsystem in order; rollback ALL on first fatal error.
//   - sysfs writes have side effects (not true atomic); rollback is best-effort.
//
// IPC integration:
//   Call m.IPCHandlers() and register each returned handler with ipc.Server.RegisterCommand.
package performance

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Profile names exposed as constants for type safety.
const (
	ProfileBalanced    = "balanced"
	ProfilePerformance = "performance"
	ProfileUltra       = "ultra"
)

// profileDef holds the runtime tuning values for a named profile.
// cmdline parameters are managed separately via CmdlineProfile (require reboot).
type profileDef struct {
	Governor       string // CPU scaling governor
	Turbo          bool   // enable turbo boost
	IOScheduler    string // block device IO scheduler
	ReadAheadKB    string // read_ahead_kb ("0" = disable)
	NRRequests     string // nr_requests queue depth
	Swappiness     string // vm.swappiness
	CachePressure  string // vm.vfs_cache_pressure
	NUMABal        bool   // enable NUMA automatic balancing
	THP            string // transparent_hugepage: "never"|"madvise"|"always"
	GamingSched    bool   // apply low-latency CFS scheduler tuning
	DropCaches     bool   // drop page caches before activating (ultra only)
	RouteIRQs      bool   // redirect GPU/NIC IRQs to system cores
	IRQSystemCores string // cpulist for system cores when routing IRQs
}

var profileDefs = map[string]profileDef{
	ProfileBalanced: {
		Governor: "schedutil", Turbo: false,
		IOScheduler: "mq-deadline", ReadAheadKB: "256", NRRequests: "64",
		Swappiness: "60", CachePressure: "100",
		NUMABal: true, THP: "madvise",
		GamingSched: false, DropCaches: false, RouteIRQs: false,
	},
	ProfilePerformance: {
		Governor: "performance", Turbo: true,
		IOScheduler: "none", ReadAheadKB: "512", NRRequests: "128",
		Swappiness: "10", CachePressure: "50",
		NUMABal: false, THP: "madvise",
		GamingSched: true, DropCaches: false, RouteIRQs: false,
	},
	ProfileUltra: {
		Governor: "performance", Turbo: true,
		IOScheduler: "none", ReadAheadKB: "0", NRRequests: "256",
		Swappiness: "5", CachePressure: "10",
		NUMABal: false, THP: "never",
		GamingSched: true, DropCaches: true,
		RouteIRQs: true, IRQSystemCores: "0-1",
	},
}

// systemSnapshot bundles all subsystem snapshots for atomic rollback.
type systemSnapshot struct {
	CPU   *CPUSnapshot
	IRQ   *IRQSnapshot
	IO    *IOSnapshot
	Mem   *MemorySnapshot
	Sched *SchedulerSnapshot
}

// perfState is persisted to stateDir/perf-state.json.
type perfState struct {
	Active    string    `json:"active_profile"`
	AppliedAt time.Time `json:"applied_at"`
}

// emitFn is the function signature for emitting structured security events.
type emitFn func(category, message string, data map[string]any)

// Manager coordinates all performance runtime tuning modules.
type Manager struct {
	cpu   CPURuntime
	irq   IRQRuntime
	io    IORuntime
	mem   MemoryRuntime
	sched SchedulerRuntime
	cmd   CmdlineProfile

	stateDir string
	dryRun   bool
	emit     emitFn

	mu       sync.Mutex
	active   string
	lastSnap *systemSnapshot
}

// New creates a Manager. Previously persisted profile state is restored on construction.
//
//	stateDir: typically /var/lib/hisnos
//	dryRun:   when true, Apply logs actions but does not write to sysfs
//	emit:     callback for structured security/observability events
func New(stateDir string, dryRun bool, emit emitFn) *Manager {
	if emit == nil {
		emit = func(_, _ string, _ map[string]any) {}
	}
	m := &Manager{stateDir: stateDir, dryRun: dryRun, emit: emit, active: ProfileBalanced}
	if st, err := m.loadState(); err == nil {
		if _, ok := profileDefs[st.Active]; ok {
			m.active = st.Active
		}
	}
	return m
}

// Apply switches to the named performance profile.
// The operation is rollback-safe: if any subsystem fails fatally, all
// already-applied changes are reverted before returning the error.
func (m *Manager) Apply(profile string) error {
	def, ok := profileDefs[profile]
	if !ok {
		return fmt.Errorf("unknown profile %q (valid: balanced, performance, ultra)", profile)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	log.Printf("[perf] applying profile=%s dryRun=%v", profile, m.dryRun)

	if m.dryRun {
		m.active = profile
		m.emit("performance", "dry_run_profile_switch", map[string]any{"profile": profile})
		return m.saveState(profile)
	}

	// 1. Capture before-state for rollback.
	snap, err := m.captureSnapshot()
	if err != nil {
		return fmt.Errorf("pre-apply snapshot: %w", err)
	}

	// 2. Apply subsystems in order; rollback on first fatal failure.
	if err := m.cpu.Apply(def.Governor, def.Turbo); err != nil {
		return fmt.Errorf("cpu: %w", err)
	}
	if def.RouteIRQs {
		if err := m.irq.Apply(def.IRQSystemCores); err != nil {
			m.cpu.Restore(snap.CPU)
			return fmt.Errorf("irq: %w", err)
		}
	}
	if err := m.io.Apply(def.IOScheduler, def.ReadAheadKB, def.NRRequests); err != nil {
		m.cpu.Restore(snap.CPU)
		if def.RouteIRQs {
			m.irq.Restore(snap.IRQ)
		}
		return fmt.Errorf("io: %w", err)
	}
	if err := m.mem.Apply(def.Swappiness, def.CachePressure, def.NUMABal, def.THP); err != nil {
		m.cpu.Restore(snap.CPU)
		if def.RouteIRQs {
			m.irq.Restore(snap.IRQ)
		}
		m.io.Restore(snap.IO)
		return fmt.Errorf("mem: %w", err)
	}
	if err := m.sched.Apply(def.GamingSched); err != nil {
		m.cpu.Restore(snap.CPU)
		if def.RouteIRQs {
			m.irq.Restore(snap.IRQ)
		}
		m.io.Restore(snap.IO)
		m.mem.Restore(snap.Mem)
		return fmt.Errorf("sched: %w", err)
	}

	// 3. Optional: drop caches (non-fatal — just log on failure).
	if def.DropCaches {
		if err := m.mem.DropCaches(); err != nil {
			log.Printf("[perf] WARN: drop_caches: %v (non-fatal)", err)
		}
	}

	// 4. Commit state.
	m.lastSnap = snap
	m.active = profile
	m.emit("performance", "profile_applied", map[string]any{
		"profile":  profile,
		"dry_run":  false,
		"previous": snap,
	})

	return m.saveState(profile)
}

// Revert restores the system to the state captured before the most recent Apply call.
// Returns an error if no snapshot is available (no Apply has been called in this session).
func (m *Manager) Revert() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.lastSnap == nil {
		return fmt.Errorf("no apply-snapshot available; Apply must be called before Revert")
	}
	m.cpu.Restore(m.lastSnap.CPU)
	m.irq.Restore(m.lastSnap.IRQ)
	m.io.Restore(m.lastSnap.IO)
	m.mem.Restore(m.lastSnap.Mem)
	m.sched.Restore(m.lastSnap.Sched)
	m.active = ProfileBalanced
	m.emit("performance", "profile_reverted", nil)
	log.Printf("[perf] reverted to pre-apply snapshot")
	return m.saveState(ProfileBalanced)
}

// ActiveProfile returns the name of the currently applied runtime profile.
func (m *Manager) ActiveProfile() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

// CmdlineActiveProfile reads /proc/cmdline and returns the inferred cmdline profile.
func (m *Manager) CmdlineActiveProfile() string {
	return m.cmd.ActiveProfile()
}

// QueueCmdlineProfile stages kernel cmdline args for the next reboot.
// Returns a human-readable status message; always indicates reboot is required.
func (m *Manager) QueueCmdlineProfile(profile string) (string, error) {
	cpus, err := onlineCPUs()
	if err != nil {
		return "", fmt.Errorf("read online CPUs: %w", err)
	}
	return m.cmd.QueueProfile(profile, len(cpus))
}

// Beat is a no-op heartbeat for watchdog integration.
// The performance subsystem is stateless between profile switches.
func (m *Manager) Beat() {}

// IPCHandlers returns a map of IPC command name → handler for registration
// with ipc.Server.RegisterCommand.
func (m *Manager) IPCHandlers() map[string]func(map[string]any) (map[string]any, error) {
	return map[string]func(map[string]any) (map[string]any, error){
		"set_performance_profile": func(params map[string]any) (map[string]any, error) {
			mode, _ := params["mode"].(string)
			if mode == "" {
				return nil, fmt.Errorf("params.mode required (balanced|performance|ultra)")
			}
			if err := m.Apply(mode); err != nil {
				return nil, err
			}
			return map[string]any{"profile": mode, "applied": true, "dry_run": m.dryRun}, nil
		},
		"get_performance_profile": func(_ map[string]any) (map[string]any, error) {
			return map[string]any{
				"runtime_profile": m.ActiveProfile(),
				"cmdline_profile": m.CmdlineActiveProfile(),
				"available":       []string{ProfileBalanced, ProfilePerformance, ProfileUltra},
				"dry_run":         m.dryRun,
			}, nil
		},
		"queue_cmdline_profile": func(params map[string]any) (map[string]any, error) {
			mode, _ := params["mode"].(string)
			if mode == "" {
				return nil, fmt.Errorf("params.mode required (balanced|performance|ultra)")
			}
			msg, err := m.QueueCmdlineProfile(mode)
			if err != nil {
				return nil, err
			}
			return map[string]any{"message": msg, "reboot_required": true}, nil
		},
	}
}

// captureSnapshot takes a before-state snapshot of all subsystems.
func (m *Manager) captureSnapshot() (*systemSnapshot, error) {
	cpu, err := m.cpu.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("cpu snapshot: %w", err)
	}
	irq, _ := m.irq.Snapshot()   // non-fatal
	io, _ := m.io.Snapshot()      // non-fatal
	mem, _ := m.mem.Snapshot()    // non-fatal
	sched, _ := m.sched.Snapshot() // non-fatal
	return &systemSnapshot{CPU: cpu, IRQ: irq, IO: io, Mem: mem, Sched: sched}, nil
}

func (m *Manager) loadState() (perfState, error) {
	var st perfState
	b, err := os.ReadFile(filepath.Join(m.stateDir, "perf-state.json"))
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

func (m *Manager) saveState(profile string) error {
	st := perfState{Active: profile, AppliedAt: time.Now().UTC()}
	return writeFileAtomic(filepath.Join(m.stateDir, "perf-state.json"), mustMarshal(st))
}
