// core/automation/confidence_model.go — Autonomous response confidence model.
//
// Each candidate automated action is assigned a confidence score in [0.0, 1.0]
// based on three factors:
//
//  1. Signal strength  — how far the triggering metric exceeds its threshold
//  2. History factor   — past false-positive rate for this action type
//  3. Cluster factor   — whether a hot temporal cluster corroborates the action
//
// Scoring formula:
//
//	confidence = clamp(signal × history × cluster, 0, 1)
//
// Where:
//
//	signal  = min(excessRatio, 2.0) / 2.0       (0→0.5 for threshold+100%, caps at 1.0)
//	history = 1.0 - (falsePositives / (falsePositives + confirmations + 1))
//	cluster = 1.0 if hot cluster supports action, else 0.6
//
// Actions with confidence < confidenceThreshold are not executed immediately.
// Instead they are queued as PendingAction entries with a 5-minute confirmation
// deadline. The operator can approve or reject them via IPC. Expired entries
// are silently discarded.
//
// Confidence feedback:
//   - Confirmed action   → +1 confirmation for the action type
//   - Rejected / expired → +1 false-positive for the action type
//   - Feedback is persisted to /var/lib/hisnos/automation-confidence.json
package automation

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

const (
	confidencePath      = "/var/lib/hisnos/automation-confidence.json"
	confidenceThreshold = 0.70 // actions below this score are queued for confirmation
	pendingTTL          = 5 * time.Minute
)

// PendingAction is an action waiting for operator confirmation.
type PendingAction struct {
	ID          string         `json:"id"`
	ActionType  string         `json:"action_type"`
	Reason      string         `json:"reason"`
	Confidence  float64        `json:"confidence"`
	Params      map[string]any `json:"params,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	ExpiresAt   time.Time      `json:"expires_at"`
}

// actionHistory tracks per-action-type outcome counts.
type actionHistory struct {
	Confirmations int `json:"confirmations"`
	FalsePositives int `json:"false_positives"`
}

// confidenceState is persisted to disk.
type confidenceState struct {
	History map[string]*actionHistory `json:"history"`
}

// ConfidenceModel scores automated actions and manages the pending queue.
type ConfidenceModel struct {
	mu      sync.Mutex
	state   confidenceState
	pending []*PendingAction // in-memory queue; not persisted (short TTL)
	nextID  int

	emit func(category, event string, data map[string]any)
}

// NewConfidenceModel loads history and initialises the model.
func NewConfidenceModel(emit func(string, string, map[string]any)) *ConfidenceModel {
	if emit == nil {
		emit = func(_, _ string, _ map[string]any) {}
	}
	cm := &ConfidenceModel{
		state: confidenceState{History: make(map[string]*actionHistory)},
		emit:  emit,
	}
	cm.load()
	return cm
}

// ScoreInput carries the parameters needed to compute a confidence score.
type ScoreInput struct {
	ActionType     string
	CurrentValue   float64 // metric value that triggered the action
	ThresholdValue float64 // configured threshold for this metric
	HotCluster     bool    // true if a corroborating temporal cluster is active
}

// Score computes [0,1] confidence for the given action.
func (cm *ConfidenceModel) Score(in ScoreInput) float64 {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	// Signal factor.
	var excessRatio float64
	if in.ThresholdValue > 0 {
		excessRatio = (in.CurrentValue - in.ThresholdValue) / in.ThresholdValue
	}
	signal := excessRatio
	if signal > 1.0 {
		signal = 1.0
	}
	if signal < 0 {
		signal = 0
	}

	// History factor.
	hist := cm.historyFor(in.ActionType)
	total := float64(hist.Confirmations + hist.FalsePositives + 1)
	history := 1.0 - float64(hist.FalsePositives)/total

	// Cluster factor.
	cluster := 0.6
	if in.HotCluster {
		cluster = 1.0
	}

	conf := signal * history * cluster
	if conf > 1.0 {
		conf = 1.0
	}
	return conf
}

// Evaluate scores the action and either queues it (low confidence) or
// returns true immediately (high confidence ≥ threshold).
// Returns (approved bool, pendingID string).
func (cm *ConfidenceModel) Evaluate(in ScoreInput, reason string, params map[string]any) (bool, string) {
	conf := cm.Score(in)

	if conf >= confidenceThreshold {
		return true, ""
	}

	// Queue for operator confirmation.
	cm.mu.Lock()
	cm.nextID++
	id := formatPendingID(cm.nextID)
	pa := &PendingAction{
		ID:         id,
		ActionType: in.ActionType,
		Reason:     reason,
		Confidence: conf,
		Params:     params,
		CreatedAt:  time.Now(),
		ExpiresAt:  time.Now().Add(pendingTTL),
	}
	cm.pending = append(cm.pending, pa)
	cm.mu.Unlock()

	cm.emit("automation", "action_pending_confirmation", map[string]any{
		"id": id, "action_type": in.ActionType,
		"confidence": conf, "expires_in_s": int(pendingTTL.Seconds()),
		"reason": reason,
	})
	log.Printf("[confidence] queued %s confidence=%.2f id=%s", in.ActionType, conf, id)
	return false, id
}

// Confirm marks a pending action as operator-confirmed and records the outcome.
// Returns the approved PendingAction or nil if not found / expired.
func (cm *ConfidenceModel) Confirm(id string) *PendingAction {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.purgeExpired()
	for i, pa := range cm.pending {
		if pa.ID == id {
			cm.pending = append(cm.pending[:i], cm.pending[i+1:]...)
			cm.recordConfirm(pa.ActionType)
			log.Printf("[confidence] confirmed %s id=%s", pa.ActionType, id)
			return pa
		}
	}
	return nil
}

// Reject marks a pending action as operator-rejected (counts as false positive).
func (cm *ConfidenceModel) Reject(id string) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.purgeExpired()
	for i, pa := range cm.pending {
		if pa.ID == id {
			cm.pending = append(cm.pending[:i], cm.pending[i+1:]...)
			cm.recordFP(pa.ActionType)
			log.Printf("[confidence] rejected %s id=%s", pa.ActionType, id)
			return true
		}
	}
	return false
}

// PendingList returns all non-expired pending actions.
func (cm *ConfidenceModel) PendingList() []*PendingAction {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.purgeExpired()
	out := make([]*PendingAction, len(cm.pending))
	copy(out, cm.pending)
	return out
}

// RecordOutcome allows the orchestrator to feed back whether an auto-approved
// action was confirmed (via post-action telemetry) or turned out to be a FP.
func (cm *ConfidenceModel) RecordOutcome(actionType string, confirmed bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if confirmed {
		cm.recordConfirm(actionType)
	} else {
		cm.recordFP(actionType)
	}
}

// Status returns IPC-ready state.
func (cm *ConfidenceModel) Status() map[string]any {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.purgeExpired()
	pending := make([]map[string]any, 0, len(cm.pending))
	for _, pa := range cm.pending {
		pending = append(pending, map[string]any{
			"id": pa.ID, "action_type": pa.ActionType,
			"confidence": pa.Confidence,
			"expires_in_s": int(time.Until(pa.ExpiresAt).Seconds()),
		})
	}
	return map[string]any{
		"pending_count": len(cm.pending),
		"pending":       pending,
		"threshold":     confidenceThreshold,
	}
}

// ─── internal helpers ────────────────────────────────────────────────────────

func (cm *ConfidenceModel) historyFor(actionType string) *actionHistory {
	h, ok := cm.state.History[actionType]
	if !ok {
		h = &actionHistory{}
		cm.state.History[actionType] = h
	}
	return h
}

func (cm *ConfidenceModel) recordConfirm(actionType string) {
	cm.historyFor(actionType).Confirmations++
	cm.save()
}

func (cm *ConfidenceModel) recordFP(actionType string) {
	cm.historyFor(actionType).FalsePositives++
	cm.save()
}

// purgeExpired removes pending actions past their TTL and counts them as FPs.
// Must be called with mu held.
func (cm *ConfidenceModel) purgeExpired() {
	now := time.Now()
	kept := cm.pending[:0]
	for _, pa := range cm.pending {
		if now.After(pa.ExpiresAt) {
			cm.historyFor(pa.ActionType).FalsePositives++
			log.Printf("[confidence] expired pending id=%s %s", pa.ID, pa.ActionType)
		} else {
			kept = append(kept, pa)
		}
	}
	cm.pending = kept
}

func (cm *ConfidenceModel) save() {
	data, err := json.Marshal(cm.state)
	if err != nil {
		return
	}
	_ = writeAtomicAuto(confidencePath, string(data))
}

func (cm *ConfidenceModel) load() {
	data, err := os.ReadFile(confidencePath)
	if err != nil {
		return
	}
	var s confidenceState
	if err := json.Unmarshal(data, &s); err != nil {
		return
	}
	if s.History == nil {
		s.History = make(map[string]*actionHistory)
	}
	cm.state = s
}

func formatPendingID(n int) string {
	// Simple zero-padded decimal — no external formatting needed.
	s := "000000" + intToStr(n)
	return "pa-" + s[len(s)-6:]
}

func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := 20
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
