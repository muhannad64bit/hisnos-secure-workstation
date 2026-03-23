// core/supervisor/self_healer.go — Self-healing supervisor extension.
//
// Augments the existing supervisor with:
//
//  1. Exponential backoff restart policy for repeatedly-failing services.
//     The backoff sequence: 1s, 2s, 4s, 8s, 16s, 30s (capped).
//     After maxRestartAttempts consecutive failures, the service is considered
//     permanently failed and triggers safe-mode evaluation.
//
//  2. Correlated failure detection.
//     If ≥ correlatedFailureThreshold distinct services fail within
//     correlatedFailureWindow, the SelfHealer fires the onCorrelatedFailure
//     callback (typically → safe-mode escalation).
//
//  3. Health check probe support.
//     Optional per-service health check function. If the probe fails, the
//     service is restarted even if it hasn't exited.
//
// The SelfHealer is designed to sit alongside the existing Supervisor (not
// replace it). It receives RestartNotify(name, err) calls from the Supervisor
// when a service exits and decides whether to restart, back off, or escalate.
//
// Integration: in hisnosd main.go:
//
//	healer := supervisor.NewSelfHealer(onCorrelated, emit)
//	healer.Register("threat-engine", restartFn, nil)
//	// Supervisor calls healer.RestartNotify(name, err) in its loop.
package supervisor

import (
	"log"
	"sync"
	"time"
)

const (
	maxRestartAttempts         = 6
	correlatedFailureThreshold = 3
	correlatedFailureWindow    = 2 * time.Minute
	backoffCap                 = 30 * time.Second
)

// backoffSequence defines restart delays in seconds.
var backoffSequence = []time.Duration{1, 2, 4, 8, 16, 30}

func backoffFor(attempt int) time.Duration {
	if attempt >= len(backoffSequence) {
		return backoffCap
	}
	return backoffSequence[attempt] * time.Second
}

// serviceRecord tracks restart state for one service.
type serviceRecord struct {
	Name        string
	Restart     func() error
	HealthCheck func() error // nil = no probe
	Attempts    int          // consecutive failures
	LastFail    time.Time
	Disabled    bool // set when maxRestartAttempts exceeded
}

// SelfHealer manages exponential backoff and correlated failure detection.
type SelfHealer struct {
	mu       sync.Mutex
	services map[string]*serviceRecord
	// ring of recent failure timestamps for correlation detection
	failureRing     [16]time.Time
	failureRingHead int

	onCorrelatedFailure func(failedServices []string)
	emit                func(category, event string, data map[string]any)
}

// NewSelfHealer creates a healer with a correlated failure callback.
func NewSelfHealer(
	onCorrelatedFailure func(failedServices []string),
	emit func(string, string, map[string]any),
) *SelfHealer {
	if emit == nil {
		emit = func(_, _ string, _ map[string]any) {}
	}
	return &SelfHealer{
		services:            make(map[string]*serviceRecord),
		onCorrelatedFailure: onCorrelatedFailure,
		emit:                emit,
	}
}

// Register adds a service to the self-healer.
// restart must safely start the service (idempotent).
// healthCheck may be nil.
func (sh *SelfHealer) Register(name string, restart func() error, healthCheck func() error) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.services[name] = &serviceRecord{
		Name:        name,
		Restart:     restart,
		HealthCheck: healthCheck,
	}
	log.Printf("[self-healer] registered %q", name)
}

// RestartNotify is called by the supervisor when a service exits unexpectedly.
// It schedules a backoff restart and checks for correlated failures.
func (sh *SelfHealer) RestartNotify(name string, exitErr error) {
	sh.mu.Lock()
	svc, ok := sh.services[name]
	if !ok {
		sh.mu.Unlock()
		return
	}
	if svc.Disabled {
		sh.mu.Unlock()
		log.Printf("[self-healer] %q is disabled (max restarts exceeded)", name)
		return
	}

	svc.Attempts++
	svc.LastFail = time.Now()
	attempt := svc.Attempts
	delay := backoffFor(attempt - 1)

	// Record failure for correlation detection.
	sh.recordFailure(time.Now())
	correlated := sh.detectCorrelation()
	sh.mu.Unlock()

	sh.emit("health", "service_failed", map[string]any{
		"service": name, "attempt": attempt,
		"backoff_s": delay.Seconds(), "error": errStr(exitErr),
	})

	if attempt > maxRestartAttempts {
		sh.mu.Lock()
		svc.Disabled = true
		sh.mu.Unlock()
		log.Printf("[self-healer] %q exceeded max restarts (%d) — disabling", name, maxRestartAttempts)
		sh.emit("health", "service_permanently_failed", map[string]any{
			"service": name, "attempts": attempt,
		})
		if sh.onCorrelatedFailure != nil {
			sh.onCorrelatedFailure([]string{name})
		}
		return
	}

	if correlated != nil && sh.onCorrelatedFailure != nil {
		log.Printf("[self-healer] correlated failure detected: %v", correlated)
		sh.emit("health", "safe_mode_triggered", map[string]any{
			"reason": "correlated_failure", "services": correlated,
		})
		sh.onCorrelatedFailure(correlated)
		return
	}

	log.Printf("[self-healer] restarting %q in %v (attempt %d/%d)", name, delay, attempt, maxRestartAttempts)
	go sh.restartAfterDelay(svc, delay, attempt)
}

// ProbeAll runs health checks for all registered services with probes.
// Call periodically from the supervisor loop (e.g. every 10 s).
func (sh *SelfHealer) ProbeAll() {
	sh.mu.Lock()
	services := make([]*serviceRecord, 0, len(sh.services))
	for _, svc := range sh.services {
		if svc.HealthCheck != nil && !svc.Disabled {
			services = append(services, svc)
		}
	}
	sh.mu.Unlock()

	for _, svc := range services {
		if err := svc.HealthCheck(); err != nil {
			log.Printf("[self-healer] health check failed for %q: %v", svc.Name, err)
			sh.RestartNotify(svc.Name, err)
		}
	}
}

// Reset clears restart counters for a service (call after successful start).
func (sh *SelfHealer) Reset(name string) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if svc, ok := sh.services[name]; ok {
		svc.Attempts = 0
		svc.Disabled = false
	}
}

// Status returns IPC-ready healer state.
func (sh *SelfHealer) Status() map[string]any {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	services := make([]map[string]any, 0, len(sh.services))
	for _, svc := range sh.services {
		services = append(services, map[string]any{
			"name":     svc.Name,
			"attempts": svc.Attempts,
			"disabled": svc.Disabled,
		})
	}
	return map[string]any{
		"services":                   services,
		"max_restart_attempts":       maxRestartAttempts,
		"correlated_failure_window_s": int(correlatedFailureWindow.Seconds()),
	}
}

// ─── internal ───────────────────────────────────────────────────────────────

func (sh *SelfHealer) restartAfterDelay(svc *serviceRecord, delay time.Duration, attempt int) {
	time.Sleep(delay)
	sh.mu.Lock()
	if svc.Disabled {
		sh.mu.Unlock()
		return
	}
	restartFn := svc.Restart
	sh.mu.Unlock()

	if err := restartFn(); err != nil {
		log.Printf("[self-healer] restart of %q failed (attempt %d): %v", svc.Name, attempt, err)
		sh.emit("health", "service_restarted", map[string]any{
			"service": svc.Name, "attempt": attempt, "success": false,
		})
	} else {
		log.Printf("[self-healer] restarted %q (attempt %d)", svc.Name, attempt)
		sh.emit("health", "service_restarted", map[string]any{
			"service": svc.Name, "attempt": attempt, "success": true,
		})
		sh.Reset(svc.Name)
	}
}

// recordFailure adds a timestamp to the failure ring. Must be called with mu held.
func (sh *SelfHealer) recordFailure(t time.Time) {
	sh.failureRing[sh.failureRingHead] = t
	sh.failureRingHead = (sh.failureRingHead + 1) % len(sh.failureRing)
}

// detectCorrelation returns names of services that failed within the window,
// or nil if not enough failures. Must be called with mu held.
func (sh *SelfHealer) detectCorrelation() []string {
	cutoff := time.Now().Add(-correlatedFailureWindow)
	recent := 0
	for _, t := range sh.failureRing {
		if !t.IsZero() && t.After(cutoff) {
			recent++
		}
	}
	if recent < correlatedFailureThreshold {
		return nil
	}
	// Collect names of recently-failed services.
	var failed []string
	for _, svc := range sh.services {
		if !svc.LastFail.IsZero() && svc.LastFail.After(cutoff) {
			failed = append(failed, svc.Name)
		}
	}
	return failed
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
