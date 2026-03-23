// core/performance/numa_scheduler.go — NUMA-aware game thread affinity scheduler.
//
// Detects NUMA topology and the GPU's preferred memory node, then pins
// registered game PIDs to the local NUMA node using taskset(1).
// Eliminates cross-node memory access penalties (~30–100 ns per access).
//
// NUMA topology discovery:
//   /sys/devices/system/node/nodeN/cpulist — CPUs local to node N
//   /sys/bus/pci/devices/*/class            — filter VGA(0x030000)+3D(0x030200)
//   /sys/bus/pci/devices/*/numa_node        — preferred node for that GPU
//
// On exit (or profile revert) the original affinity (all CPUs) is restored.
package performance

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// numaNode describes one NUMA memory domain.
type numaNode struct {
	ID      int
	CPUs    []int
	CPUList string // kernel cpulist notation (e.g. "0-7")
}

// NUMAScheduler pins game processes to the GPU-local NUMA node.
type NUMAScheduler struct {
	mu        sync.Mutex
	topology  []numaNode
	gpuNodeID int   // NUMA node closest to the primary GPU (-1 = unknown/UMA)
	pinned    []int // PIDs currently pinned

	emit func(category, msg string, data map[string]any)
}

// NewNUMAScheduler creates a scheduler and discovers topology immediately.
func NewNUMAScheduler(emit func(string, string, map[string]any)) *NUMAScheduler {
	ns := &NUMAScheduler{gpuNodeID: -1, emit: emit}
	if emit == nil {
		ns.emit = func(_, _ string, _ map[string]any) {}
	}
	if err := ns.discover(); err != nil {
		log.Printf("[numa] topology discovery failed: %v (UMA system or no NUMA support)", err)
	}
	return ns
}

// IsNUMA returns true if the system has more than one NUMA node.
func (ns *NUMAScheduler) IsNUMA() bool {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return len(ns.topology) > 1
}

// PinPIDs pins a slice of PIDs to the GPU-local NUMA node.
// Falls back silently on UMA systems.
func (ns *NUMAScheduler) PinPIDs(pids []int) {
	ns.mu.Lock()
	defer ns.mu.Unlock()

	if ns.gpuNodeID < 0 || len(ns.topology) < 2 {
		return // UMA or GPU node unknown — nothing to pin
	}

	var node *numaNode
	for i := range ns.topology {
		if ns.topology[i].ID == ns.gpuNodeID {
			node = &ns.topology[i]
			break
		}
	}
	if node == nil {
		return
	}

	for _, pid := range pids {
		if err := ns.pinPID(pid, node.CPUList); err != nil {
			log.Printf("[numa] pin pid=%d to node%d (%s): %v", pid, node.ID, node.CPUList, err)
			continue
		}
		ns.pinned = append(ns.pinned, pid)
		log.Printf("[numa] pinned pid=%d to node%d CPUs [%s]", pid, node.ID, node.CPUList)
	}

	if len(pids) > 0 {
		ns.emit("performance", "numa_pins_applied", map[string]any{
			"gpu_node": ns.gpuNodeID, "pids": pids, "cpu_list": node.CPUList,
		})
	}
}

// RestoreAll removes affinity pinning from all tracked PIDs (allows all CPUs).
func (ns *NUMAScheduler) RestoreAll() {
	ns.mu.Lock()
	defer ns.mu.Unlock()

	cpus, err := onlineCPUs()
	if err != nil || len(cpus) == 0 {
		return
	}
	allCPUs := formatCPUList(cpus)
	for _, pid := range ns.pinned {
		_ = ns.pinPID(pid, allCPUs)
	}
	log.Printf("[numa] restored affinity for %d PIDs", len(ns.pinned))
	ns.pinned = nil
}

// Status returns a summary for IPC/observability.
func (ns *NUMAScheduler) Status() map[string]any {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	nodes := make([]map[string]any, 0, len(ns.topology))
	for _, n := range ns.topology {
		nodes = append(nodes, map[string]any{"id": n.ID, "cpu_list": n.CPUList})
	}
	return map[string]any{
		"numa_nodes":    nodes,
		"gpu_node":      ns.gpuNodeID,
		"pinned_pids":   ns.pinned,
		"is_numa_system": len(ns.topology) > 1,
	}
}

// discover reads NUMA topology and locates the primary GPU node.
func (ns *NUMAScheduler) discover() error {
	nodeBase := "/sys/devices/system/node"
	entries, err := os.ReadDir(nodeBase)
	if err != nil {
		return fmt.Errorf("read %s: %w", nodeBase, err)
	}

	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "node") {
			continue
		}
		idStr := strings.TrimPrefix(e.Name(), "node")
		id, err := strconv.Atoi(idStr)
		if err != nil {
			continue
		}
		cpuListStr, err := readFile(filepath.Join(nodeBase, e.Name(), "cpulist"))
		if err != nil {
			continue
		}
		cpus, err := parseCPUList(cpuListStr)
		if err != nil {
			continue
		}
		ns.topology = append(ns.topology, numaNode{ID: id, CPUs: cpus, CPUList: cpuListStr})
	}

	ns.gpuNodeID = ns.detectGPUNode()
	log.Printf("[numa] topology: %d nodes, GPU prefers node%d", len(ns.topology), ns.gpuNodeID)
	return nil
}

// detectGPUNode finds the NUMA node closest to the primary GPU.
// Returns -1 if unable to determine (UMA or GPU absent).
func (ns *NUMAScheduler) detectGPUNode() int {
	pciBase := "/sys/bus/pci/devices"
	entries, _ := os.ReadDir(pciBase)
	for _, e := range entries {
		classPath := filepath.Join(pciBase, e.Name(), "class")
		class, err := readFile(classPath)
		if err != nil {
			continue
		}
		// PCI class 0x0300xx = VGA/3D graphics controllers.
		if !strings.HasPrefix(class, "0x0300") {
			continue
		}
		numaPath := filepath.Join(pciBase, e.Name(), "numa_node")
		numaStr, err := readFile(numaPath)
		if err != nil {
			continue
		}
		nodeID, err := strconv.Atoi(numaStr)
		if err != nil || nodeID < 0 {
			continue
		}
		return nodeID
	}
	return -1
}

// pinPID calls taskset to set CPU affinity for a process.
func (ns *NUMAScheduler) pinPID(pid int, cpuList string) error {
	out, err := exec.Command("taskset", "--cpu-list", cpuList, "-p", strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
