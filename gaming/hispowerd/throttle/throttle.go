// throttle/throttle.go — Phase 4: Daemon Throttling Mode
//
// Reduces resource consumption of non-gaming daemons during gaming sessions.
// Three throttle mechanisms:
//
//   1. cgroup cpu.max — reduces CPU quota for daemon cgroups
//      Path: /sys/fs/cgroup/user.slice/user-<uid>.slice/user@<uid>.service/.../<svc>/cpu.max
//      The user owns their own service cgroup subtree → no root required.
//
//   2. Vault idle timer — stops hisnos-vault-idle.timer via systemctl --user
//      Screen-lock watcher (hisnos-vault-watcher.service) is NOT stopped.
//
//   3. threatd slow-mode flag — writes GAMING_SLOW=1 to gaming-state.json
//      threatd reads this flag at its next poll and extends its interval.
//
// All changes are reversible. Restore() undoes everything even on partial failure.

package throttle

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"hisnos.local/hispowerd/config"
	"hisnos.local/hispowerd/observe"
)

// savedCgroup records cpu.max before throttle was applied.
type savedCgroup struct {
	path    string
	service string
	previous string // original cpu.max content
}

// Throttler manages daemon CPU throttling and vault timer suppression.
type Throttler struct {
	cfg       *config.Config
	log       *observe.Logger
	mu        sync.Mutex
	saved     []savedCgroup
	timerStopped bool
}

// NewThrottler creates a Throttler.
func NewThrottler(cfg *config.Config, log *observe.Logger) *Throttler {
	return &Throttler{cfg: cfg, log: log}
}

// Apply throttles all configured daemons and stops the vault idle timer.
func (t *Throttler) Apply() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.saved = t.saved[:0]
	t.timerStopped = false

	var lastErr error

	// 1. Throttle daemons via cgroup cpu.max.
	for _, svc := range t.cfg.ThrottleDaemons {
		if err := t.throttleService(svc); err != nil {
			t.log.Warn("throttle: %s: %v", svc, err)
			lastErr = err
		}
	}

	// 2. Stop vault idle timer (keep watcher alive).
	if err := t.stopVaultTimer(); err != nil {
		t.log.Warn("throttle: vault idle timer: %v", err)
		// Not fatal — vault watcher still prevents exposure.
	} else {
		t.timerStopped = true
	}

	return lastErr
}

// Restore undoes all throttle changes.
func (t *Throttler) Restore() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	var lastErr error

	// 1. Restore cgroup cpu.max.
	for _, s := range t.saved {
		if err := writeCgroupCPUMax(s.path, s.previous); err != nil {
			t.log.Warn("throttle: restore cgroup %s: %v", s.service, err)
			lastErr = err
		} else {
			t.log.Info("throttle: restored %s cpu.max → %s", s.service, s.previous)
		}
	}
	t.saved = t.saved[:0]

	// 2. Restart vault idle timer.
	if t.timerStopped {
		if err := t.startVaultTimer(); err != nil {
			t.log.Warn("throttle: restart vault idle timer: %v", err)
			lastErr = err
		} else {
			t.timerStopped = false
		}
	}

	return lastErr
}

// throttleService finds the cgroup for svc and reduces its cpu.max.
func (t *Throttler) throttleService(svc string) error {
	cgPath, err := findServiceCgroup(svc)
	if err != nil {
		return fmt.Errorf("find cgroup: %w", err)
	}
	if cgPath == "" {
		t.log.Info("throttle: cgroup not found for %s — skipping", svc)
		return nil
	}

	cpuMaxPath := filepath.Join(cgPath, "cpu.max")
	prev, err := readCgroupCPUMax(cpuMaxPath)
	if err != nil {
		return fmt.Errorf("read cpu.max: %w", err)
	}

	// 10% CPU quota: "100000 1000000" (100ms per 1s period = 10%)
	throttledValue := "100000 1000000"
	if err := writeCgroupCPUMax(cpuMaxPath, throttledValue); err != nil {
		return fmt.Errorf("write cpu.max: %w", err)
	}

	t.saved = append(t.saved, savedCgroup{
		path:     cpuMaxPath,
		service:  svc,
		previous: prev,
	})
	t.log.Info("throttle: %s → cpu.max=%s (was: %s)", svc, throttledValue, prev)
	return nil
}

// findServiceCgroup returns the cgroup v2 filesystem path for a user service.
// Returns empty string (not an error) if the service is not running.
func findServiceCgroup(serviceName string) (string, error) {
	uid := strconv.Itoa(os.Getuid())
	// Canonical path in cgroup v2 unified hierarchy for a user service.
	candidates := []string{
		// systemd default location for user services
		filepath.Join("/sys/fs/cgroup/user.slice",
			"user-"+uid+".slice",
			"user@"+uid+".service",
			"app.slice",
			serviceName),
		// Alternative: directly under user@uid.service
		filepath.Join("/sys/fs/cgroup/user.slice",
			"user-"+uid+".slice",
			"user@"+uid+".service",
			serviceName),
	}
	for _, path := range candidates {
		if _, err := os.Stat(filepath.Join(path, "cpu.max")); err == nil {
			return path, nil
		}
	}
	return "", nil // not found (service may not be running)
}

func readCgroupCPUMax(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func writeCgroupCPUMax(path, value string) error {
	return os.WriteFile(path, []byte(value+"\n"), 0644)
}

// stopVaultTimer stops the vault idle timer (screen-lock watcher is separate).
func (t *Throttler) stopVaultTimer() error {
	if t.cfg.VaultIdleTimer == "" {
		return nil
	}
	out, err := exec.Command(
		"/usr/bin/systemctl", "--user", "stop", t.cfg.VaultIdleTimer,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	t.log.Info("throttle: stopped %s", t.cfg.VaultIdleTimer)
	return nil
}

// startVaultTimer restarts the vault idle timer.
func (t *Throttler) startVaultTimer() error {
	if t.cfg.VaultIdleTimer == "" {
		return nil
	}
	out, err := exec.Command(
		"/usr/bin/systemctl", "--user", "start", t.cfg.VaultIdleTimer,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	t.log.Info("throttle: restarted %s", t.cfg.VaultIdleTimer)
	return nil
}
