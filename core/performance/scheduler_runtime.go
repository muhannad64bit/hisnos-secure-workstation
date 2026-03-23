// core/performance/scheduler_runtime.go — Kernel CFS scheduler parameter tuning.
//
// Reduces scheduling latency for interactive gaming workloads by narrowing
// the CFS scheduling granularity and wakeup thresholds.
// All parameters are available at runtime via /proc/sys/kernel/sched_*.
package performance

import (
	"log"
)

// SchedulerRuntime manages kernel CFS scheduler sysctl parameters.
type SchedulerRuntime struct{}

// SchedulerSnapshot holds scheduler parameter state before a profile switch.
type SchedulerSnapshot struct {
	Params map[string]string // sysctl key (without path prefix) → value
}

// managedParams enumerates the scheduler sysctl keys we control.
// Not all kernels expose all keys; missing ones are silently skipped.
var managedParams = []string{
	"sched_latency_ns",
	"sched_min_granularity_ns",
	"sched_wakeup_granularity_ns",
	"sched_migration_cost_ns",
	"sched_autogroup_enabled",
}

// Snapshot captures current scheduler parameters.
func (r *SchedulerRuntime) Snapshot() (*SchedulerSnapshot, error) {
	snap := &SchedulerSnapshot{Params: make(map[string]string, len(managedParams))}
	for _, key := range managedParams {
		v, err := readFile("/proc/sys/kernel/" + key)
		if err != nil {
			continue // kernel does not expose this param
		}
		snap.Params[key] = v
	}
	return snap, nil
}

// Apply tunes the CFS scheduler for gaming (low latency) or standard use (throughput).
//
// gaming=true applies a low-latency profile:
//   - Narrowed scheduling windows reduce jitter for frame-sensitive workloads.
//   - Autogroup enabled so games run in their own scheduling group.
//
// gaming=false restores Fedora upstream defaults (throughput-oriented).
func (r *SchedulerRuntime) Apply(gaming bool) error {
	set := func(key, value string) {
		if err := writeFile("/proc/sys/kernel/"+key, value); err != nil {
			log.Printf("[perf/sched] WARN: %s=%s: %v", key, value, err)
		}
	}
	if gaming {
		// Latency-tuned: tighter granularity, faster preemption response.
		// Default Fedora values: latency=6ms, min_gran=0.75ms, wakeup=1ms.
		set("sched_latency_ns", "4000000")       // 4ms (from 6ms)
		set("sched_min_granularity_ns", "500000") // 0.5ms (from 0.75ms)
		set("sched_wakeup_granularity_ns", "1000000")
		set("sched_migration_cost_ns", "250000") // 0.25ms (from 0.5ms)
		set("sched_autogroup_enabled", "1")
	} else {
		// Throughput-optimised: restore Fedora upstream defaults.
		set("sched_latency_ns", "6000000")
		set("sched_min_granularity_ns", "750000")
		set("sched_wakeup_granularity_ns", "1000000")
		set("sched_migration_cost_ns", "500000")
		set("sched_autogroup_enabled", "1")
	}
	return nil
}

// Restore resets scheduler parameters to the snapshotted values.
func (r *SchedulerRuntime) Restore(snap *SchedulerSnapshot) {
	if snap == nil {
		return
	}
	for key, value := range snap.Params {
		if err := writeFile("/proc/sys/kernel/"+key, value); err != nil {
			log.Printf("[perf/sched] WARN: restore %s=%s: %v", key, value, err)
		}
	}
}
