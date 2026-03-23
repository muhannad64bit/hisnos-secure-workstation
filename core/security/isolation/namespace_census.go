// core/security/isolation/namespace_census.go
//
// NamespaceCensus enumerates all Linux namespaces and identifies orphans.
//
// An orphan namespace is one that:
//   - Exists in /proc (reachable from some process).
//   - Is NOT owned by a process whose parent chain leads to an allowed runtime.
//   - Has been running for longer than orphanGraceWindow (5 minutes).
//
// Actions available on suspicious namespace trees:
//   - Flag: emit security event only (non-destructive).
//   - Kill:  send SIGKILL to all processes in the namespace tree.
//
// Allowed runtimes (do not flag their namespaces):
//   systemd, distrobox, kvm, qemu, containerd, podman, crun, runc, buildah
//
// Census is run every 2 minutes via RunLoop().
// Results are exposed via CurrentCensus() for API/dashboard consumption.

package isolation

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	orphanGraceWindow  = 5 * time.Minute
	censusInterval     = 2 * time.Minute
	censusReportFile   = "/var/lib/hisnos/namespace-census.json"
)

// AllowedRuntimes are process names whose namespaces are always trusted.
var AllowedRuntimes = []string{
	"systemd", "distrobox", "kvm", "qemu", "qemu-kvm",
	"containerd", "podman", "crun", "runc", "buildah",
	"chrome", "chromium", "firefox", "flatpak",
	"bwrap", // bubblewrap used by Flatpak
}

// ── Census types ──────────────────────────────────────────────────────────

// NSEntry describes one namespace from the census.
type NSEntry struct {
	Inode      string    `json:"inode"`
	Type       string    `json:"type"`       // net, pid, mnt, user, uts, ipc
	OwnerPIDs  []int     `json:"owner_pids"` // PIDs in this namespace
	OwnerComms []string  `json:"owner_comms"`
	KnownRuntime bool   `json:"known_runtime"`
	Orphan     bool      `json:"orphan"`
	FirstSeen  time.Time `json:"first_seen"`
}

// CensusResult is the full snapshot of all namespaces.
type CensusResult struct {
	Timestamp  time.Time  `json:"timestamp"`
	Total      int        `json:"total_namespaces"`
	Orphans    int        `json:"orphan_namespaces"`
	Entries    []NSEntry  `json:"entries"`
}

// ── Census ────────────────────────────────────────────────────────────────

// NamespaceCensus is the namespace monitoring service.
type NamespaceCensus struct {
	mu        sync.RWMutex
	firstSeen map[string]time.Time // inode → first-observed time
	current   *CensusResult
	onOrphan  func(entry NSEntry)
}

// NewNamespaceCensus creates a census. onOrphan is called when a new orphan is detected.
func NewNamespaceCensus(onOrphan func(NSEntry)) *NamespaceCensus {
	return &NamespaceCensus{
		firstSeen: make(map[string]time.Time),
		onOrphan:  onOrphan,
	}
}

// RunLoop runs the census on censusInterval. Blocks until done is closed.
func (c *NamespaceCensus) RunLoop(done <-chan struct{}) {
	c.run()
	ticker := time.NewTicker(censusInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			c.run()
		}
	}
}

// CurrentCensus returns the most recent census result.
func (c *NamespaceCensus) CurrentCensus() *CensusResult {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.current
}

// KillNamespaceTree sends SIGKILL to all processes in the given namespace inode.
// Returns the number of processes killed.
func (c *NamespaceCensus) KillNamespaceTree(inode string) (int, error) {
	c.mu.RLock()
	census := c.current
	c.mu.RUnlock()

	if census == nil {
		return 0, fmt.Errorf("no census available")
	}

	for _, entry := range census.Entries {
		if entry.Inode != inode {
			continue
		}
		count := 0
		var errs []string
		for _, pid := range entry.OwnerPIDs {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				errs = append(errs, fmt.Sprintf("kill %d: %v", pid, err))
			} else {
				count++
			}
		}
		if len(errs) > 0 {
			return count, fmt.Errorf("partial kill: %s", strings.Join(errs, "; "))
		}
		return count, nil
	}
	return 0, fmt.Errorf("inode %s not found in census", inode)
}

// ── Internal ──────────────────────────────────────────────────────────────

func (c *NamespaceCensus) run() {
	now := time.Now()

	// Map: nsKey (type:inode) → pids + comms.
	type nsData struct {
		nsType string
		inode  string
		pids   []int
		comms  []string
	}
	nsMap := make(map[string]*nsData)

	procs, _ := filepath.Glob("/proc/[0-9]*")
	for _, dir := range procs {
		pid, _ := strconv.Atoi(filepath.Base(dir))
		if pid == 0 {
			continue
		}
		comm := strings.TrimSpace(readProc(dir + "/comm"))

		nsTypes := []string{"net", "pid", "mnt", "user", "uts", "ipc", "cgroup"}
		for _, nsType := range nsTypes {
			target, err := os.Readlink(filepath.Join(dir, "ns", nsType))
			if err != nil {
				continue
			}
			// target is "net:[4026531992]"
			key := nsType + ":" + target
			if nsMap[key] == nil {
				// Parse inode from "net:[4026531992]"
				inode := target
				if idx := strings.Index(target, "["); idx >= 0 {
					inode = strings.Trim(target[idx:], "[]")
				}
				nsMap[key] = &nsData{nsType: nsType, inode: inode}
			}
			nsMap[key].pids = append(nsMap[key].pids, pid)
			nsMap[key].comms = appendUnique(nsMap[key].comms, comm)
		}
	}

	// Get init (PID 1) namespaces to use as the "host" baseline.
	initNS := make(map[string]string) // type → inode
	for _, nsType := range []string{"net", "pid", "mnt", "user", "uts", "ipc"} {
		target, _ := os.Readlink(filepath.Join("/proc/1/ns", nsType))
		if idx := strings.Index(target, "["); idx >= 0 {
			initNS[nsType] = strings.Trim(target[idx:], "[]")
		}
	}

	var entries []NSEntry
	orphanCount := 0

	for _, ns := range nsMap {
		// Is this a host (init) namespace?
		if initInode, ok := initNS[ns.nsType]; ok && initInode == ns.inode {
			continue // skip host namespaces
		}

		// Record first-seen time.
		key := ns.nsType + ":" + ns.inode
		c.mu.Lock()
		if _, exists := c.firstSeen[key]; !exists {
			c.firstSeen[key] = now
		}
		firstSeen := c.firstSeen[key]
		c.mu.Unlock()

		// Is it owned by a known runtime?
		knownRuntime := false
		for _, comm := range ns.comms {
			if isKnownRuntime(comm) {
				knownRuntime = true
				break
			}
		}

		// Orphan: non-host, non-known-runtime, older than grace window.
		orphan := !knownRuntime && now.Sub(firstSeen) > orphanGraceWindow

		entry := NSEntry{
			Inode:        ns.inode,
			Type:         ns.nsType,
			OwnerPIDs:    ns.pids,
			OwnerComms:   ns.comms,
			KnownRuntime: knownRuntime,
			Orphan:       orphan,
			FirstSeen:    firstSeen,
		}
		entries = append(entries, entry)

		if orphan {
			orphanCount++
			log.Printf("[namespace] orphan %s namespace inode=%s comms=%v age=%s",
				ns.nsType, ns.inode, ns.comms, now.Sub(firstSeen).Round(time.Second))
			if c.onOrphan != nil {
				c.onOrphan(entry)
			}
		}
	}

	result := &CensusResult{
		Timestamp: now,
		Total:     len(entries),
		Orphans:   orphanCount,
		Entries:   entries,
	}

	c.mu.Lock()
	c.current = result
	c.mu.Unlock()

	// Persist census report.
	go persistCensus(result)

	if orphanCount > 0 {
		log.Printf("[namespace] census complete: %d namespaces, %d orphans", len(entries), orphanCount)
	}

	// Prune firstSeen entries for gone namespaces.
	c.mu.Lock()
	seen := make(map[string]bool)
	for _, ns := range nsMap {
		seen[ns.nsType+":"+ns.inode] = true
	}
	for k := range c.firstSeen {
		if !seen[k] {
			delete(c.firstSeen, k)
		}
	}
	c.mu.Unlock()
}

// ── Helpers ───────────────────────────────────────────────────────────────

func isKnownRuntime(comm string) bool {
	lower := strings.ToLower(comm)
	for _, known := range AllowedRuntimes {
		if strings.Contains(lower, strings.ToLower(known)) {
			return true
		}
	}
	return false
}

func appendUnique(slice []string, s string) []string {
	for _, existing := range slice {
		if existing == s {
			return slice
		}
	}
	return append(slice, s)
}

func readProc(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func persistCensus(r *CensusResult) {
	data, _ := json.MarshalIndent(r, "", "  ")
	dir := filepath.Dir(censusReportFile)
	os.MkdirAll(dir, 0750)
	tmp, err := os.CreateTemp(dir, ".census-*.tmp")
	if err != nil {
		return
	}
	tmp.Write(data)
	tmp.Sync()
	tmp.Close()
	os.Rename(tmp.Name(), censusReportFile)
}
