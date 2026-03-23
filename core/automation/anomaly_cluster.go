// core/automation/anomaly_cluster.go — Correlated signal clustering.
//
// Groups threat engine signals that fire within a short time window into
// semantic clusters. A cluster is "hot" when 2+ distinct signal types fire
// within clusterWindow seconds. Pattern classification maps signal combinations
// to known attack patterns (escalation, lateral movement, exfil prep, etc.)
//
// This is a lightweight density-based approach — no ML library required.
package automation

import (
	"fmt"
	"sync"
	"time"
)

const (
	clusterWindow = 60 * time.Second // signals within this window form a cluster
	hotThreshold  = 2               // minimum distinct signals to form a hot cluster
)

// SignalEvent is a single named threat signal observation with its score.
type SignalEvent struct {
	Name  string
	Score float64
	At    time.Time
}

// Cluster represents a group of correlated signals.
type Cluster struct {
	ID       string
	Signals  []string // distinct signal names in this cluster
	Strength float64  // weighted average score of signals
	Pattern  string   // detected attack pattern
	Hot      bool     // true if ≥ hotThreshold distinct signals
	FirstSeen time.Time
	LastSeen  time.Time
}

// AnomalyCluster records signal events and identifies correlated clusters.
type AnomalyCluster struct {
	mu      sync.Mutex
	history []SignalEvent
}

// NewAnomalyCluster creates an AnomalyCluster.
func NewAnomalyCluster() *AnomalyCluster {
	return &AnomalyCluster{}
}

// Record adds a new signal observation.
func (ac *AnomalyCluster) Record(name string, score float64) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	now := time.Now()
	ac.history = append(ac.history, SignalEvent{Name: name, Score: score, At: now})
	// Prune events older than 2× window to bound memory.
	cutoff := now.Add(-2 * clusterWindow)
	for len(ac.history) > 0 && ac.history[0].At.Before(cutoff) {
		ac.history = ac.history[1:]
	}
}

// ActiveClusters returns clusters formed from events within the last clusterWindow.
// Each cluster groups events that overlap within the same time window.
// Only clusters with ≥ hotThreshold distinct signals are marked Hot.
func (ac *AnomalyCluster) ActiveClusters() []Cluster {
	ac.mu.Lock()
	defer ac.mu.Unlock()

	cutoff := time.Now().Add(-clusterWindow)
	var recent []SignalEvent
	for _, e := range ac.history {
		if e.At.After(cutoff) {
			recent = append(recent, e)
		}
	}
	if len(recent) == 0 {
		return nil
	}

	// Collect distinct signal names and scores.
	seen := make(map[string]float64)
	var first, last time.Time
	for i, e := range recent {
		if existing, ok := seen[e.Name]; !ok || e.Score > existing {
			seen[e.Name] = e.Score
		}
		if i == 0 {
			first = e.At
		}
		last = e.At
	}

	if len(seen) == 0 {
		return nil
	}

	// Build single cluster (all recent signals are one temporal cluster).
	signals := make([]string, 0, len(seen))
	var totalScore float64
	for name, score := range seen {
		signals = append(signals, name)
		totalScore += score
	}
	strength := totalScore / float64(len(seen))

	pattern := classifyPattern(signals, strength)
	hot := len(signals) >= hotThreshold

	return []Cluster{{
		ID:        fmt.Sprintf("cl-%d", first.UnixNano()%1_000_000),
		Signals:   signals,
		Strength:  strength,
		Pattern:   pattern,
		Hot:       hot,
		FirstSeen: first,
		LastSeen:  last,
	}}
}

// classifyPattern maps a set of active signal names to a semantic attack pattern.
// Patterns are matched in priority order; "generic" is the fallback.
func classifyPattern(signals []string, strength float64) string {
	has := func(name string) bool {
		for _, s := range signals {
			if s == name {
				return true
			}
		}
		return false
	}

	switch {
	// Exfil preparation: vault exposed + firewall tampered.
	case has("vault_exposure") && has("firewall_anomaly"):
		return "exfil_prep"

	// Lateral movement: namespace escape + privilege escalation.
	case has("namespace_abuse") && has("privilege_escalation"):
		return "lateral_movement"

	// Persistence + kernel: deep rootkit indicators.
	case has("persistence_signal") && has("kernel_integrity"):
		return "persistence_rootkit"

	// Privilege escalation + kernel: kernel exploit attempt.
	case has("privilege_escalation") && has("kernel_integrity"):
		return "kernel_exploit"

	// High-strength rising: general escalation.
	case strength > 60 && (has("privilege_escalation") || has("firewall_anomaly")):
		return "escalation"

	default:
		return "generic"
	}
}

// HotClusters filters ActiveClusters to only those marked Hot.
func (ac *AnomalyCluster) HotClusters() []Cluster {
	var hot []Cluster
	for _, c := range ac.ActiveClusters() {
		if c.Hot {
			hot = append(hot, c)
		}
	}
	return hot
}
