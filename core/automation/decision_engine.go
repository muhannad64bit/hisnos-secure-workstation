// core/automation/decision_engine.go — Autonomous security decision loop.
//
// Reads threat state from /var/lib/hisnos/threat-state.json every 30 seconds.
// Feeds observations into the risk predictor and anomaly clusterer.
// Triggers pre-emptive actions when:
//   - Prediction.AlertProbability ≥ 0.65 AND
//   - Active hot clusters exist AND
//   - Automation is not suppressed AND
//   - System is not already in safe-mode
//
// Exposes IPC commands: get_automation_status, override_automation.
package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	evalInterval      = 30 * time.Second
	alertProbThresh   = 0.65  // fire pre-emptive actions above this probability
	incidentIDPrefix  = "inc"
)

// threatStateFile is read on each evaluation cycle (written by the threat engine).
const threatStateFile = "/var/lib/hisnos/threat-state.json"

// threatSnapshot is the subset of threat-state.json that the decision engine reads.
type threatSnapshot struct {
	Score      float64 `json:"score"`
	Level      string  `json:"level"`
	Trajectory string  `json:"trajectory"`
	Signals    []struct {
		Name    string  `json:"name"`
		Decayed float64 `json:"decayed"`
	} `json:"signals"`
}

// DecisionEngine is the main automation coordinator.
// It owns the eval loop and all sub-components.
type DecisionEngine struct {
	state        *LearningState
	predictor    *RiskPredictor
	clusterer    *AnomalyCluster
	orchestrator *ResponseOrchestrator

	inSafeMode func() bool // injected from hisnosd
	emit       func(category, msg string, data map[string]any)

	mu       sync.Mutex
	lastPred Prediction
	lastDisp *DispatchResult

	evalCount atomic.Int64
}

// NewDecisionEngine constructs the full automation stack.
//
//	stateDir:   /var/lib/hisnos
//	submit:     IPC action dispatcher (wired to ipc.Server or stub in tests)
//	inSafeMode: callback — returns true when hisnosd is in safe-mode
//	emit:       structured event callback
func NewDecisionEngine(
	stateDir string,
	submit ActionFn,
	inSafeMode func() bool,
	emit func(string, string, map[string]any),
) *DecisionEngine {
	if inSafeMode == nil {
		inSafeMode = func() bool { return false }
	}
	if emit == nil {
		emit = func(_, _ string, _ map[string]any) {}
	}
	return &DecisionEngine{
		state:        NewLearningState(stateDir),
		predictor:    NewRiskPredictor(),
		clusterer:    NewAnomalyCluster(),
		orchestrator: NewResponseOrchestrator(submit),
		inSafeMode:   inSafeMode,
		emit:         emit,
	}
}

// Run starts the evaluation loop. Blocks until ctx is cancelled.
func (de *DecisionEngine) Run(ctx context.Context) {
	ticker := time.NewTicker(evalInterval)
	defer ticker.Stop()
	log.Printf("[automation] decision engine started (interval=%v)", evalInterval)

	for {
		select {
		case <-ctx.Done():
			log.Printf("[automation] decision engine stopped")
			return
		case <-ticker.C:
			de.evaluate()
		}
	}
}

// evaluate runs one decision cycle synchronously.
func (de *DecisionEngine) evaluate() {
	de.evalCount.Add(1)

	// 1. Read current threat state.
	snap, err := readThreatState()
	if err != nil {
		log.Printf("[automation] WARN: read threat state: %v", err)
		return
	}

	// 2. Feed observations into predictor and clusterer.
	de.predictor.Observe(snap.Score)
	for _, sig := range snap.Signals {
		if sig.Decayed > 0 {
			de.clusterer.Record(sig.Name, sig.Decayed)
		}
	}

	// 3. Compute prediction.
	threshold := de.state.Threshold()
	pred := de.predictor.Predict(threshold)

	de.mu.Lock()
	de.lastPred = pred
	de.mu.Unlock()

	// 4. Skip if suppressed or in safe-mode (safe-mode enforcer handles its own actions).
	if de.state.IsSuppressed() {
		log.Printf("[automation] suppressed — skipping dispatch (score=%.1f)", snap.Score)
		return
	}
	if de.inSafeMode() {
		log.Printf("[automation] in safe-mode — skipping pre-emptive dispatch")
		return
	}

	// 5. Dispatch pre-emptive actions if alert probability is high and clusters are hot.
	if pred.AlertProbability < alertProbThresh {
		return
	}
	hotClusters := de.clusterer.HotClusters()
	if len(hotClusters) == 0 {
		return
	}

	result := de.orchestrator.Dispatch(hotClusters)

	de.mu.Lock()
	de.lastDisp = result
	de.mu.Unlock()

	if result == nil || result.Skipped {
		return
	}

	// 6. Record the incident.
	incID := fmt.Sprintf("%s-%d", incidentIDPrefix, time.Now().UnixNano()%1_000_000)
	de.state.RecordIncident(incID, snap.Score, pred.Trajectory, result.Actions)

	de.emit("automation", "preemptive_action", map[string]any{
		"incident_id":       incID,
		"score":             snap.Score,
		"projected":         pred.ProjectedScore,
		"alert_probability": pred.AlertProbability,
		"pattern":           result.Pattern,
		"actions":           result.Actions,
		"threshold":         threshold,
	})
	log.Printf("[automation] pre-emptive: id=%s score=%.1f→%.1f prob=%.2f pattern=%s actions=%v",
		incID, snap.Score, pred.ProjectedScore, pred.AlertProbability, result.Pattern, result.Actions)
}

// Status returns a complete status snapshot for IPC/HTTP.
func (de *DecisionEngine) Status() map[string]any {
	de.mu.Lock()
	pred := de.lastPred
	disp := de.lastDisp
	de.mu.Unlock()

	stateStatus := de.state.Status()
	threshold := de.state.Threshold()
	clusters := de.clusterer.ActiveClusters()

	clusterSummary := make([]map[string]any, 0, len(clusters))
	for _, c := range clusters {
		clusterSummary = append(clusterSummary, map[string]any{
			"id": c.ID, "pattern": c.Pattern,
			"signals": c.Signals, "strength": c.Strength, "hot": c.Hot,
		})
	}

	var lastAction map[string]any
	if disp != nil {
		lastAction = map[string]any{
			"pattern": disp.Pattern, "actions": disp.Actions,
			"skipped": disp.Skipped, "skip_reason": disp.SkipReason,
		}
	}

	return map[string]any{
		"enabled":           true,
		"eval_count":        de.evalCount.Load(),
		"suppressed":        de.state.IsSuppressed(),
		"alert_threshold":   threshold,
		"current_score":     pred.CurrentScore,
		"projected_score":   pred.ProjectedScore,
		"trajectory":        pred.Trajectory,
		"alert_probability": pred.AlertProbability,
		"time_to_critical":  pred.TimeToCritical,
		"active_clusters":   clusterSummary,
		"last_action":       lastAction,
		"learning":          stateStatus,
	}
}

// IPCHandlers returns IPC command handlers for registration with ipc.Server.
func (de *DecisionEngine) IPCHandlers() map[string]func(map[string]any) (map[string]any, error) {
	return map[string]func(map[string]any) (map[string]any, error){
		"get_automation_status": func(_ map[string]any) (map[string]any, error) {
			return de.Status(), nil
		},
		"override_automation": func(params map[string]any) (map[string]any, error) {
			action, _ := params["action"].(string)
			switch action {
			case "suppress":
				durMin := 30
				if d, ok := params["duration_minutes"].(float64); ok && d > 0 {
					durMin = int(d)
				}
				de.state.Suppress(time.Duration(durMin) * time.Minute)
				return map[string]any{"suppressed": true, "duration_minutes": durMin}, nil
			case "reset":
				de.state.Suppress(0) // zero duration means "not suppressed"
				de.predictor.Reset()
				return map[string]any{"reset": true}, nil
			case "mark_false_positive":
				id, _ := params["incident_id"].(string)
				if id == "" {
					return nil, fmt.Errorf("params.incident_id required for mark_false_positive")
				}
				de.state.MarkFalsePositive(id)
				return map[string]any{"marked": "false_positive", "incident_id": id}, nil
			case "mark_confirmed":
				id, _ := params["incident_id"].(string)
				if id == "" {
					return nil, fmt.Errorf("params.incident_id required for mark_confirmed")
				}
				de.state.MarkConfirmed(id)
				return map[string]any{"marked": "confirmed", "incident_id": id}, nil
			default:
				return nil, fmt.Errorf("unknown action %q (suppress|reset|mark_false_positive|mark_confirmed)", action)
			}
		},
	}
}

// Beat is a no-op heartbeat for watchdog registration.
func (de *DecisionEngine) Beat() {}

// readThreatState reads and parses the threat engine's persisted state file.
func readThreatState() (threatSnapshot, error) {
	var snap threatSnapshot
	b, err := os.ReadFile(threatStateFile)
	if err != nil {
		return snap, fmt.Errorf("read %s: %w", threatStateFile, err)
	}
	return snap, json.Unmarshal(b, &snap)
}
