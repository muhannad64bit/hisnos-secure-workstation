// core/performance/irq_runtime.go — Hardware interrupt affinity management.
//
// Routes GPU and NIC IRQs to designated CPU cores, reducing interrupt
// processing pressure on cores reserved for game workloads.
// Reads /proc/interrupts; writes /proc/irq/<n>/smp_affinity_list.
package performance

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// IRQRuntime manages hardware interrupt CPU affinity.
type IRQRuntime struct{}

// IRQSnapshot holds per-IRQ affinity before a profile switch for rollback.
type IRQSnapshot struct {
	Affinity map[int]string // irq number → smp_affinity_list value
}

// Driver name patterns to identify GPU and NIC IRQs in /proc/interrupts.
var (
	gpuPattern = regexp.MustCompile(`(?i)nvidia|amdgpu|radeon|i915|xe`)
	nicPattern = regexp.MustCompile(`(?i)(eth|ens|enp|eno|wlp|wlan|wlo|mlx|ixgbe|e1000|igb|igc|virtio-net)\d`)
)

// Snapshot captures current affinity for all GPU and NIC IRQs.
func (r *IRQRuntime) Snapshot() (*IRQSnapshot, error) {
	irqs, err := r.findTargetIRQs()
	if err != nil {
		return nil, err
	}
	snap := &IRQSnapshot{Affinity: make(map[int]string, len(irqs))}
	for _, irq := range irqs {
		v, err := readFile(fmt.Sprintf("/proc/irq/%d/smp_affinity_list", irq))
		if err != nil {
			continue // IRQ may have disappeared between scan and read
		}
		snap.Affinity[irq] = v
	}
	return snap, nil
}

// Apply routes GPU and NIC IRQs to the CPU set given by cpuList (e.g. "0-1").
// Managed IRQs (smp_affinity read-only) are skipped with a warning.
func (r *IRQRuntime) Apply(cpuList string) error {
	irqs, err := r.findTargetIRQs()
	if err != nil {
		return err
	}
	moved := 0
	for _, irq := range irqs {
		path := fmt.Sprintf("/proc/irq/%d/smp_affinity_list", irq)
		if err := writeFile(path, cpuList); err != nil {
			// Managed IRQs are controlled by the driver and cannot be redirected.
			log.Printf("[perf/irq] WARN: irq%d smp_affinity_list: %v (managed?)", irq, err)
			continue
		}
		moved++
	}
	log.Printf("[perf/irq] routed %d/%d GPU+NIC IRQs → CPUs [%s]", moved, len(irqs), cpuList)
	return nil
}

// Restore resets IRQ affinity to the snapshotted values.
func (r *IRQRuntime) Restore(snap *IRQSnapshot) {
	if snap == nil {
		return
	}
	for irq, aff := range snap.Affinity {
		path := fmt.Sprintf("/proc/irq/%d/smp_affinity_list", irq)
		if err := writeFile(path, aff); err != nil {
			log.Printf("[perf/irq] WARN: restore irq%d: %v", irq, err)
		}
	}
}

// findTargetIRQs scans /proc/interrupts for GPU and NIC interrupt numbers.
func (r *IRQRuntime) findTargetIRQs() ([]int, error) {
	f, err := os.Open("/proc/interrupts")
	if err != nil {
		return nil, fmt.Errorf("open /proc/interrupts: %w", err)
	}
	defer f.Close()

	var irqs []int
	seen := make(map[int]bool)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// First field: "16:" or "NMI:" etc.
		irqStr := strings.TrimSuffix(fields[0], ":")
		irq, err := strconv.Atoi(irqStr)
		if err != nil {
			continue // non-numeric (NMI, LOC, SPU, etc.)
		}
		if seen[irq] {
			continue
		}
		// Match device name (everything after the count columns).
		lineRest := strings.Join(fields[1:], " ")
		if gpuPattern.MatchString(lineRest) || nicPattern.MatchString(lineRest) {
			irqs = append(irqs, irq)
			seen[irq] = true
		}
	}
	return irqs, sc.Err()
}
