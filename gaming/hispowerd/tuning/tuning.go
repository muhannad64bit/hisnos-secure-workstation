// tuning/tuning.go — Phase 6: GPU & Scheduler Tuning
//
// Four sub-operations:
//
//   1. CPU Governor → "performance"
//      Writes to /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor.
//      Requires CAP_SYS_ADMIN (root). Fails gracefully on EPERM.
//
//   2. sched_autogroup
//      Writes "1" to /proc/sys/kernel/sched_autogroup_enabled.
//      Requires CAP_SYS_ADMIN. Fails gracefully.
//
//   3. Nice / RT priority for game process
//      Calls setpriority(PRIO_PROCESS, pid, nice) via syscall.
//      Values in [-5, 0] do NOT require CAP_SYS_NICE (user can increase niceness
//      below their current value). -5 is achievable from nice=0 baseline.
//      More negative values require CAP_SYS_NICE.
//
//   4. Gaming environment variables
//      Writes to $XDG_CONFIG_HOME/environment.d/hisnos-gaming.conf
//      (user-owned; systemd reads this for user sessions).
//      Next game launch picks up MANGOHUD=1, DXVK_ASYNC=1, etc.
//
// Restore: undoes governor (→ saved value), nice (→ 0), removes env conf.

package tuning

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"hisnos.local/hispowerd/config"
	"hisnos.local/hispowerd/observe"
)

// Tuner applies gaming performance tuning.
type Tuner struct {
	cfg     *config.Config
	log     *observe.Logger
	mu      sync.Mutex

	savedGovernors map[string]string // cpu path → previous governor
	savedAutogroup string            // previous sched_autogroup value
	envFileWritten bool
	savedNicePIDs  []int // PIDs we reniced
}

// NewTuner creates a Tuner.
func NewTuner(cfg *config.Config, log *observe.Logger) *Tuner {
	return &Tuner{
		cfg:            cfg,
		log:            log,
		savedGovernors: make(map[string]string),
	}
}

// Apply applies all tuning for the given game PID.
// Partial success is acceptable — each sub-operation is independent.
func (t *Tuner) Apply(gamePIDs []int) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.savedGovernors = make(map[string]string)
	t.savedAutogroup = ""
	t.envFileWritten = false
	t.savedNicePIDs = nil

	// 1. CPU governor → performance.
	t.applyGovernor()

	// 2. sched_autogroup.
	t.applyAutogroup()

	// 3. Renice game processes.
	t.applyNice(gamePIDs)

	// 4. Inject environment variables.
	t.applyEnvVars()

	return nil // all sub-operations log their own errors
}

// Restore reverts all tuning changes.
func (t *Tuner) Restore(gamePIDs []int) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// 1. Restore governors.
	for path, gov := range t.savedGovernors {
		if err := writeGovernor(path, gov); err != nil {
			t.log.Warn("tuning: restore governor %s → %s: %v", path, gov, err)
		} else {
			t.log.Info("tuning: governor %s → %s", filepath.Base(filepath.Dir(path)), gov)
		}
	}
	t.savedGovernors = make(map[string]string)

	// 2. Restore autogroup.
	if t.savedAutogroup != "" {
		if err := os.WriteFile("/proc/sys/kernel/sched_autogroup_enabled",
			[]byte(t.savedAutogroup+"\n"), 0644); err != nil {
			t.log.Warn("tuning: restore autogroup: %v", err)
		}
		t.savedAutogroup = ""
	}

	// 3. Restore nice (→ 0).
	for _, pid := range t.savedNicePIDs {
		if err := setNice(pid, 0); err != nil {
			t.log.Info("tuning: restore nice pid=%d: %v (may have exited)", pid, err)
		}
	}
	t.savedNicePIDs = nil

	// 4. Remove env conf.
	if t.envFileWritten {
		t.removeEnvVars()
		t.envFileWritten = false
	}

	return nil
}

// ─── sub-operations ──────────────────────────────────────────────────────────

func (t *Tuner) applyGovernor() {
	govPaths, err := filepath.Glob("/sys/devices/system/cpu/cpu*/cpufreq/scaling_governor")
	if err != nil || len(govPaths) == 0 {
		t.log.Warn("tuning: no cpufreq governors found — CPU frequency scaling unavailable")
		return
	}
	for _, path := range govPaths {
		prev, err := readGovernor(path)
		if err != nil {
			t.log.Warn("tuning: read governor %s: %v", path, err)
			continue
		}
		if err := writeGovernor(path, t.cfg.CPUGovernor); err != nil {
			if isPermError(err) {
				t.log.Warn("tuning: governor requires CAP_SYS_ADMIN: %v", err)
				return // same error for all CPUs
			}
			t.log.Warn("tuning: write governor %s: %v", path, err)
			continue
		}
		t.savedGovernors[path] = prev
	}
	if len(t.savedGovernors) > 0 {
		t.log.Info("tuning: CPU governor → %s (%d CPUs)", t.cfg.CPUGovernor, len(t.savedGovernors))
	}
}

func (t *Tuner) applyAutogroup() {
	const path = "/proc/sys/kernel/sched_autogroup_enabled"
	prev, err := os.ReadFile(path)
	if err != nil {
		t.log.Info("tuning: sched_autogroup not available: %v", err)
		return
	}
	t.savedAutogroup = strings.TrimSpace(string(prev))
	if err := os.WriteFile(path, []byte("1\n"), 0644); err != nil {
		if isPermError(err) {
			t.log.Warn("tuning: sched_autogroup requires CAP_SYS_ADMIN: %v", err)
		} else {
			t.log.Warn("tuning: sched_autogroup: %v", err)
		}
		t.savedAutogroup = "" // nothing to restore
		return
	}
	t.log.Info("tuning: sched_autogroup enabled")
}

func (t *Tuner) applyNice(pids []int) {
	if len(pids) == 0 || t.cfg.GameNiceValue >= 0 {
		return
	}
	for _, pid := range pids {
		if err := setNice(pid, t.cfg.GameNiceValue); err != nil {
			t.log.Warn("tuning: nice pid=%d to %d: %v", pid, t.cfg.GameNiceValue, err)
			continue
		}
		t.savedNicePIDs = append(t.savedNicePIDs, pid)
	}
	if len(t.savedNicePIDs) > 0 {
		t.log.Info("tuning: reniced %d game process(es) to nice=%d", len(t.savedNicePIDs), t.cfg.GameNiceValue)
	}
}

func (t *Tuner) applyEnvVars() {
	if !t.cfg.InjectEnvVars || len(t.cfg.GameEnvVars) == 0 {
		return
	}
	confDir := filepath.Join(
		envOr("XDG_CONFIG_HOME", filepath.Join(envOr("HOME", "/root"), ".config")),
		"environment.d",
	)
	if err := os.MkdirAll(confDir, 0700); err != nil {
		t.log.Warn("tuning: env.d mkdir: %v", err)
		return
	}
	confPath := filepath.Join(confDir, "hisnos-gaming.conf")
	var sb strings.Builder
	sb.WriteString("# HisnOS gaming performance environment variables\n")
	sb.WriteString("# Written by hispowerd — removed on session end\n")
	for _, env := range t.cfg.GameEnvVars {
		sb.WriteString(env + "\n")
	}
	if err := os.WriteFile(confPath, []byte(sb.String()), 0600); err != nil {
		t.log.Warn("tuning: write env conf: %v", err)
		return
	}
	t.envFileWritten = true
	t.log.Info("tuning: wrote gaming env vars to %s", confPath)
}

func (t *Tuner) removeEnvVars() {
	confPath := filepath.Join(
		envOr("XDG_CONFIG_HOME", filepath.Join(envOr("HOME", "/root"), ".config")),
		"environment.d",
		"hisnos-gaming.conf",
	)
	if err := os.Remove(confPath); err != nil && !os.IsNotExist(err) {
		t.log.Warn("tuning: remove env conf %s: %v", confPath, err)
	} else {
		t.log.Info("tuning: removed gaming env conf")
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func readGovernor(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func writeGovernor(path, governor string) error {
	return os.WriteFile(path, []byte(governor+"\n"), 0644)
}

func setNice(pid, nice int) error {
	// syscall.Setpriority(which, who, prio)
	// which=PRIO_PROCESS=0, who=pid, prio=nice value
	return syscall.Setpriority(syscall.PRIO_PROCESS, pid, nice)
}

func isPermError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "operation not permitted") ||
		strings.Contains(s, "permission denied")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// AppliedSummary returns a human-readable summary of what was tuned (for journald events).
func (t *Tuner) AppliedSummary() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	parts := []string{}
	if len(t.savedGovernors) > 0 {
		parts = append(parts, fmt.Sprintf("governor=%s", t.cfg.CPUGovernor))
	}
	if t.savedAutogroup != "" {
		parts = append(parts, "autogroup=1")
	}
	if len(t.savedNicePIDs) > 0 {
		parts = append(parts, fmt.Sprintf("nice=%d", t.cfg.GameNiceValue))
	}
	if t.envFileWritten {
		parts = append(parts, "env_vars=injected")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}
