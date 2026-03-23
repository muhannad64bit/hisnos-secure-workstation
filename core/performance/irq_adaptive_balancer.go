// core/performance/irq_adaptive_balancer.go — Real-time IRQ load adaptive balancer.
//
// Monitors GPU, NIC, and USB interrupt rates per CPU. When distribution becomes
// skewed (high coefficient of variation across CPUs), it rebalances affinity
// to spread load across performance cores, reducing frame-time jitter.
//
// Integration with irqbalance:
//   If irqbalance.service is active when gaming starts, it is stopped.
//   On Stop(), irqbalance is restarted so system returns to managed state.
//   If irqbalance was already inactive, it is not touched.
//
// Rebalance frequency: every 4 seconds during gaming.
// Cooldown between rebalances: 8 seconds (prevent thrashing).
// Imbalance threshold: coefficient of variation > 0.4 (40%) across active CPUs.
package performance

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	irqScanInterval   = 4 * time.Second
	irqRebalanceCooldown = 8 * time.Second
	irqImbalanceCV    = 0.40 // coefficient of variation threshold
)

// irqSample tracks a single IRQ's per-CPU counts across two measurements.
type irqSample struct {
	IRQ     int
	Name    string
	PerCPU  []uint64 // latest count per CPU
	PrevCPU []uint64 // previous count per CPU
}

// IRQAdaptiveBalancer manages dynamic IRQ CPU affinity during gaming.
type IRQAdaptiveBalancer struct {
	mu             sync.Mutex
	samples        map[int]*irqSample // irq number → sample
	lastRebalance  time.Time
	irqbalanceWasActive bool
	perfCores      string // cpulist of performance cores (e.g. "2-11")

	emit func(category, msg string, data map[string]any)
}

// NewIRQAdaptiveBalancer creates the balancer with the given performance core cpulist.
func NewIRQAdaptiveBalancer(perfCores string, emit func(string, string, map[string]any)) *IRQAdaptiveBalancer {
	if emit == nil {
		emit = func(_, _ string, _ map[string]any) {}
	}
	return &IRQAdaptiveBalancer{
		samples:   make(map[int]*irqSample),
		perfCores: perfCores,
		emit:      emit,
	}
}

// Start begins the adaptive balancing loop. Call Stop() to tear down.
func (b *IRQAdaptiveBalancer) Start(perfCores string) {
	if perfCores != "" {
		b.mu.Lock()
		b.perfCores = perfCores
		b.mu.Unlock()
	}
	b.stopIRQBalance()
	log.Printf("[irq-adaptive] started (perfCores=%s interval=%v threshold=%.0f%%)",
		b.perfCores, irqScanInterval, irqImbalanceCV*100)
}

// Tick performs one evaluation cycle (called by supervisor goroutine).
func (b *IRQAdaptiveBalancer) Tick() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.collect(); err != nil {
		log.Printf("[irq-adaptive] collect: %v", err)
		return
	}

	if time.Since(b.lastRebalance) < irqRebalanceCooldown {
		return
	}

	skewed := b.findSkewedIRQs()
	if len(skewed) == 0 {
		return
	}

	moved := 0
	for _, irq := range skewed {
		path := fmt.Sprintf("/proc/irq/%d/smp_affinity_list", irq)
		if err := writeFile(path, b.perfCores); err != nil {
			log.Printf("[irq-adaptive] WARN: rebalance irq%d: %v", irq, err)
			continue
		}
		moved++
	}

	if moved > 0 {
		b.lastRebalance = time.Now()
		b.emit("performance", "irq_rebalanced", map[string]any{
			"irqs_moved": moved, "target_cpus": b.perfCores,
		})
		log.Printf("[irq-adaptive] rebalanced %d IRQs → CPUs [%s]", moved, b.perfCores)
	}
}

// Stop restores irqbalance if it was running before gaming started.
func (b *IRQAdaptiveBalancer) Stop() {
	b.mu.Lock()
	wasActive := b.irqbalanceWasActive
	b.mu.Unlock()

	if wasActive {
		if out, err := exec.Command("systemctl", "start", "irqbalance.service").CombinedOutput(); err != nil {
			log.Printf("[irq-adaptive] WARN: restart irqbalance: %v: %s", err, strings.TrimSpace(string(out)))
		} else {
			log.Printf("[irq-adaptive] irqbalance.service restarted")
		}
	}
}

// stopIRQBalance stops irqbalance if active, recording prior state.
func (b *IRQAdaptiveBalancer) stopIRQBalance() {
	b.mu.Lock()
	defer b.mu.Unlock()

	out, err := exec.Command("systemctl", "is-active", "irqbalance.service").Output()
	active := err == nil && strings.TrimSpace(string(out)) == "active"
	b.irqbalanceWasActive = active

	if active {
		if _, err := exec.Command("systemctl", "stop", "irqbalance.service").CombinedOutput(); err != nil {
			log.Printf("[irq-adaptive] WARN: stop irqbalance: %v (continuing anyway)", err)
		} else {
			log.Printf("[irq-adaptive] irqbalance.service stopped for adaptive mode")
		}
	}
}

// collect parses /proc/interrupts and updates samples for GPU/NIC/USB IRQs.
// Must be called with mu held.
func (b *IRQAdaptiveBalancer) collect() error {
	f, err := os.Open("/proc/interrupts")
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	cpuCount := 0
	first := true
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// Header line: "CPU0 CPU1 ..."
		if first && strings.HasPrefix(strings.TrimSpace(line), "CPU") {
			cpuCount = len(fields)
			first = false
			continue
		}
		irqStr := strings.TrimSuffix(fields[0], ":")
		irq, err := strconv.Atoi(irqStr)
		if err != nil {
			continue
		}
		lineRest := strings.Join(fields[1:], " ")
		if !gpuPattern.MatchString(lineRest) && !nicPattern.MatchString(lineRest) &&
			!strings.Contains(strings.ToLower(lineRest), "xhci") {
			continue
		}
		counts := make([]uint64, 0, cpuCount)
		for i := 1; i <= cpuCount && i < len(fields); i++ {
			n, _ := strconv.ParseUint(fields[i], 10, 64)
			counts = append(counts, n)
		}
		if s, ok := b.samples[irq]; ok {
			copy(s.PrevCPU, s.PerCPU)
			s.PerCPU = counts
		} else {
			b.samples[irq] = &irqSample{
				IRQ:     irq,
				Name:    lineRest,
				PerCPU:  counts,
				PrevCPU: make([]uint64, len(counts)),
			}
		}
	}
	return sc.Err()
}

// findSkewedIRQs returns IRQ numbers where the interrupt rate is significantly
// concentrated on a single CPU (coefficient of variation > threshold).
// Must be called with mu held.
func (b *IRQAdaptiveBalancer) findSkewedIRQs() []int {
	var skewed []int
	for irq, s := range b.samples {
		if len(s.PerCPU) == 0 {
			continue
		}
		// Compute per-CPU rates (delta counts).
		rates := make([]float64, len(s.PerCPU))
		var total float64
		for i, cur := range s.PerCPU {
			prev := uint64(0)
			if i < len(s.PrevCPU) {
				prev = s.PrevCPU[i]
			}
			if cur >= prev {
				rates[i] = float64(cur - prev)
			}
			total += rates[i]
		}
		if total < 10 { // skip low-activity IRQs
			continue
		}
		// Coefficient of variation = stddev / mean.
		n := float64(len(rates))
		mean := total / n
		if mean == 0 {
			continue
		}
		var variance float64
		for _, r := range rates {
			d := r - mean
			variance += d * d
		}
		variance /= n
		cv := sqrtApprox(variance) / mean
		if cv > irqImbalanceCV {
			skewed = append(skewed, irq)
		}
	}
	return skewed
}
