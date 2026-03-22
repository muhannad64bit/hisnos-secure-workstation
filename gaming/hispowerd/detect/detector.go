// detect/detector.go — Phase 1: Game Session Detection Engine
//
// Scans /proc every ScanIntervalSeconds to detect active gaming sessions.
// Detection priority order:
//   1. session.lock file (manual override — highest priority)
//   2. Steam process tree
//   3. Proton / Wine processes (children of Steam or standalone)
//   4. Game allowlist executables
//
// Returns a Session struct describing the detected session.
// Session.Active = false means no gaming workload detected.
//
// No root required. Reads /proc as the user running the daemon.

package detect

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"hisnos.local/hispowerd/config"
	"hisnos.local/hispowerd/observe"
)

// Session describes a detected gaming session.
type Session struct {
	Active      bool
	PID         int    // primary game/launcher PID
	Name        string // executable name
	SessionType string // "steam" | "proton" | "wine" | "manual"
	DetectedAt  time.Time
}

// Detector scans /proc for gaming workloads.
type Detector struct {
	cfg *config.Config
	log *observe.Logger

	// Track when we first detected this session to set start_timestamp.
	sessionStart time.Time
	lastPID      int
}

// NewDetector creates a Detector.
func NewDetector(cfg *config.Config, log *observe.Logger) *Detector {
	return &Detector{cfg: cfg, log: log}
}

// Detect performs a single scan and returns the current session state.
func (d *Detector) Detect() Session {
	// 1. Manual session.lock check (highest priority).
	if s, ok := d.checkLockFile(); ok {
		d.trackStart(s.PID)
		s.DetectedAt = d.sessionStart
		return s
	}

	// 2. Scan /proc for game processes.
	procs, err := scanProc()
	if err != nil {
		d.log.Warn("detect: /proc scan error: %v", err)
		return Session{}
	}

	// 3. Steam detection.
	if d.cfg.SteamDetection {
		if s, ok := d.detectSteam(procs); ok {
			d.trackStart(s.PID)
			s.DetectedAt = d.sessionStart
			return s
		}
	}

	// 4. Proton/Wine detection.
	if d.cfg.ProtonDetection {
		if s, ok := d.detectProton(procs); ok {
			d.trackStart(s.PID)
			s.DetectedAt = d.sessionStart
			return s
		}
	}

	// 5. Allowlist match.
	if s, ok := d.detectAllowlist(procs); ok {
		d.trackStart(s.PID)
		s.DetectedAt = d.sessionStart
		return s
	}

	// No gaming detected — reset tracker.
	d.lastPID = 0
	d.sessionStart = time.Time{}
	return Session{}
}

// checkLockFile returns a manual session if the lock file exists.
func (d *Detector) checkLockFile() (Session, bool) {
	data, err := os.ReadFile(d.cfg.SessionLockFile)
	if err != nil {
		return Session{}, false
	}

	s := Session{
		Active:      true,
		SessionType: "manual",
		Name:        "manual",
	}

	// Parse optional fields: pid=<n>, name=<game>
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "pid=") {
			if pid, err := strconv.Atoi(strings.TrimPrefix(line, "pid=")); err == nil {
				s.PID = pid
			}
		}
		if strings.HasPrefix(line, "name=") {
			s.Name = strings.TrimPrefix(line, "name=")
		}
	}
	return s, true
}

// detectSteam looks for a running Steam process tree.
func (d *Detector) detectSteam(procs []procEntry) (Session, bool) {
	for _, p := range procs {
		if isSteamProcess(p) {
			// Look for a child game process (not just the launcher).
			if gameChild := findSteamGameChild(procs, p.pid); gameChild.pid != 0 {
				return Session{
					Active:      true,
					PID:         gameChild.pid,
					Name:        gameChild.comm,
					SessionType: "steam",
				}, true
			}
			// Steam itself is running — treat as active session.
			return Session{
				Active:      true,
				PID:         p.pid,
				Name:        "steam",
				SessionType: "steam",
			}, true
		}
	}
	return Session{}, false
}

// detectProton looks for Proton or Wine processes.
func (d *Detector) detectProton(procs []procEntry) (Session, bool) {
	for _, p := range procs {
		if isProtonProcess(p) {
			stype := "proton"
			if strings.Contains(strings.ToLower(p.comm), "wine") && !strings.Contains(strings.ToLower(p.comm), "proton") {
				stype = "wine"
			}
			return Session{
				Active:      true,
				PID:         p.pid,
				Name:        p.comm,
				SessionType: stype,
			}, true
		}
	}
	return Session{}, false
}

// detectAllowlist checks if any running process matches the game allowlist.
func (d *Detector) detectAllowlist(procs []procEntry) (Session, bool) {
	allowset := make(map[string]struct{}, len(d.cfg.GameAllowlist))
	for _, name := range d.cfg.GameAllowlist {
		allowset[strings.ToLower(name)] = struct{}{}
	}
	for _, p := range procs {
		nameLower := strings.ToLower(p.comm)
		exeLower := strings.ToLower(filepath.Base(p.exe))
		if _, ok := allowset[nameLower]; ok {
			return Session{Active: true, PID: p.pid, Name: p.comm, SessionType: "steam"}, true
		}
		if _, ok := allowset[exeLower]; ok {
			return Session{Active: true, PID: p.pid, Name: exeLower, SessionType: "steam"}, true
		}
	}
	return Session{}, false
}

// trackStart records session start time on PID change.
func (d *Detector) trackStart(pid int) {
	if pid != d.lastPID {
		d.lastPID = pid
		d.sessionStart = time.Now()
	}
	if d.sessionStart.IsZero() {
		d.sessionStart = time.Now()
	}
}

// ─── /proc helpers ───────────────────────────────────────────────────────────

type procEntry struct {
	pid  int
	ppid int
	comm string   // /proc/<pid>/comm (truncated at 15 chars by kernel)
	exe  string   // /proc/<pid>/exe symlink target
	cmdline []string // /proc/<pid>/cmdline split on NUL
}

func isSteamProcess(p procEntry) bool {
	name := strings.ToLower(p.comm)
	if name == "steam" || name == "steam.sh" {
		return true
	}
	for _, arg := range p.cmdline {
		if strings.Contains(strings.ToLower(arg), "steam") &&
			strings.Contains(strings.ToLower(arg), ".sh") {
			return true
		}
	}
	return false
}

func isProtonProcess(p procEntry) bool {
	name := strings.ToLower(p.comm)
	exe := strings.ToLower(p.exe)
	for _, keyword := range []string{"proton", "wine64", "wine", "wineserver", "winedevice"} {
		if strings.Contains(name, keyword) || strings.Contains(exe, keyword) {
			return true
		}
	}
	// .exe in cmdline (Windows game via Proton/Wine)
	for _, arg := range p.cmdline {
		if strings.HasSuffix(strings.ToLower(arg), ".exe") {
			return true
		}
	}
	return false
}

// findSteamGameChild finds a non-launcher child of the Steam process.
func findSteamGameChild(procs []procEntry, steamPID int) procEntry {
	for _, p := range procs {
		if p.ppid == steamPID && !isSteamProcess(p) && !isSystemProcess(p) {
			return p
		}
	}
	return procEntry{}
}

func isSystemProcess(p procEntry) bool {
	for _, s := range []string{"sh", "bash", "python", "steam", "steamwebhelper"} {
		if p.comm == s {
			return true
		}
	}
	return false
}

// scanProc reads all numeric /proc entries and returns their details.
func scanProc() ([]procEntry, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}

	procs := make([]procEntry, 0, 256)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p := readProcEntry(pid)
		if p.pid != 0 {
			procs = append(procs, p)
		}
	}
	return procs, nil
}

func readProcEntry(pid int) procEntry {
	base := fmt.Sprintf("/proc/%d", pid)

	// comm (process name, up to 15 chars)
	comm, _ := readTrimmed(filepath.Join(base, "comm"))

	// status (ppid)
	ppid := readPPID(filepath.Join(base, "status"))

	// exe symlink
	exe, _ := os.Readlink(filepath.Join(base, "exe"))

	// cmdline (NUL-separated)
	var cmdline []string
	if data, err := os.ReadFile(filepath.Join(base, "cmdline")); err == nil {
		for _, part := range strings.Split(string(data), "\x00") {
			if part != "" {
				cmdline = append(cmdline, part)
			}
		}
	}

	if comm == "" && exe == "" {
		return procEntry{}
	}

	return procEntry{
		pid:     pid,
		ppid:    ppid,
		comm:    comm,
		exe:     exe,
		cmdline: cmdline,
	}
}

func readPPID(statusPath string) int {
	f, err := os.Open(statusPath)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "PPid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				ppid, _ := strconv.Atoi(fields[1])
				return ppid
			}
		}
	}
	return 0
}

func readTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\n"), nil
}

// FindGameProcesses returns all PIDs that appear to be game workloads
// descended from (or equal to) gamePID. Used by cpu.Isolator.
func FindGameProcesses(gamePID int) []int {
	procs, err := scanProc()
	if err != nil {
		return []int{gamePID}
	}

	// Collect descendant PIDs via BFS.
	visited := map[int]bool{gamePID: true}
	queue := []int{gamePID}
	result := []int{gamePID}

	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, p := range procs {
			if p.ppid == parent && !visited[p.pid] {
				visited[p.pid] = true
				result = append(result, p.pid)
				queue = append(queue, p.pid)
			}
		}
	}
	return result
}
