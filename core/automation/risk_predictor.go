// core/automation/risk_predictor.go — 10-minute risk escalation predictor.
//
// Uses double exponential smoothing (Holt's linear method) to project the
// threat risk score 10 minutes into the future. No external math library.
//
// Holt's method:
//   Level:   L_t = α·y_t + (1−α)·(L_{t-1} + T_{t-1})
//   Trend:   T_t = β·(L_t − L_{t-1}) + (1−β)·T_{t-1}
//   Forecast at horizon h: F_{t+h} = L_t + h·T_t
//
// Alert probability is linear in (projected − threshold), clipped to [0, 1].
package automation

import (
	"fmt"
	"sync"
	"time"
)

const (
	smoothingAlpha  = 0.3 // level smoothing factor
	smoothingBeta   = 0.2 // trend smoothing factor
	historyWindow   = 30  // max samples retained for projection
	predictionHoriz = 10  // minutes to predict forward
)

// TimedScore is a single timestamped risk score observation.
type TimedScore struct {
	Score float64
	At    time.Time
}

// Prediction holds the output of a projection run.
type Prediction struct {
	CurrentScore    float64 // latest observed score
	ProjectedScore  float64 // predicted score in predictionHoriz minutes
	Trajectory      string  // RISING | FALLING | STABLE | VOLATILE
	AlertProbability float64 // [0,1] probability of exceeding threshold
	TimeToCritical  float64 // estimated minutes until score exceeds threshold (−1 if N/A)
}

// RiskPredictor projects risk score escalation using Holt's linear smoothing.
type RiskPredictor struct {
	mu      sync.Mutex
	history []TimedScore
	level   float64
	trend   float64
	seeded  bool
}

// NewRiskPredictor returns a fresh predictor.
func NewRiskPredictor() *RiskPredictor {
	return &RiskPredictor{}
}

// Observe records a new risk score observation and updates the smoothing state.
func (p *RiskPredictor) Observe(score float64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	p.history = append(p.history, TimedScore{Score: score, At: now})
	if len(p.history) > historyWindow {
		p.history = p.history[len(p.history)-historyWindow:]
	}

	if !p.seeded {
		p.level = score
		p.trend = 0
		p.seeded = true
		return
	}
	prevLevel := p.level
	p.level = smoothingAlpha*score + (1-smoothingAlpha)*(p.level+p.trend)
	p.trend = smoothingBeta*(p.level-prevLevel) + (1-smoothingBeta)*p.trend
}

// Predict returns a 10-minute escalation projection.
// threshold is the alert trigger threshold from LearningState.
func (p *RiskPredictor) Predict(threshold float64) Prediction {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.history) == 0 {
		return Prediction{Trajectory: "STABLE"}
	}

	current := p.history[len(p.history)-1].Score

	if !p.seeded {
		return Prediction{CurrentScore: current, ProjectedScore: current, Trajectory: "STABLE"}
	}

	// Project forward predictionHoriz minutes.
	// Holt's linear: F_{t+h} = L + h·T  (h = number of periods = 10 one-minute ticks)
	projected := p.level + float64(predictionHoriz)*p.trend
	if projected < 0 {
		projected = 0
	}
	if projected > 100 {
		projected = 100
	}

	// Classify trajectory using trend per minute.
	traj := classifyTrajectory(p.trend, p.history)

	// Alert probability: linear interpolation between [threshold-10, threshold+10] → [0, 1].
	prob := (projected - (threshold - 10)) / 20.0
	if prob < 0 {
		prob = 0
	}
	if prob > 1 {
		prob = 1
	}

	// Estimated minutes until score reaches threshold (if rising).
	ttc := float64(-1)
	if p.trend > 0.1 && current < threshold {
		ttc = (threshold - p.level) / p.trend
		if ttc < 0 {
			ttc = 0
		}
	}

	return Prediction{
		CurrentScore:    current,
		ProjectedScore:  projected,
		Trajectory:      traj,
		AlertProbability: prob,
		TimeToCritical:  ttc,
	}
}

// Reset clears all history and smoothing state (e.g. on safe-mode entry).
func (p *RiskPredictor) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.history = nil
	p.seeded = false
	p.level = 0
	p.trend = 0
}

// Summary returns a description for logging/IPC.
func (p *RiskPredictor) Summary(threshold float64) map[string]any {
	pred := p.Predict(threshold)
	return map[string]any{
		"current_score":     pred.CurrentScore,
		"projected_score":   fmt.Sprintf("%.1f", pred.ProjectedScore),
		"trajectory":        pred.Trajectory,
		"alert_probability": fmt.Sprintf("%.2f", pred.AlertProbability),
		"time_to_critical":  pred.TimeToCritical,
		"horizon_minutes":   predictionHoriz,
	}
}

// classifyTrajectory labels the current trend based on trend-per-minute and variance.
// RISING: trend > +0.5/min  FALLING: trend < -0.5/min  VOLATILE: stddev > 8  else STABLE.
func classifyTrajectory(trend float64, history []TimedScore) string {
	switch {
	case trend > 0.5:
		return "RISING"
	case trend < -0.5:
		return "FALLING"
	}
	// Check volatility: compute stddev of the last min(10, len) samples.
	n := len(history)
	if n > 10 {
		history = history[n-10:]
		n = 10
	}
	if n < 3 {
		return "STABLE"
	}
	var sum float64
	for _, s := range history {
		sum += s.Score
	}
	mean := sum / float64(n)
	var variance float64
	for _, s := range history {
		d := s.Score - mean
		variance += d * d
	}
	variance /= float64(n)
	stddev := sqrtApprox(variance)
	if stddev > 8.0 {
		return "VOLATILE"
	}
	return "STABLE"
}

// sqrtApprox computes sqrt(x) via Newton-Raphson (10 iterations). Stdlib-free.
func sqrtApprox(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x / 2
	for i := 0; i < 10; i++ {
		z -= (z*z - x) / (2 * z)
	}
	return z
}
