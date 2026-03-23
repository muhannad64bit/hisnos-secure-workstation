// core/automation/temporal_cluster.go — Temporal attack session tracker.
//
// Groups related security signals into attack sessions based on time proximity.
// A session starts when the first signal arrives and remains open while new
// signals arrive within sessionInactivityTimeout of the previous one.
// Sessions that exceed sessionMaxDuration are forcibly closed.
//
// Each session accumulates:
//   - distinct signal types seen
//   - escalation count (number of signals above escalationScoreThreshold)
//   - composite session risk score
//
// Risk score amplification:
//   - Base: arithmetic mean of individual signal scores
//   - +5 per escalation event within the session
//   - Capped at 100
//
// Pattern classification maps the set of distinct signal types to a named
// attack pattern (lateral_movement, kernel_exploit, exfil_prep,
// persistence_rootkit, escalation, generic). See classifyPattern() in
// anomaly_cluster.go — shared by both cluster types.
//
// A session is considered "hot" if:
//   - It is still open (last signal < inactivity timeout)
//   - It has ≥ 2 distinct signal types
//   - Its composite risk score ≥ hotSessionMinScore
//
// State is in-memory only (sessions are transient; baselines are separate).
package automation

import (
	"log"
	"sync"
	"time"
)

const (
	sessionInactivityTimeout = 2 * time.Minute
	sessionMaxDuration       = 10 * time.Minute
	hotSessionMinScore       = 50.0
	escalationScoreThreshold = 70.0 // signal score ≥ this counts as escalation
)

// SessionSignal is one security signal fed into the tracker.
type SessionSignal struct {
	Type  string  // e.g. "rt_escalation", "namespace_anomaly", "threat_score_spike"
	Score float64 // 0–100
}

// AttackSession represents an ongoing correlated attack sequence.
type AttackSession struct {
	ID            string
	StartedAt     time.Time
	LastSignalAt  time.Time
	Signals       []SessionSignal
	DistinctTypes map[string]bool
	Escalations   int
	RiskScore     float64
	Pattern       string
	Closed        bool
}

// TemporalClusterTracker groups signals into attack sessions.
type TemporalClusterTracker struct {
	mu       sync.Mutex
	sessions []*AttackSession
	nextID   int

	emit func(category, event string, data map[string]any)
}

// NewTemporalClusterTracker creates a tracker ready to receive signals.
func NewTemporalClusterTracker(emit func(string, string, map[string]any)) *TemporalClusterTracker {
	if emit == nil {
		emit = func(_, _ string, _ map[string]any) {}
	}
	return &TemporalClusterTracker{emit: emit}
}

// Ingest feeds one security signal into the tracker.
// It finds (or opens) the appropriate session and updates its state.
// Returns the session ID the signal was assigned to.
func (t *TemporalClusterTracker) Ingest(sig SessionSignal) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	t.closeStaleSessions(now)

	// Find the most recent open session.
	sess := t.findOpenSession(now)
	if sess == nil {
		t.nextID++
		sess = &AttackSession{
			ID:            formatSessionID(t.nextID),
			StartedAt:     now,
			LastSignalAt:  now,
			DistinctTypes: make(map[string]bool),
		}
		t.sessions = append(t.sessions, sess)
		log.Printf("[temporal] new session %s opened", sess.ID)
		t.emit("automation", "attack_session_opened", map[string]any{
			"session_id": sess.ID,
		})
	}

	sess.Signals = append(sess.Signals, sig)
	sess.LastSignalAt = now
	sess.DistinctTypes[sig.Type] = true
	if sig.Score >= escalationScoreThreshold {
		sess.Escalations++
	}

	sess.RiskScore = t.computeRiskScore(sess)
	sess.Pattern = classifyPattern(keysOf(sess.DistinctTypes))

	log.Printf("[temporal] session %s signal=%s score=%.1f risk=%.1f pattern=%s",
		sess.ID, sig.Type, sig.Score, sess.RiskScore, sess.Pattern)

	return sess.ID
}

// HotSessions returns all currently hot open sessions.
func (t *TemporalClusterTracker) HotSessions() []*AttackSession {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeStaleSessions(time.Now())
	var hot []*AttackSession
	for _, s := range t.sessions {
		if !s.Closed && len(s.DistinctTypes) >= 2 && s.RiskScore >= hotSessionMinScore {
			hot = append(hot, s)
		}
	}
	return hot
}

// Status returns an IPC-ready summary.
func (t *TemporalClusterTracker) Status() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeStaleSessions(time.Now())
	var open, hot int
	for _, s := range t.sessions {
		if !s.Closed {
			open++
			if len(s.DistinctTypes) >= 2 && s.RiskScore >= hotSessionMinScore {
				hot++
			}
		}
	}
	return map[string]any{
		"total_sessions": len(t.sessions),
		"open_sessions":  open,
		"hot_sessions":   hot,
	}
}

// Purge removes closed sessions older than maxAge to bound memory usage.
func (t *TemporalClusterTracker) Purge(maxAge time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	kept := t.sessions[:0]
	for _, s := range t.sessions {
		if !s.Closed || s.LastSignalAt.After(cutoff) {
			kept = append(kept, s)
		}
	}
	t.sessions = kept
}

// ─── internal helpers ────────────────────────────────────────────────────────

// computeRiskScore calculates the amplified session risk score.
// Must be called with mu held.
func (t *TemporalClusterTracker) computeRiskScore(sess *AttackSession) float64 {
	if len(sess.Signals) == 0 {
		return 0
	}
	var sum float64
	for _, sig := range sess.Signals {
		sum += sig.Score
	}
	base := sum / float64(len(sess.Signals))
	// Amplify by escalation count.
	score := base + float64(sess.Escalations)*5
	if score > 100 {
		score = 100
	}
	return score
}

// findOpenSession returns the most recent open session within inactivity window.
// Must be called with mu held.
func (t *TemporalClusterTracker) findOpenSession(now time.Time) *AttackSession {
	var best *AttackSession
	for _, s := range t.sessions {
		if s.Closed {
			continue
		}
		if now.Sub(s.LastSignalAt) > sessionInactivityTimeout {
			continue
		}
		if now.Sub(s.StartedAt) > sessionMaxDuration {
			continue
		}
		if best == nil || s.LastSignalAt.After(best.LastSignalAt) {
			best = s
		}
	}
	return best
}

// closeStaleSessions marks sessions as closed when they exceed time limits.
// Must be called with mu held.
func (t *TemporalClusterTracker) closeStaleSessions(now time.Time) {
	for _, s := range t.sessions {
		if s.Closed {
			continue
		}
		inactive := now.Sub(s.LastSignalAt) > sessionInactivityTimeout
		tooLong := now.Sub(s.StartedAt) > sessionMaxDuration
		if inactive || tooLong {
			s.Closed = true
			reason := "inactivity"
			if tooLong {
				reason = "max_duration"
			}
			log.Printf("[temporal] session %s closed (%s) risk=%.1f signals=%d",
				s.ID, reason, s.RiskScore, len(s.Signals))
			t.emit("automation", "attack_session_closed", map[string]any{
				"session_id":  s.ID,
				"reason":      reason,
				"risk_score":  s.RiskScore,
				"pattern":     s.Pattern,
				"signal_count": len(s.Signals),
				"duration_s":  int(s.LastSignalAt.Sub(s.StartedAt).Seconds()),
			})
		}
	}
}

// keysOf returns sorted keys of a string→bool map.
func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// sortStrings is a minimal insertion sort for small slices (no sort import needed
// here since sort is already imported in anomaly_cluster.go for the package).
func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		key := ss[i]
		j := i - 1
		for j >= 0 && ss[j] > key {
			ss[j+1] = ss[j]
			j--
		}
		ss[j+1] = key
	}
}

func formatSessionID(n int) string {
	s := "000" + intToStr(n)
	return "sess-" + s[len(s)-4:]
}
