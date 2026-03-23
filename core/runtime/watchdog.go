// core/runtime/watchdog.go
//
// Watchdog supervises registered subsystems via a 5-second heartbeat protocol.
//
// Escalation ladder per subsystem (triggered by consecutive missed heartbeats):
//
//   Level 0 (OK)       — heartbeat received within SLA
//   Level 1 (WARN)     — 1 missed heartbeat → log warning
//   Level 2 (RESTART)  — 2 consecutive missed → attempt restart
//   Level 3 (CIRCUIT)  — 3 restarts within 60s → open circuit breaker,
//                         stop auto-restart, emit SubsystemFailing event
//   Level 4 (SAFE)     — critical subsystem flapping → emit SafeModeCandidate
//   Level 5 (ALERT)    — write operator alert to /var/lib/hisnos/operator-alert.json
//
// Circuit breaker: stays open for circuitCooldown (5 min).  After cooldown,
// the circuit closes and restarts are attempted again.
//
// Each subsystem sends a heartbeat by calling Beat() on its *SubsystemHandle.
// The watchdog checks all registered subsystems every heartbeatPeriod (5s).

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	heartbeatPeriod = 5 * time.Second
	circuitWindow   = 60 * time.Second
	circuitThresh   = 3  // restarts within circuitWindow
	circuitCooldown = 5 * time.Minute
	alertFile       = "/var/lib/hisnos/operator-alert.json"
)

// ── Subsystem registration ────────────────────────────────────────────────

// SubsystemSpec describes a watchdog-managed subsystem.
type SubsystemSpec struct {
	Name     string
	SLA      time.Duration           // max time between heartbeats before warning
	Critical bool                    // critical subsystems escalate to safe-mode
	Restart  func() error            // called on level-2 escalation; nil = no restart
	OnFail   func(name string)       // called when circuit trips; nil = no-op
}

// SubsystemHandle is returned to the registered goroutine.
// Call Beat() periodically to signal liveness.
type SubsystemHandle struct {
	name string
	ch   chan struct{}
}

// Beat signals to the watchdog that the subsystem is alive.
// Non-blocking — drops signal if the channel is full (watchdog is busy).
func (h *SubsystemHandle) Beat() {
	select {
	case h.ch <- struct{}{}:
	default:
	}
}

// ── Watchdog ──────────────────────────────────────────────────────────────

// Watchdog is the centralised heartbeat supervisor.
type Watchdog struct {
	mu      sync.Mutex
	entries map[string]*wdEntry

	// External callbacks.
	onSafeModeCandidate func(reason string)
}

type wdEntry struct {
	spec    SubsystemSpec
	handle  *SubsystemHandle
	lastBeat time.Time

	// Escalation counters.
	missedBeats  int
	restartTimes []time.Time  // ring of recent restart timestamps
	circuitOpen  bool
	circuitOpenAt time.Time

	// Restart backoff.
	restartBackoff time.Duration
}

// NewWatchdog creates a Watchdog.
// onSafeModeCandidate is called when a critical subsystem trips its circuit breaker.
func NewWatchdog(onSafeModeCandidate func(reason string)) *Watchdog {
	return &Watchdog{
		entries:             make(map[string]*wdEntry),
		onSafeModeCandidate: onSafeModeCandidate,
	}
}

// Register adds a subsystem to the watchdog.
// Returns a SubsystemHandle the subsystem uses to send heartbeats.
func (wd *Watchdog) Register(spec SubsystemSpec) *SubsystemHandle {
	wd.mu.Lock()
	defer wd.mu.Unlock()

	ch := make(chan struct{}, 4) // buffered to absorb bursts
	handle := &SubsystemHandle{name: spec.Name, ch: ch}

	wd.entries[spec.Name] = &wdEntry{
		spec:           spec,
		handle:         handle,
		lastBeat:       time.Now(), // grace period on registration
		restartBackoff: 5 * time.Second,
	}

	log.Printf("[watchdog] registered subsystem %q (SLA=%s critical=%v)",
		spec.Name, spec.SLA, spec.Critical)
	return handle
}

// Run starts the watchdog loop. Blocks until ctx is cancelled.
func (wd *Watchdog) Run(ctx context.Context) {
	ticker := time.NewTicker(heartbeatPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			wd.tick()
		}
	}
}

// tick processes all heartbeat channels and runs escalation logic.
func (wd *Watchdog) tick() {
	wd.mu.Lock()
	defer wd.mu.Unlock()

	now := time.Now()

	for _, e := range wd.entries {
		// Drain all pending heartbeats from the channel.
		drained := false
		for {
			select {
			case <-e.handle.ch:
				e.lastBeat = now
				drained = true
			default:
				goto done
			}
		}
	done:
		_ = drained

		// Check circuit breaker cooldown.
		if e.circuitOpen && now.Sub(e.circuitOpenAt) >= circuitCooldown {
			log.Printf("[watchdog] circuit CLOSED for %q (cooldown elapsed)", e.spec.Name)
			e.circuitOpen = false
			e.restartTimes = nil
			e.missedBeats = 0
		}

		// Measure time since last heartbeat.
		age := now.Sub(e.lastBeat)
		if age <= e.spec.SLA {
			// Level 0: healthy.
			if e.missedBeats > 0 {
				log.Printf("[watchdog] %q recovered (age=%s)", e.spec.Name, age)
				e.missedBeats = 0
				e.restartBackoff = 5 * time.Second
			}
			continue
		}

		e.missedBeats++

		switch {
		case e.missedBeats == 1:
			// Level 1: warn.
			log.Printf("[watchdog] WARN: %q missed heartbeat (SLA=%s age=%s)",
				e.spec.Name, e.spec.SLA, age.Round(time.Millisecond))

		case e.missedBeats >= 2 && !e.circuitOpen && e.spec.Restart != nil:
			// Level 2: attempt restart.
			recentRestarts := countRecent(e.restartTimes, now, circuitWindow)
			if recentRestarts >= circuitThresh {
				// Level 3: circuit breaker.
				e.circuitOpen = true
				e.circuitOpenAt = now
				log.Printf("[watchdog] CIRCUIT OPEN for %q (%d restarts in %s)",
					e.spec.Name, recentRestarts, circuitWindow)

				if e.spec.OnFail != nil {
					go e.spec.OnFail(e.spec.Name)
				}

				if e.spec.Critical && wd.onSafeModeCandidate != nil {
					// Level 4: safe-mode candidate.
					reason := fmt.Sprintf("watchdog: critical subsystem %q circuit open", e.spec.Name)
					go wd.onSafeModeCandidate(reason)
				}

				// Level 5: write operator alert.
				go writeOperatorAlert(e.spec.Name, recentRestarts)
			} else {
				// Attempt restart with backoff.
				log.Printf("[watchdog] restarting %q (missed=%d backoff=%s)",
					e.spec.Name, e.missedBeats, e.restartBackoff)
				e.restartTimes = append(e.restartTimes, now)
				backoff := e.restartBackoff
				restartFn := e.spec.Restart
				name := e.spec.Name
				go func() {
					time.Sleep(backoff)
					if err := restartFn(); err != nil {
						log.Printf("[watchdog] restart %q failed: %v", name, err)
					} else {
						log.Printf("[watchdog] restart %q succeeded", name)
					}
				}()
				// Exponential backoff: cap at 60s.
				e.restartBackoff *= 2
				if e.restartBackoff > 60*time.Second {
					e.restartBackoff = 60 * time.Second
				}
			}
		}
	}
}

// Status returns a snapshot of all subsystem health statuses.
func (wd *Watchdog) Status() []SubsystemStatus {
	wd.mu.Lock()
	defer wd.mu.Unlock()

	now := time.Now()
	out := make([]SubsystemStatus, 0, len(wd.entries))
	for _, e := range wd.entries {
		out = append(out, SubsystemStatus{
			Name:          e.spec.Name,
			Alive:         now.Sub(e.lastBeat) <= e.spec.SLA,
			LastHeartbeat: e.lastBeat,
			Restarts:      len(e.restartTimes),
			CircuitOpen:   e.circuitOpen,
		})
	}
	return out
}

// ── Helpers ───────────────────────────────────────────────────────────────

func countRecent(times []time.Time, now time.Time, window time.Duration) int {
	cutoff := now.Add(-window)
	n := 0
	for _, t := range times {
		if t.After(cutoff) {
			n++
		}
	}
	return n
}

// writeOperatorAlert writes a JSON alert file for out-of-band operator notification.
func writeOperatorAlert(subsystem string, restarts int) {
	alert := map[string]any{
		"timestamp":  time.Now().UTC().Format(time.RFC3339),
		"type":       "subsystem_circuit_open",
		"subsystem":  subsystem,
		"restarts":   restarts,
		"action":     "manual intervention required",
	}
	data, _ := json.MarshalIndent(alert, "", "  ")
	if err := os.MkdirAll(filepath.Dir(alertFile), 0750); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(alertFile), ".alert-*.tmp")
	if err != nil {
		return
	}
	tmp.Write(data)
	tmp.Sync()
	tmp.Close()
	os.Rename(tmp.Name(), alertFile)
	log.Printf("[watchdog] operator alert written: %s", alertFile)
}
