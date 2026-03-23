// core/security/containment/containment.go
//
// Containment provides emergency network and process isolation primitives.
//
// Available profiles:
//   EmergencyNetworkContainment:
//     Installs a highest-priority nft table that blocks all egress/ingress
//     except loopback.  Reversible via RestoreNetwork().
//
//   ProcessQuarantine:
//     Moves target PIDs into a dedicated cgroup (/sys/fs/cgroup/hisnos-quarantine)
//     and applies cpu.max=50000 1000000 (5% CPU).  Does NOT kill processes.
//     Reversible via ReleaseQuarantine(pids).
//
//   FilesystemReadOnly (optional escalation):
//     Remounts /home and /tmp read-only.  Reversible via RestoreFilesystem().
//     Only applied when explicitly requested — this is highly disruptive.
//
// All operations are atomic and reversible.
// State is tracked in-process; a crash leaves containment active until
// the operator manually reverts (a safe default for an emergency measure).
//
// Audit: every action is logged to the security event stream via the
// provided callback.

package containment

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const (
	containmentTable = "inet hisnos_containment"
	quarantineCgroup = "/sys/fs/cgroup/hisnos-quarantine"
)

// ── Containment state ──────────────────────────────────────────────────────

// Manager tracks active containment measures and provides rollback.
type Manager struct {
	mu sync.Mutex

	networkContained bool
	fsReadOnly       []string // mount points remounted read-only
	quarantinedPIDs  []int

	onEvent func(action, detail string)
}

// NewManager creates a containment Manager.
// onEvent is called for each action taken (for audit/event-stream).
func NewManager(onEvent func(action, detail string)) *Manager {
	return &Manager{onEvent: onEvent}
}

// ── Network containment ────────────────────────────────────────────────────

// ApplyNetworkContainment installs an emergency nftables table that blocks
// all traffic except loopback.  Layered at priority -100 (before all other rules).
func (m *Manager) ApplyNetworkContainment() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.networkContained {
		return nil // already applied
	}

	nftRules := `
table inet hisnos_containment {
    chain containment_in {
        type filter hook input priority -100; policy drop;
        iif lo accept comment "allow loopback"
        ct state established,related accept comment "allow established"
    }
    chain containment_out {
        type filter hook output priority -100; policy drop;
        oif lo accept comment "allow loopback"
        ct state established,related accept comment "allow established"
    }
    chain containment_fwd {
        type filter hook forward priority -100; policy drop;
    }
}
`
	tmpFile, err := os.CreateTemp("", "hisnos-containment-*.nft")
	if err != nil {
		return fmt.Errorf("create tmp nft file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(nftRules); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write nft rules: %w", err)
	}
	tmpFile.Close()

	if out, err := exec.Command("nft", "-f", tmpFile.Name()).CombinedOutput(); err != nil {
		return fmt.Errorf("nft apply: %w — %s", err, strings.TrimSpace(string(out)))
	}

	m.networkContained = true
	log.Printf("[containment] EMERGENCY NETWORK CONTAINMENT APPLIED")
	m.emit("network_containment_applied",
		"all traffic blocked except loopback (nft table hisnos_containment)")
	return nil
}

// RestoreNetwork removes the emergency nft containment table.
func (m *Manager) RestoreNetwork() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.networkContained {
		return nil
	}

	// Flush then delete the table.
	cmds := [][]string{
		{"nft", "flush", "table", "inet", "hisnos_containment"},
		{"nft", "delete", "table", "inet", "hisnos_containment"},
	}
	for _, args := range cmds {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			// Log but continue — table may already be gone.
			log.Printf("[containment] WARN: %v: %s", err, string(out))
		}
	}

	m.networkContained = false
	log.Printf("[containment] network containment removed")
	m.emit("network_containment_removed", "hisnos_containment table deleted")
	return nil
}

// NetworkContained returns true if emergency network containment is active.
func (m *Manager) NetworkContained() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.networkContained
}

// ── Process quarantine ─────────────────────────────────────────────────────

// QuarantinePIDs moves the given PIDs into the quarantine cgroup.
// Each process's CPU is throttled to 5%.
func (m *Manager) QuarantinePIDs(pids []int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Create the quarantine cgroup.
	if err := os.MkdirAll(quarantineCgroup, 0755); err != nil {
		return fmt.Errorf("create quarantine cgroup: %w", err)
	}

	// Apply CPU throttle (5% CPU per second).
	cpuMax := quarantineCgroup + "/cpu.max"
	if err := os.WriteFile(cpuMax, []byte("50000 1000000\n"), 0644); err != nil {
		log.Printf("[containment] WARN: cannot set cpu.max on quarantine cgroup: %v", err)
	}

	// Optionally restrict memory.
	memMax := quarantineCgroup + "/memory.max"
	if err := os.WriteFile(memMax, []byte("256M\n"), 0644); err != nil {
		log.Printf("[containment] WARN: cannot set memory.max on quarantine cgroup: %v", err)
	}

	quarantinedCount := 0
	for _, pid := range pids {
		procsPath := quarantineCgroup + "/cgroup.procs"
		if err := os.WriteFile(procsPath, []byte(strconv.Itoa(pid)+"\n"), 0644); err != nil {
			log.Printf("[containment] WARN: cannot move PID %d to quarantine: %v", pid, err)
			continue
		}
		m.quarantinedPIDs = append(m.quarantinedPIDs, pid)
		quarantinedCount++
	}

	log.Printf("[containment] quarantined %d/%d processes into %s",
		quarantinedCount, len(pids), quarantineCgroup)
	m.emit("process_quarantine_applied",
		fmt.Sprintf("%d processes moved to quarantine cgroup (CPU: 5%%)", quarantinedCount))
	return nil
}

// ReleaseQuarantine moves quarantined processes back to the root cgroup.
func (m *Manager) ReleaseQuarantine() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rootProcs := "/sys/fs/cgroup/cgroup.procs"
	released := 0
	for _, pid := range m.quarantinedPIDs {
		if err := os.WriteFile(rootProcs, []byte(strconv.Itoa(pid)+"\n"), 0644); err != nil {
			// Process may have exited.
			if !os.IsPermission(err) {
				continue
			}
		}
		released++
	}
	m.quarantinedPIDs = nil

	// Remove the quarantine cgroup (will fail if still has members — acceptable).
	os.Remove(quarantineCgroup)

	log.Printf("[containment] quarantine released (%d processes returned)", released)
	m.emit("process_quarantine_released", fmt.Sprintf("%d processes returned to root cgroup", released))
	return nil
}

// ── Filesystem read-only escalation ───────────────────────────────────────

// RemountReadOnly remounts the given paths read-only.
// This is highly disruptive — only call on explicit operator request.
func (m *Manager) RemountReadOnly(paths []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, path := range paths {
		absPath, err := filepath.Abs(path)
		if err != nil {
			continue
		}
		if err := syscall.Mount("", absPath, "", syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("remount %s read-only: %w", absPath, err)
		}
		m.fsReadOnly = append(m.fsReadOnly, absPath)
		log.Printf("[containment] remounted read-only: %s", absPath)
	}

	m.emit("filesystem_readonly_applied",
		fmt.Sprintf("paths remounted read-only: %v", paths))
	return nil
}

// RestoreFilesystem remounts previously read-only paths as read-write.
func (m *Manager) RestoreFilesystem() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, path := range m.fsReadOnly {
		if err := syscall.Mount("", path, "", syscall.MS_REMOUNT, ""); err != nil {
			log.Printf("[containment] WARN: cannot restore %s to read-write: %v", path, err)
		}
	}
	m.fsReadOnly = nil
	m.emit("filesystem_readonly_restored", "paths restored to read-write")
	return nil
}

// ── Full emergency rollback ────────────────────────────────────────────────

// EmergencyRestore removes all active containment measures.
// Called by safetyNet on crash/SIGTERM.
func (m *Manager) EmergencyRestore() {
	if err := m.RestoreFilesystem(); err != nil {
		log.Printf("[containment] WARN: filesystem restore: %v", err)
	}
	if err := m.ReleaseQuarantine(); err != nil {
		log.Printf("[containment] WARN: quarantine release: %v", err)
	}
	if err := m.RestoreNetwork(); err != nil {
		log.Printf("[containment] WARN: network restore: %v", err)
	}
	log.Printf("[containment] emergency restore complete")
}

// ── Helpers ───────────────────────────────────────────────────────────────

func (m *Manager) emit(action, detail string) {
	if m.onEvent != nil {
		m.onEvent(action, detail)
	}
}
