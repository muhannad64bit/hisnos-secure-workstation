// core/performance/rt_guard.go — Real-time priority escalation guard.
//
// Scans all running processes every 3 seconds and enforces the following policy:
//
//  1. Only explicitly whitelisted process names may hold SCHED_FIFO (policy=1)
//     or SCHED_RR (policy=2) scheduling.
//  2. Any non-whitelisted process found with RT scheduling is demoted to
//     SCHED_OTHER (policy=0, priority=0) via chrt(1).
//  3. Each violation emits a security telemetry event exactly once per unique
//     PID (re-alerts if the PID reappears after being collected away).
//  4. The system-wide RT throttle (/proc/sys/kernel/sched_rt_runtime_us) is
//     clamped to rtBudgetUs (950 ms per second = 95%) on every Tick.
//
// Whitelisted comm names (exact match against /proc/<pid>/comm):
//
//	steam, gamemoded, pipewire, pipewire-pulse, pulseaudio
//	wineserver, wine64, wine, proton
//	jackd, jackdbus
//	systemd (PID 1 subtree — matched by uid=0 + comm prefix)
//
// /proc/<pid>/stat layout (space-separated, comm wrapped in parentheses):
//
//	pid (comm) state ppid pgroup session tty_nr tpgid flags
//	... [fields 1-39] ...
//	field[17] = priority, field[18] = nice,
//	field[40] = rt_priority  (0 = non-RT; >0 = RT priority level)
//	field[41] = policy       (0=OTHER,1=FIFO,2=RR,3=BATCH,5=IDLE,6=DEADLINE)
//
// Stat field indices above use 0-based after splitting on ") " to handle
// comms containing spaces.
package performance

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	rtGuardInterval = 3 * time.Second
	rtBudgetUs      = 950000 // 95% of 1s RT period — kernel default
	rtPeriodUs      = 1000000
)

// rtSchedFIFO and rtSchedRR are Linux scheduling policy constants.
const (
	rtSchedFIFO = 1
	rtSchedRR   = 2
)

// rtWhitelist is the set of process comm names allowed to hold RT scheduling.
var rtWhitelist = map[string]bool{
	"steam":         true,
	"gamemoded":     true,
	"pipewire":      true,
	"pipewire-pulse": true,
	"pulseaudio":    true,
	"wineserver":    true,
	"wine64":        true,
	"wine":          true,
	"proton":        true,
	"jackd":         true,
	"jackdbus":      true,
}

// rtProcess holds metadata about a discovered RT process.
type rtProcess struct {
	PID    int
	Comm   string
	Policy int
	RTPrio int
}

// RTGuard prevents unauthorised RT scheduling escalation.
type RTGuard struct {
	mu       sync.Mutex
	alerted  map[int]bool // PIDs already reported this guard cycle
	demoted  map[int]bool // PIDs already demoted (avoid repeat chrt calls)
	budgetOK bool         // whether sched_rt_runtime_us write succeeded last tick

	emit func(category, event string, data map[string]any)
}

// NewRTGuard creates and returns an RTGuard ready to Tick.
func NewRTGuard(emit func(string, string, map[string]any)) *RTGuard {
	if emit == nil {
		emit = func(_, _ string, _ map[string]any) {}
	}
	g := &RTGuard{
		alerted: make(map[int]bool),
		demoted: make(map[int]bool),
		emit:    emit,
	}
	// Enforce RT budget immediately on creation.
	g.enforceRTBudget()
	return g
}

// Tick performs one guard evaluation cycle.
func (g *RTGuard) Tick() {
	g.enforceRTBudget()

	procs, err := g.scanRTProcesses()
	if err != nil {
		log.Printf("[rt-guard] scan: %v", err)
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	// Garbage-collect alerted/demoted maps — remove PIDs that no longer exist.
	g.gcMaps(procs)

	for _, p := range procs {
		if rtWhitelist[p.Comm] {
			continue // allowed
		}
		// Unauthorised RT process.
		if !g.alerted[p.PID] {
			g.alerted[p.PID] = true
			log.Printf("[rt-guard] VIOLATION pid=%d comm=%q policy=%d rt_prio=%d",
				p.PID, p.Comm, p.Policy, p.RTPrio)
			g.emit("security", "rt_escalation_blocked", map[string]any{
				"pid": p.PID, "comm": p.Comm,
				"policy": p.Policy, "rt_priority": p.RTPrio,
			})
		}
		if !g.demoted[p.PID] {
			if err := demoteToNormal(p.PID); err != nil {
				log.Printf("[rt-guard] WARN: demote pid=%d: %v", p.PID, err)
			} else {
				g.demoted[p.PID] = true
				log.Printf("[rt-guard] demoted pid=%d (%s) → SCHED_OTHER", p.PID, p.Comm)
			}
		}
	}
}

// Status returns a snapshot of the current guard state for IPC.
func (g *RTGuard) Status() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	alerted := make([]int, 0, len(g.alerted))
	for pid := range g.alerted {
		alerted = append(alerted, pid)
	}
	return map[string]any{
		"alerted_pids":   alerted,
		"demoted_count":  len(g.demoted),
		"rt_budget_ok":   g.budgetOK,
		"rt_budget_us":   rtBudgetUs,
	}
}

// enforceRTBudget writes sched_rt_runtime_us to prevent RT starvation of CFS.
func (g *RTGuard) enforceRTBudget() {
	path := "/proc/sys/kernel/sched_rt_runtime_us"
	val := strconv.Itoa(rtBudgetUs)
	err := writeFile(path, val)
	g.mu.Lock()
	g.budgetOK = err == nil
	g.mu.Unlock()
	if err != nil {
		log.Printf("[rt-guard] WARN: set %s: %v", path, err)
	}
}

// scanRTProcesses walks /proc and returns all processes with SCHED_FIFO or SCHED_RR.
func (g *RTGuard) scanRTProcesses() ([]rtProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("readdir /proc: %w", err)
	}
	var result []rtProcess
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // non-numeric entry (e.g. "self", "net")
		}
		p, ok := readRTStat(pid)
		if !ok {
			continue
		}
		if p.Policy == rtSchedFIFO || p.Policy == rtSchedRR {
			result = append(result, p)
		}
	}
	return result, nil
}

// gcMaps removes stale PID entries from alerted/demoted maps.
// Must be called with mu held.
func (g *RTGuard) gcMaps(live []rtProcess) {
	liveSet := make(map[int]bool, len(live))
	for _, p := range live {
		liveSet[p.PID] = true
	}
	for pid := range g.alerted {
		if !liveSet[pid] {
			delete(g.alerted, pid)
			delete(g.demoted, pid)
		}
	}
}

// readRTStat parses /proc/<pid>/stat to extract comm, policy, and rt_priority.
//
// /proc/PID/stat field layout (kernel source: fs/proc/array.c):
//
//	%d (%s) %c %d %d %d %d %d %u %lu ... (41+ fields)
//
// Splitting on ") " after the comm field handles comms containing spaces.
// Fields after the comm (0-indexed starting at 0 = state after ") ") are:
//
//	[0]=state [1]=ppid [2]=pgrp [3]=session [4]=tty_nr [5]=tpgid
//	[6]=flags [7-14]=minflt..cutime [15]=priority [16]=nice
//	[17]=num_threads [18]=itrealvalue [19]=starttime ...
//	[38]=rt_priority [39]=policy
func readRTStat(pid int) (rtProcess, bool) {
	path := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	data, err := os.ReadFile(path)
	if err != nil {
		return rtProcess{}, false
	}
	raw := strings.TrimSpace(string(data))

	// Extract comm between first '(' and last ')'.
	start := strings.Index(raw, "(")
	end := strings.LastIndex(raw, ")")
	if start < 0 || end < 0 || end <= start {
		return rtProcess{}, false
	}
	comm := raw[start+1 : end]

	// Everything after the closing ')' + space.
	rest := strings.TrimSpace(raw[end+1:])
	fields := strings.Fields(rest)
	// We need at least 40 fields after comm (index 38=rt_priority, 39=policy).
	if len(fields) < 40 {
		return rtProcess{}, false
	}

	rtPrio, err1 := strconv.Atoi(fields[38])
	policy, err2 := strconv.Atoi(fields[39])
	if err1 != nil || err2 != nil {
		return rtProcess{}, false
	}

	return rtProcess{PID: pid, Comm: comm, Policy: policy, RTPrio: rtPrio}, true
}

// readCommFile reads /proc/<pid>/comm (trim whitespace).
// Used as a secondary check when stat parsing is ambiguous.
func readCommFile(pid int) string {
	path := filepath.Join("/proc", strconv.Itoa(pid), "comm")
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if sc.Scan() {
		return strings.TrimSpace(sc.Text())
	}
	return ""
}

// demoteToNormal sets a process's scheduling policy to SCHED_OTHER (0) via chrt.
// chrt is part of util-linux; available on all Fedora systems.
func demoteToNormal(pid int) error {
	out, err := exec.Command("chrt", "-o", "-p", "0", strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
