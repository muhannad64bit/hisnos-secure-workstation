// core/performance/cpu_runtime.go — CPU governor, turbo boost, and core affinity.
//
// All changes are applied via /sys/devices/system/cpu — no kernel recompilation.
// Supports both Intel pstate and AMD cpufreq boost interfaces.
package performance

import (
	"fmt"
	"log"
	"os"
)

// CPURuntime manages CPU frequency scaling governor and turbo boost state.
type CPURuntime struct{}

// CPUSnapshot captures per-CPU governor and turbo state before a profile switch.
type CPUSnapshot struct {
	Governors    map[int]string // cpu index → scaling_governor
	TurboEnabled bool
}

// Snapshot captures the current CPU governor and turbo state for rollback.
func (r *CPURuntime) Snapshot() (*CPUSnapshot, error) {
	cpus, err := onlineCPUs()
	if err != nil {
		return nil, err
	}
	snap := &CPUSnapshot{Governors: make(map[int]string, len(cpus))}
	for _, cpu := range cpus {
		gov, err := readFile(fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/scaling_governor", cpu))
		if err != nil {
			// Some CPUs lack cpufreq (hybrid architectures, VMs) — skip silently.
			continue
		}
		snap.Governors[cpu] = gov
	}
	snap.TurboEnabled = currentTurboEnabled()
	return snap, nil
}

// Apply sets the governor and turbo state for all online CPUs.
// governor: "performance" | "schedutil" | "powersave" | "ondemand"
// turbo: true to enable boost, false to disable.
func (r *CPURuntime) Apply(governor string, turbo bool) error {
	cpus, err := onlineCPUs()
	if err != nil {
		return err
	}
	for _, cpu := range cpus {
		path := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/scaling_governor", cpu)
		if err := writeFile(path, governor); err != nil {
			// Hybrid CPUs (e-cores) may share governors — log, don't fail.
			log.Printf("[perf/cpu] WARN: set governor cpu%d: %v", cpu, err)
		}
	}
	if err := applyTurbo(turbo); err != nil {
		log.Printf("[perf/cpu] WARN: turbo=%v: %v (non-fatal)", turbo, err)
	}
	log.Printf("[perf/cpu] governor=%s turbo=%v applied to %d CPUs", governor, turbo, len(cpus))
	return nil
}

// Restore resets CPUs to the snapshotted governor and turbo state.
func (r *CPURuntime) Restore(snap *CPUSnapshot) {
	if snap == nil {
		return
	}
	for cpu, gov := range snap.Governors {
		path := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/scaling_governor", cpu)
		if err := writeFile(path, gov); err != nil {
			log.Printf("[perf/cpu] WARN: restore cpu%d governor: %v", cpu, err)
		}
	}
	if err := applyTurbo(snap.TurboEnabled); err != nil {
		log.Printf("[perf/cpu] WARN: restore turbo: %v", err)
	}
}

// applyTurbo enables or disables CPU turbo boost.
// Supports Intel pstate (no_turbo) and AMD cpufreq/boost interfaces.
func applyTurbo(enable bool) error {
	// Intel pstate: no_turbo=0 → turbo ON; no_turbo=1 → turbo OFF.
	intelPath := "/sys/devices/system/cpu/intel_pstate/no_turbo"
	if _, err := os.Stat(intelPath); err == nil {
		val := "0"
		if !enable {
			val = "1"
		}
		return writeFile(intelPath, val)
	}
	// AMD cpufreq: boost=1 → turbo ON; boost=0 → turbo OFF.
	amdPath := "/sys/devices/system/cpu/cpufreq/boost"
	if _, err := os.Stat(amdPath); err == nil {
		val := "1"
		if !enable {
			val = "0"
		}
		return writeFile(amdPath, val)
	}
	// Not found — common in VMs and systems without boost; not an error.
	return nil
}

// currentTurboEnabled returns whether CPU turbo boost is currently active.
func currentTurboEnabled() bool {
	// Intel: no_turbo=0 → turbo is ON.
	if v, err := readFile("/sys/devices/system/cpu/intel_pstate/no_turbo"); err == nil {
		return v == "0"
	}
	// AMD: boost=1 → turbo is ON.
	if v, err := readFile("/sys/devices/system/cpu/cpufreq/boost"); err == nil {
		return v == "1"
	}
	return false
}
