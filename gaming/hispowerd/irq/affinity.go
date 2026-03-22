// irq/affinity.go — Phase 3: IRQ Affinity Optimizer
//
// Detects GPU and NIC IRQ numbers from /proc/interrupts.
// Pins them to gaming cores via /proc/irq/<n>/smp_affinity (hex bitmask).
//
// PRIVILEGE NOTE:
//   Writing to /proc/irq/*/smp_affinity requires CAP_SYS_ADMIN (root).
//   This daemon runs as a user service with NoNewPrivileges=yes.
//   If AmbientCapabilities=CAP_SYS_ADMIN is NOT set, writes will fail with EPERM.
//   The daemon gracefully degrades: logs the failure, continues without IRQ tuning.
//   For full IRQ tuning, either:
//     a) Set AmbientCapabilities=CAP_SYS_ADMIN in the service file, OR
//     b) Configure the companion system service hisnos-hispowerd-irq.service
//
// Safety: previous smp_affinity values are saved before any change.
// Restore is always attempted regardless of errors.
// If restore fails: recovery script writes "ff" (all CPUs) to all saved IRQ paths.

package irq

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"hisnos.local/hispowerd/config"
	"hisnos.local/hispowerd/observe"
)

// savedIRQ records the pre-gaming smp_affinity for one IRQ.
type savedIRQ struct {
	number   int
	category string // "gpu" | "nic"
	previous string // hex bitmask as read from smp_affinity
}

// Optimizer implements IRQ affinity management.
type Optimizer struct {
	cfg   *config.Config
	log   *observe.Logger
	mu    sync.Mutex
	saved []savedIRQ
}

// NewOptimizer creates an IRQ Optimizer.
func NewOptimizer(cfg *config.Config, log *observe.Logger) *Optimizer {
	return &Optimizer{cfg: cfg, log: log}
}

// Apply detects GPU/NIC IRQs and pins them to gaming cores.
// Partial success is not treated as failure.
func (o *Optimizer) Apply() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.saved = o.saved[:0]

	detected, err := detectIRQs(o.cfg)
	if err != nil {
		return fmt.Errorf("irq detect: %w", err)
	}
	if len(detected) == 0 {
		o.log.Info("irq: no GPU/NIC IRQs detected in /proc/interrupts")
		return nil
	}

	// Build gaming cores hex mask: bitmask of gaming core bits → hex string.
	gamingHex := fmt.Sprintf("%x", o.cfg.GamingCoreMask())

	var tuned int
	var permErr bool
	for _, irqEntry := range detected {
		prev, err := readAffinityMask(irqEntry.number)
		if err != nil {
			o.log.Warn("irq: read affinity irq=%d: %v", irqEntry.number, err)
			continue
		}
		if err := writeAffinityMask(irqEntry.number, gamingHex); err != nil {
			if isPermError(err) {
				permErr = true
			}
			o.log.Warn("irq: write affinity irq=%d (%s): %v", irqEntry.number, irqEntry.name, err)
			continue
		}
		o.saved = append(o.saved, savedIRQ{
			number:   irqEntry.number,
			category: irqEntry.category,
			previous: prev,
		})
		o.log.Info("irq: pinned irq=%d (%s) [%s] → cores 0x%s",
			irqEntry.number, irqEntry.name, irqEntry.category, gamingHex)
		tuned++
	}

	if permErr && tuned == 0 {
		return fmt.Errorf("irq: CAP_SYS_ADMIN required for smp_affinity writes; set AmbientCapabilities=CAP_SYS_ADMIN in service or use hisnos-hispowerd-irq.service helper")
	}
	if tuned > 0 {
		o.log.Info("irq: tuned %d IRQ(s) to gaming cores", tuned)
	}
	return nil
}

// Restore reinstates all saved IRQ affinities.
// On error, logs but continues restoring remaining IRQs.
func (o *Optimizer) Restore() error {
	o.mu.Lock()
	defer o.mu.Unlock()

	var lastErr error
	for _, s := range o.saved {
		if err := writeAffinityMask(s.number, s.previous); err != nil {
			o.log.Warn("irq: restore irq=%d: %v", s.number, err)
			lastErr = err
		} else {
			o.log.Info("irq: restored irq=%d → 0x%s", s.number, s.previous)
		}
	}
	o.saved = o.saved[:0]
	return lastErr
}

// EmergencyRestore writes the all-CPUs mask to all saved IRQs.
// Used by the crash recovery path when Restore() itself fails.
func (o *Optimizer) EmergencyRestore() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, s := range o.saved {
		_ = writeAffinityMask(s.number, "ffffffff")
	}
	o.saved = o.saved[:0]
}

// SavedIRQs returns the list of saved IRQ numbers (for recovery script).
func (o *Optimizer) SavedIRQs() []int {
	o.mu.Lock()
	defer o.mu.Unlock()
	nums := make([]int, len(o.saved))
	for i, s := range o.saved {
		nums[i] = s.number
	}
	return nums
}

// ─── /proc/interrupts parsing ────────────────────────────────────────────────

type irqEntry struct {
	number   int
	name     string
	category string // "gpu" | "nic"
}

// detectIRQs parses /proc/interrupts and returns GPU and NIC IRQs.
func detectIRQs(cfg *config.Config) ([]irqEntry, error) {
	f, err := os.Open("/proc/interrupts")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []irqEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		// Lines format: "  <irq>:  <cpu0> ... <cpuN>  <type>  <name1> <name2>..."
		// Skip header (starts with "CPU").
		if strings.HasPrefix(strings.TrimSpace(line), "CPU") {
			continue
		}
		entry, ok := parseInterruptLine(line, cfg)
		if ok {
			entries = append(entries, entry)
		}
	}
	return entries, sc.Err()
}

func parseInterruptLine(line string, cfg *config.Config) (irqEntry, bool) {
	// Trim and split.
	line = strings.TrimSpace(line)
	colonIdx := strings.Index(line, ":")
	if colonIdx < 0 {
		return irqEntry{}, false
	}

	irqStr := strings.TrimSpace(line[:colonIdx])
	irqNum, err := strconv.Atoi(irqStr)
	if err != nil {
		return irqEntry{}, false // non-numeric IRQ (e.g., "NMI", "LOC")
	}

	// The device name is the last field after the interrupt type.
	rest := strings.TrimSpace(line[colonIdx+1:])
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return irqEntry{}, false
	}

	// Last field(s) are device names.
	deviceName := strings.ToLower(strings.Join(fields[len(fields)-1:], " "))

	// Check GPU patterns.
	for _, pat := range cfg.GPUIRQPatterns {
		if strings.Contains(deviceName, strings.ToLower(pat)) {
			return irqEntry{number: irqNum, name: deviceName, category: "gpu"}, true
		}
	}
	// Check NIC patterns.
	for _, pat := range cfg.NICIRQPatterns {
		if strings.Contains(deviceName, strings.ToLower(pat)) {
			return irqEntry{number: irqNum, name: deviceName, category: "nic"}, true
		}
	}
	return irqEntry{}, false
}

// ─── smp_affinity file I/O ───────────────────────────────────────────────────

func irqAffinityPath(irq int) string {
	return fmt.Sprintf("/proc/irq/%d/smp_affinity", irq)
}

func readAffinityMask(irq int) (string, error) {
	data, err := os.ReadFile(irqAffinityPath(irq))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func writeAffinityMask(irq int, hexMask string) error {
	path := irqAffinityPath(irq)
	return os.WriteFile(path, []byte(hexMask+"\n"), 0644)
}

func isPermError(err error) bool {
	return strings.Contains(err.Error(), "operation not permitted") ||
		strings.Contains(err.Error(), "permission denied")
}

// ParseHexMask converts a hex mask string to uint64 (used in recovery).
func ParseHexMask(hex string) (uint64, error) {
	hex = strings.TrimPrefix(strings.TrimSpace(hex), "0x")
	return strconv.ParseUint(hex, 16, 64)
}
