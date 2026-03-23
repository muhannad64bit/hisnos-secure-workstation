// core/automation/learning_state.go — Adaptive threshold learning and incident history.
//
// Tracks false positive / confirmed alert ratios and adjusts the automation
// trigger threshold over time. Adjustment is damped: at most one change per 2 hours,
// threshold bounded [MinThreshold, MaxThreshold].
//
// Persisted to: /var/lib/hisnos/automation-state.json (atomic rename).
package automation

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	DefaultThreshold = 70.0  // initial trigger threshold (risk score)
	MinThreshold     = 50.0  // floor — never trigger below this
	MaxThreshold     = 85.0  // ceiling — always trigger above this
	thresholdStep    = 2.5   // adjustment per false positive
	confirmStep      = 1.0   // adjustment per confirmed alert
	adjustCooldown   = 2 * time.Hour
	maxIncidents     = 50 // cap stored incident history
)

// Incident records a single automation trigger event and its outcome.
type Incident struct {
	ID          string    `json:"id"`
	Timestamp   time.Time `json:"timestamp"`
	TriggerScore float64   `json:"trigger_score"`
	Trajectory  string    `json:"trajectory"`
	ActionsRun  []string  `json:"actions_run"`
	Confirmed   bool      `json:"confirmed"`   // true = real threat
	FalsePos    bool      `json:"false_positive"` // true = operator override / no threat
}

// LearningState holds the adaptive threshold and incident history.
// It is the only struct in this package that is persisted.
type LearningState struct {
	AlertThreshold   float64    `json:"alert_threshold"`
	FalsePositives   int        `json:"false_positives"`
	ConfirmedAlerts  int        `json:"confirmed_alerts"`
	LastAdjustment   time.Time  `json:"last_adjustment"`
	OverrideCooldown time.Time  `json:"override_cooldown_until"` // suppress automation until this time
	Incidents        []Incident `json:"incidents"`

	stateFile string
	mu        sync.RWMutex
}

// NewLearningState loads persisted state from stateDir or returns defaults.
func NewLearningState(stateDir string) *LearningState {
	ls := &LearningState{
		AlertThreshold: DefaultThreshold,
		stateFile:      filepath.Join(stateDir, "automation-state.json"),
	}
	if err := ls.load(); err != nil {
		log.Printf("[automation/state] starting with defaults: %v", err)
	}
	return ls
}

// Threshold returns the current adaptive alert threshold (thread-safe).
func (ls *LearningState) Threshold() float64 {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	return ls.AlertThreshold
}

// IsSuppressed returns true if the operator has suppressed automation until a future time.
func (ls *LearningState) IsSuppressed() bool {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	return time.Now().Before(ls.OverrideCooldown)
}

// Suppress disables automation triggers for duration.
func (ls *LearningState) Suppress(duration time.Duration) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.OverrideCooldown = time.Now().Add(duration)
	log.Printf("[automation/state] suppressed for %v until %s", duration, ls.OverrideCooldown.Format(time.RFC3339))
	_ = ls.save()
}

// RecordIncident stores a new trigger event.
func (ls *LearningState) RecordIncident(id string, score float64, traj string, actions []string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	inc := Incident{
		ID:           id,
		Timestamp:    time.Now().UTC(),
		TriggerScore: score,
		Trajectory:   traj,
		ActionsRun:   actions,
	}
	ls.Incidents = append(ls.Incidents, inc)
	if len(ls.Incidents) > maxIncidents {
		ls.Incidents = ls.Incidents[len(ls.Incidents)-maxIncidents:]
	}
	_ = ls.save()
}

// MarkFalsePositive labels the most recent incident as a false positive and
// raises the threshold to reduce future sensitivity.
func (ls *LearningState) MarkFalsePositive(incidentID string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	for i := range ls.Incidents {
		if ls.Incidents[i].ID == incidentID {
			ls.Incidents[i].FalsePos = true
			ls.FalsePositives++
			break
		}
	}
	ls.adjustThreshold(+thresholdStep)
	_ = ls.save()
}

// MarkConfirmed labels an incident as a real threat and lowers the threshold
// slightly to increase future sensitivity.
func (ls *LearningState) MarkConfirmed(incidentID string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	for i := range ls.Incidents {
		if ls.Incidents[i].ID == incidentID {
			ls.Incidents[i].Confirmed = true
			ls.ConfirmedAlerts++
			break
		}
	}
	ls.adjustThreshold(-confirmStep)
	_ = ls.save()
}

// Status returns a summary map suitable for IPC/HTTP responses.
func (ls *LearningState) Status() map[string]any {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	suppressed := time.Now().Before(ls.OverrideCooldown)
	return map[string]any{
		"alert_threshold":    ls.AlertThreshold,
		"false_positives":    ls.FalsePositives,
		"confirmed_alerts":   ls.ConfirmedAlerts,
		"suppressed":         suppressed,
		"suppress_until":     ls.OverrideCooldown.Format(time.RFC3339),
		"recent_incidents":   len(ls.Incidents),
		"last_adjustment":    ls.LastAdjustment.Format(time.RFC3339),
	}
}

// adjustThreshold adjusts AlertThreshold by delta, enforcing cooldown and bounds.
// Must be called with mu held.
func (ls *LearningState) adjustThreshold(delta float64) {
	if time.Since(ls.LastAdjustment) < adjustCooldown {
		log.Printf("[automation/state] threshold adjustment skipped (cooldown active)")
		return
	}
	newVal := ls.AlertThreshold + delta
	if newVal < MinThreshold {
		newVal = MinThreshold
	}
	if newVal > MaxThreshold {
		newVal = MaxThreshold
	}
	log.Printf("[automation/state] threshold %.1f → %.1f (delta=%.1f)", ls.AlertThreshold, newVal, delta)
	ls.AlertThreshold = newVal
	ls.LastAdjustment = time.Now().UTC()
}

func (ls *LearningState) load() error {
	b, err := os.ReadFile(ls.stateFile)
	if err != nil {
		return err
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return json.Unmarshal(b, ls)
}

func (ls *LearningState) save() error {
	ls.mu.RLock()
	b, err := json.Marshal(ls)
	ls.mu.RUnlock()
	if err != nil {
		return err
	}
	dir := filepath.Dir(ls.stateFile)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".automation-state-")
	if err != nil {
		return err
	}
	tmp.Write(b)
	tmp.Sync()
	tmp.Close()
	return os.Rename(tmp.Name(), ls.stateFile)
}
