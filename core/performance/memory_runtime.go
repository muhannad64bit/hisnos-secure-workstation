// core/performance/memory_runtime.go — VM parameters, NUMA balancing, THP, and cache management.
//
// All parameters are applied via /proc/sys/vm/* and /sys/kernel/mm/*.
// DropCaches() requires CAP_SYS_ADMIN; it syncs first to avoid data loss.
package performance

import (
	"fmt"
	"log"
	"os"
	"os/exec"
)

// MemoryRuntime manages kernel VM parameters and transparent huge page settings.
type MemoryRuntime struct{}

// MemorySnapshot holds VM parameter state before a profile switch.
type MemorySnapshot struct {
	Swappiness          string
	VFSCachePressure    string
	NUMABalancing       string
	THPEnabled          string
	CompactionProact    string
	DirtyRatio          string
	DirtyBackgroundRatio string
}

// Snapshot captures current VM and memory management settings.
func (r *MemoryRuntime) Snapshot() (*MemorySnapshot, error) {
	vm := func(k string) string { v, _ := readFile("/proc/sys/vm/" + k); return v }
	thp, _ := readFile("/sys/kernel/mm/transparent_hugepage/enabled")
	numabal, _ := readFile("/proc/sys/kernel/numa_balancing")
	return &MemorySnapshot{
		Swappiness:           vm("swappiness"),
		VFSCachePressure:     vm("vfs_cache_pressure"),
		NUMABalancing:        numabal,
		THPEnabled:           thp,
		CompactionProact:     vm("compaction_proactiveness"),
		DirtyRatio:           vm("dirty_ratio"),
		DirtyBackgroundRatio: vm("dirty_background_ratio"),
	}, nil
}

// Apply sets VM parameters for the current profile.
//
//   - swappiness: 0-200 (5=ultra, 10=performance, 60=balanced)
//   - cachePressure: vfs_cache_pressure (10=ultra, 50=performance, 100=balanced)
//   - numaBalancing: true to allow kernel to rebalance memory across NUMA nodes
//   - thp: "never"|"madvise"|"always" (never=lowest latency, always=highest throughput)
func (r *MemoryRuntime) Apply(swappiness, cachePressure string, numaBalancing bool, thp string) error {
	vm := func(k, v string) {
		if err := writeFile("/proc/sys/vm/"+k, v); err != nil {
			log.Printf("[perf/mem] WARN: vm.%s=%s: %v", k, v, err)
		}
	}
	vm("swappiness", swappiness)
	vm("vfs_cache_pressure", cachePressure)
	// Reduce dirty page writeback latency spike during gaming.
	vm("dirty_ratio", "20")
	vm("dirty_background_ratio", "5")
	// Disable proactive compaction — it creates latency spikes.
	vm("compaction_proactiveness", "0")

	numaVal := "0"
	if numaBalancing {
		numaVal = "1"
	}
	if err := writeFile("/proc/sys/kernel/numa_balancing", numaVal); err != nil {
		log.Printf("[perf/mem] WARN: numa_balancing=%s: %v (non-NUMA system?)", numaVal, err)
	}

	thpPath := "/sys/kernel/mm/transparent_hugepage/enabled"
	if _, err := os.Stat(thpPath); err == nil {
		if err := writeFile(thpPath, thp); err != nil {
			log.Printf("[perf/mem] WARN: THP=%s: %v", thp, err)
		}
	}
	return nil
}

// DropCaches drops the page cache, dentries, and inodes from RAM.
// Called pre-gaming in ultra mode to free memory for the game process.
// Always syncs buffered writes first to avoid data loss.
// Requires CAP_SYS_ADMIN.
func (r *MemoryRuntime) DropCaches() error {
	// Flush all dirty pages to disk before dropping caches.
	if out, err := exec.Command("sync").CombinedOutput(); err != nil {
		return fmt.Errorf("sync before drop_caches: %v: %s", err, out)
	}
	// 3 = drop pagecache + dentries + inodes.
	if err := writeFile("/proc/sys/vm/drop_caches", "3"); err != nil {
		return fmt.Errorf("drop_caches: %w", err)
	}
	log.Printf("[perf/mem] page caches dropped (ultra profile)")
	return nil
}

// Restore resets VM parameters to the snapshotted values.
func (r *MemoryRuntime) Restore(snap *MemorySnapshot) {
	if snap == nil {
		return
	}
	vm := func(k, v string) {
		if v != "" {
			_ = writeFile("/proc/sys/vm/"+k, v)
		}
	}
	vm("swappiness", snap.Swappiness)
	vm("vfs_cache_pressure", snap.VFSCachePressure)
	vm("dirty_ratio", snap.DirtyRatio)
	vm("dirty_background_ratio", snap.DirtyBackgroundRatio)
	vm("compaction_proactiveness", snap.CompactionProact)
	if snap.NUMABalancing != "" {
		_ = writeFile("/proc/sys/kernel/numa_balancing", snap.NUMABalancing)
	}
	if snap.THPEnabled != "" {
		_ = writeFile("/sys/kernel/mm/transparent_hugepage/enabled", snap.THPEnabled)
	}
}
