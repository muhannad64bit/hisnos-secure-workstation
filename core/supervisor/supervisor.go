// core/supervisor/supervisor.go — Periodic subsystem health supervisor.
//
// The Supervisor runs every 15 seconds and checks whether each critical
// HisnOS service is alive. On failure it:
//   1. Updates the state manager (SubsystemState fields).
//   2. Emits a SubsystemCrashed event on the bus.
//   3. Attempts a service restart with configurable exponential backoff.
//   4. If a service crashes more than maxCrashes times within crashWindow,
//      emits a SafeModeEntered event.
//
// The Supervisor also periodically syncs the threat-state.json risk score
// into the authoritative core-state.json (every 30 seconds).

package supervisor

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"hisnos.local/hisnosd/eventbus"
	"hisnos.local/hisnosd/orchestrator"
	"hisnos.local/hisnosd/state"
)

const (
	checkInterval  = 15 * time.Second
	riskSyncInterval = 30 * time.Second
	maxCrashes     = 3
	crashWindow    = 5 * time.Minute
	restartDelay   = 5 * time.Second
	maxRestartDelay = 60 * time.Second
)

// serviceEntry describes a monitored systemd service.
type serviceEntry struct {
	unit        string // systemd unit name
	scope       string // "user" or "system"
	stateField  func(*state.SubsystemState) *bool
	restartable bool // attempt auto-restart on failure

	mu          sync.Mutex
	crashTimes  []time.Time // ring of recent crash timestamps
	restartDelay time.Duration
}

func (e *serviceEntry) isActive() bool {
	args := []string{"is-active", "--quiet", e.unit}
	if e.scope == "user" {
		args = append([]string{"--user"}, args...)
	}
	return exec.Command("/usr/bin/systemctl", args...).Run() == nil
}

func (e *serviceEntry) restart() error {
	args := []string{"restart", e.unit}
	if e.scope == "user" {
		args = append([]string{"--user"}, args...)
	}
	out, err := exec.Command("/usr/bin/systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("restart %s: %w — %s", e.unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// recordCrash appends a crash timestamp and prunes entries outside the window.
// Returns the number of crashes within the window.
func (e *serviceEntry) recordCrash() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	e.crashTimes = append(e.crashTimes, now)
	cutoff := now.Add(-crashWindow)
	n := 0
	for _, t := range e.crashTimes {
		if t.After(cutoff) {
			n++
		}
	}
	// Keep only the last 10 entries to bound memory.
	if len(e.crashTimes) > 10 {
		e.crashTimes = e.crashTimes[len(e.crashTimes)-10:]
	}
	return n
}

// Supervisor orchestrates periodic health checks and subsystem recovery.
type Supervisor struct {
	stateMgr *state.Manager
	bus      *eventbus.Bus
	threat   *orchestrator.ThreatOrchestrator
	services []*serviceEntry
}

// New creates a Supervisor. hisnosDir is used for vault/lab/gaming path construction.
func New(
	mgr *state.Manager,
	bus *eventbus.Bus,
	threat *orchestrator.ThreatOrchestrator,
) *Supervisor {
	// Define monitored services.
	services := []*serviceEntry{
		{
			unit:        "nftables.service",
			scope:       "system",
			restartable: true,
			stateField:  func(s *state.SubsystemState) *bool { return &s.NftablesAlive },
		},
		{
			unit:        "hisnos-logd.service",
			scope:       "user",
			restartable: true,
			stateField:  func(s *state.SubsystemState) *bool { return &s.LogdAlive },
		},
		{
			unit:        "hisnos-threatd.service",
			scope:       "user",
			restartable: true,
			stateField:  func(s *state.SubsystemState) *bool { return &s.ThreatdAlive },
		},
		{
			unit:        "hisnos-dashboard.socket",
			scope:       "user",
			restartable: false, // dashboard is on-demand; don't restart on socket check failure
			stateField:  func(s *state.SubsystemState) *bool { return &s.DashboardAlive },
		},
	}
	return &Supervisor{
		stateMgr: mgr,
		bus:      bus,
		threat:   threat,
		services: services,
	}
}

// Run starts the supervision and risk-sync loops. Blocks until ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) {
	checkTicker := time.NewTicker(checkInterval)
	riskTicker := time.NewTicker(riskSyncInterval)
	defer checkTicker.Stop()
	defer riskTicker.Stop()

	// Run an initial check immediately.
	s.checkAll()
	s.syncRisk()

	for {
		select {
		case <-ctx.Done():
			return
		case <-checkTicker.C:
			s.checkAll()
		case <-riskTicker.C:
			s.syncRisk()
		}
	}
}

// checkAll iterates all service entries and handles failures.
func (s *Supervisor) checkAll() {
	prevState := s.stateMgr.Get()

	for _, svc := range s.services {
		alive := svc.isActive()

		// Update state if changed.
		var wasAlive bool
		_ = s.stateMgr.Update(func(st *state.SystemState) {
			field := svc.stateField(&st.Subsystems)
			wasAlive = *field
			*field = alive

			// Keep firewall state in sync with nftables service.
			if svc.unit == "nftables.service" {
				st.Firewall.Active = alive
			}
		})

		if wasAlive && !alive {
			// Transition: was alive, now dead.
			log.Printf("[hisnosd/supervisor] WARN: %s went offline", svc.unit)
			s.bus.Emit(eventbus.EventSubsystemCrashed, map[string]any{
				"unit":  svc.unit,
				"scope": svc.scope,
			})

			if svc.unit == "nftables.service" {
				s.bus.Emit(eventbus.EventFirewallDead, nil)
			}

			if svc.restartable {
				s.attemptRestart(svc)
			}
		} else if !wasAlive && alive {
			// Transition: was dead, now alive.
			log.Printf("[hisnosd/supervisor] INFO: %s restored", svc.unit)
			s.bus.Emit(eventbus.EventSubsystemRestored, map[string]any{
				"unit":  svc.unit,
			})
		}
	}

	// Check vault mount state from /proc/mounts.
	vaultMounted := orchestrator.IsVaultMounted()
	if vaultMounted != prevState.Vault.Mounted {
		_ = s.stateMgr.Update(func(st *state.SystemState) {
			st.Vault.Mounted = vaultMounted
			if vaultMounted {
				s.bus.Emit(eventbus.EventVaultMounted, nil)
			} else {
				s.bus.Emit(eventbus.EventVaultLocked, nil)
			}
		})
	}
}

// attemptRestart tries to restart a failed service with exponential backoff.
// Runs in a goroutine so it doesn't block the supervision loop.
func (s *Supervisor) attemptRestart(svc *serviceEntry) {
	go func() {
		crashes := svc.recordCrash()
		if crashes > maxCrashes {
			log.Printf("[hisnosd/supervisor] ERR: %s crashed %d times in %s — escalating to safe-mode",
				svc.unit, crashes, crashWindow)
			s.bus.Emit(eventbus.EventSafeModeEntered, map[string]any{
				"reason": fmt.Sprintf("repeated_crashes:%s", svc.unit),
			})
			return
		}

		delay := svc.restartDelay
		if delay == 0 {
			delay = restartDelay
		}
		log.Printf("[hisnosd/supervisor] INFO: restarting %s in %s (attempt %d/%d)",
			svc.unit, delay, crashes, maxCrashes)
		time.Sleep(delay)

		if err := svc.restart(); err != nil {
			log.Printf("[hisnosd/supervisor] WARN: restart %s failed: %v", svc.unit, err)
			// Double the delay for next attempt (cap at maxRestartDelay).
			svc.mu.Lock()
			svc.restartDelay = min(delay*2, maxRestartDelay)
			svc.mu.Unlock()
		} else {
			log.Printf("[hisnosd/supervisor] INFO: %s restarted successfully", svc.unit)
			svc.mu.Lock()
			svc.restartDelay = restartDelay // reset on success
			svc.mu.Unlock()
		}
	}()
}

// syncRisk reads threat-state.json and updates the core risk state.
func (s *Supervisor) syncRisk() {
	snap := s.threat.ReadThreatState()
	prev := s.stateMgr.Get()

	if snap.RiskScore == prev.Risk.Score {
		return // no change
	}

	_ = s.stateMgr.Update(func(st *state.SystemState) {
		st.Risk.Score = snap.RiskScore
		st.Risk.Level = snap.RiskLevel
		if !snap.UpdatedAt.IsZero() {
			// snap.UpdatedAt is a string; parse best-effort.
			st.Risk.LastUpdate = time.Now().UTC()
		}
	})

	s.bus.Emit(eventbus.EventRiskScoreChanged, map[string]any{
		"score": snap.RiskScore,
		"level": snap.RiskLevel,
	})

	log.Printf("[hisnosd/supervisor] INFO: risk score synced: %d (%s)",
		snap.RiskScore, snap.RiskLevel)
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
