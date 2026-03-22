// cpu/isolator.go — Phase 2: CPU Isolation Orchestrator
//
// Moves game processes to gaming cores and system daemons to system cores
// via sched_setaffinity (SYS_SCHED_SETAFFINITY / SYS_SCHED_GETAFFINITY).
//
// sched_setaffinity on processes of the same UID does NOT require extra privileges.
// No root needed for this phase.
//
// Core layout (default, configurable):
//   cores 0-1  → system daemons (hisnos services, kernel threads)
//   cores 2-7  → game processes
//
// Rollback: previous affinity masks are saved in memory before any change.
// If the daemon crashes, hisnos-hispowerd-recover.sh restores affinities
// by calling taskset -cp 0-<max> on all processes (broad reset).

package cpu

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"hisnos.local/hispowerd/config"
	"hisnos.local/hispowerd/observe"
)

// cpuSet matches the kernel's cpu_set_t (128 bytes = 1024 CPUs).
type cpuSet [16]uint64

func (cs *cpuSet) set(cpu uint) {
	if cpu < 1024 {
		cs[cpu/64] |= 1 << (cpu % 64)
	}
}

// Saved tracks the pre-gaming affinity of a single PID.
type saved struct {
	pid      int
	previous uint64 // cs[0] from getAffinity
}

// Isolator implements CPU affinity management.
type Isolator struct {
	cfg    *config.Config
	log    *observe.Logger
	mu     sync.Mutex
	saved  []saved // process affinities saved before gaming started
}

// NewIsolator creates an Isolator.
func NewIsolator(cfg *config.Config, log *observe.Logger) *Isolator {
	return &Isolator{cfg: cfg, log: log}
}

// Apply moves game processes to gaming cores and daemons to system cores.
// gamePIDs is the list of game process PIDs (parent + descendants).
// Returns error only if ALL operations fail; partial success is not an error.
func (iso *Isolator) Apply(gamePIDs []int) error {
	iso.mu.Lock()
	defer iso.mu.Unlock()

	iso.saved = iso.saved[:0] // reset
	gameMask := iso.cfg.GamingCoreMask()
	sysMask := iso.cfg.SystemCoreMask()
	var lastErr error

	// 1. Move game processes to gaming cores.
	for _, pid := range gamePIDs {
		if err := iso.saveAndSet(pid, gameMask); err != nil {
			iso.log.Warn("cpu: set game affinity pid=%d: %v", pid, err)
			lastErr = err
		} else {
			iso.log.Info("cpu: game pid=%d → cores %s", pid, maskStr(gameMask))
		}
	}

	// 2. Move managed daemons to system cores.
	for _, svc := range iso.cfg.ManagedDaemons {
		pids := findPIDsByService(svc)
		if len(pids) == 0 {
			iso.log.Info("cpu: no PIDs found for %s — skipping", svc)
			continue
		}
		for _, pid := range pids {
			if err := iso.saveAndSet(pid, sysMask); err != nil {
				iso.log.Warn("cpu: set daemon affinity pid=%d (%s): %v", pid, svc, err)
				lastErr = err
			} else {
				iso.log.Info("cpu: daemon pid=%d (%s) → cores %s", pid, svc, maskStr(sysMask))
			}
		}
	}

	return lastErr
}

// Restore reinstates all saved affinities. Called on session end and crash recovery.
// Always attempts all restores regardless of individual errors.
func (iso *Isolator) Restore() error {
	iso.mu.Lock()
	defer iso.mu.Unlock()

	var lastErr error
	for _, s := range iso.saved {
		if err := setAffinity(s.pid, s.previous); err != nil {
			// Process may have exited — not a fatal error.
			iso.log.Info("cpu: restore pid=%d: %v (may have exited)", s.pid, err)
			lastErr = err
		} else {
			iso.log.Info("cpu: restored pid=%d → mask %016x", s.pid, s.previous)
		}
	}
	iso.saved = iso.saved[:0]
	return lastErr
}

// BroadReset sets all known processes to a full-CPU mask (crash recovery).
// Uses all-ones mask = allow all CPUs.
func (iso *Isolator) BroadReset() {
	iso.mu.Lock()
	defer iso.mu.Unlock()

	allMask := ^uint64(0) // all 64 bits set
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		_ = setAffinity(pid, allMask)
	}
	iso.saved = iso.saved[:0]
	iso.log.Info("cpu: broad reset applied — all processes allowed on all CPUs")
}

// saveAndSet saves current affinity and applies new mask.
func (iso *Isolator) saveAndSet(pid int, newMask uint64) error {
	prev, err := getAffinity(pid)
	if err != nil {
		return fmt.Errorf("getaffinity: %w", err)
	}
	if err := setAffinity(pid, newMask); err != nil {
		return fmt.Errorf("setaffinity: %w", err)
	}
	iso.saved = append(iso.saved, saved{pid: pid, previous: prev})
	return nil
}

// ─── syscall wrappers ────────────────────────────────────────────────────────

func setAffinity(pid int, mask uint64) error {
	var cs cpuSet
	cs[0] = mask
	_, _, errno := syscall.RawSyscall(
		syscall.SYS_SCHED_SETAFFINITY,
		uintptr(pid),
		uintptr(unsafe.Sizeof(cs)),
		uintptr(unsafe.Pointer(&cs)),
	)
	if errno != 0 {
		return errno
	}
	return nil
}

func getAffinity(pid int) (uint64, error) {
	var cs cpuSet
	_, _, errno := syscall.RawSyscall(
		syscall.SYS_SCHED_GETAFFINITY,
		uintptr(pid),
		uintptr(unsafe.Sizeof(cs)),
		uintptr(unsafe.Pointer(&cs)),
	)
	if errno != 0 {
		return 0, errno
	}
	return cs[0], nil
}

// ─── /proc/cgroup-based PID discovery ───────────────────────────────────────

// findPIDsByService finds PIDs whose cgroup path ends with the given service name.
// Uses /proc/<pid>/cgroup (cgroup v2 unified hierarchy line: "0::/.../<service>").
func findPIDsByService(serviceName string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}

	var pids []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if matchesCgroup(pid, serviceName) {
			pids = append(pids, pid)
		}
	}
	return pids
}

func matchesCgroup(pid int, serviceName string) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return false
	}
	// cgroup v2 line: "0::/user.slice/.../hisnos-threatd.service"
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			cgPath := strings.TrimPrefix(line, "0::")
			if strings.HasSuffix(strings.TrimSpace(cgPath), "/"+serviceName) {
				return true
			}
		}
	}
	return false
}

// maskStr formats a CPU mask as a compact core range string.
func maskStr(mask uint64) string {
	var cores []string
	for i := 0; i < 64; i++ {
		if mask&(1<<uint(i)) != 0 {
			cores = append(cores, strconv.Itoa(i))
		}
	}
	if len(cores) == 0 {
		return "none"
	}
	return strings.Join(cores, ",")
}
