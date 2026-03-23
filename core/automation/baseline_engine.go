// core/automation/baseline_engine.go — Behavioural baseline engine.
//
// Learns the "normal" operating envelope of the system by accumulating
// metric samples over a configurable learning window (default 24 hours of
// wall time, sampled every 30 seconds). Once learning is complete the engine
// switches to active mode and computes a composite anomaly z-score against
// the baseline mean/stddev for each metric.
//
// Metrics tracked (all read from live system state or injected via Observe):
//
//	namespace_count    — number of active user namespaces
//	rt_proc_count      — number of SCHED_FIFO/SCHED_RR processes
//	threat_score       — current threat engine composite score (0-100)
//	firewall_rule_count — number of active nftables rules (from nft list ruleset)
//	cgroup_count       — number of leaf cgroups under /sys/fs/cgroup
//
// State machine:
//
//	learning → active (after learningSamples collected)
//	active   → locked  (operator calls Lock() to freeze baseline)
//	locked   → active  (operator calls Unlock() to resume adaptation)
//
// Anomaly score = RMS of per-metric z-scores, clamped to [0, 100].
// A score ≥ anomalyAlertThreshold is considered anomalous.
//
// State is persisted to /var/lib/hisnos/automation-baseline.json so the
// baseline survives service restarts. Partial learning windows are resumed.
package automation

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

const (
	baselineStatePath    = "/var/lib/hisnos/automation-baseline.json"
	learningSamples      = 2880  // 24 h × 2 samples/min = 2880
	anomalyAlertThreshold = 3.0  // z-score RMS threshold
	metricCount          = 5
)

// baselineState is the Engine's full persisted representation.
type baselineState struct {
	Phase   string             `json:"phase"` // "learning" | "active" | "locked"
	Samples int                `json:"samples"`
	Mean    map[string]float64 `json:"mean"`
	M2      map[string]float64 `json:"m2"` // Welford's sum of squared deviations
}

// BaselineEngine learns system behaviour and detects anomalies via z-score.
type BaselineEngine struct {
	mu    sync.Mutex
	state baselineState

	emit func(category, event string, data map[string]any)
}

// NewBaselineEngine loads persisted state or initialises fresh learning phase.
func NewBaselineEngine(emit func(string, string, map[string]any)) *BaselineEngine {
	if emit == nil {
		emit = func(_, _ string, _ map[string]any) {}
	}
	be := &BaselineEngine{emit: emit}
	be.state = baselineState{
		Phase:   "learning",
		Samples: 0,
		Mean:    make(map[string]float64),
		M2:      make(map[string]float64),
	}
	be.load()
	log.Printf("[baseline] phase=%s samples=%d", be.state.Phase, be.state.Samples)
	return be
}

// MetricSample carries one observation of all tracked metrics.
type MetricSample struct {
	NamespaceCount    float64
	RTProcCount       float64
	ThreatScore       float64
	FirewallRuleCount float64
	CgroupCount       float64
}

// metricKeys returns ordered slice of metric names matching MetricSample fields.
var metricKeys = []string{
	"namespace_count",
	"rt_proc_count",
	"threat_score",
	"firewall_rule_count",
	"cgroup_count",
}

func (m MetricSample) toMap() map[string]float64 {
	return map[string]float64{
		"namespace_count":    m.NamespaceCount,
		"rt_proc_count":      m.RTProcCount,
		"threat_score":       m.ThreatScore,
		"firewall_rule_count": m.FirewallRuleCount,
		"cgroup_count":       m.CgroupCount,
	}
}

// Observe ingests one metric sample. In learning phase it updates the
// Welford running mean/variance. In active/locked phase it computes and
// returns the anomaly score.
func (be *BaselineEngine) Observe(sample MetricSample) float64 {
	be.mu.Lock()
	defer be.mu.Unlock()

	vals := sample.toMap()
	be.state.Samples++
	n := float64(be.state.Samples)

	// Welford's online algorithm for mean and variance.
	for _, k := range metricKeys {
		v := vals[k]
		oldMean := be.state.Mean[k]
		be.state.Mean[k] += (v - oldMean) / n
		be.state.M2[k] += (v - oldMean) * (v - be.state.Mean[k])
	}

	// Transition learning → active.
	if be.state.Phase == "learning" && be.state.Samples >= learningSamples {
		be.state.Phase = "active"
		log.Printf("[baseline] learning complete after %d samples — switching to active", be.state.Samples)
		be.emit("automation", "baseline_active", map[string]any{
			"samples": be.state.Samples,
		})
		be.save()
	}

	if be.state.Phase == "learning" {
		return 0 // no score during learning
	}

	score := be.anomalyScore(vals)

	// Persist periodically (every 100 samples when active).
	if be.state.Samples%100 == 0 {
		be.save()
	}

	if score >= anomalyAlertThreshold {
		be.emit("automation", "baseline_anomaly_detected", map[string]any{
			"z_score_rms": score,
			"phase":       be.state.Phase,
			"sample":      vals,
		})
	}

	return score
}

// anomalyScore computes the RMS z-score across all metrics.
// Must be called with mu held.
func (be *BaselineEngine) anomalyScore(vals map[string]float64) float64 {
	n := float64(be.state.Samples)
	if n < 2 {
		return 0
	}
	var sumSq float64
	for _, k := range metricKeys {
		variance := be.state.M2[k] / (n - 1)
		stddev := sqrtApprox(variance)
		if stddev < 0.001 {
			continue // metric is constant — skip
		}
		z := (vals[k] - be.state.Mean[k]) / stddev
		sumSq += z * z
	}
	rms := sqrtApprox(sumSq / float64(metricCount))
	// Clamp to [0, 100].
	if rms > 100 {
		rms = 100
	}
	return rms
}

// Phase returns the current state machine phase.
func (be *BaselineEngine) Phase() string {
	be.mu.Lock()
	defer be.mu.Unlock()
	return be.state.Phase
}

// Lock freezes the baseline so it stops adapting (operator command).
func (be *BaselineEngine) Lock() {
	be.mu.Lock()
	defer be.mu.Unlock()
	if be.state.Phase == "active" {
		be.state.Phase = "locked"
		be.save()
		log.Printf("[baseline] locked by operator")
	}
}

// Unlock resumes adaptation from the locked state.
func (be *BaselineEngine) Unlock() {
	be.mu.Lock()
	defer be.mu.Unlock()
	if be.state.Phase == "locked" {
		be.state.Phase = "active"
		be.save()
		log.Printf("[baseline] unlocked by operator")
	}
}

// Reset clears the accumulated baseline and re-enters learning mode.
func (be *BaselineEngine) Reset() {
	be.mu.Lock()
	defer be.mu.Unlock()
	be.state = baselineState{
		Phase:   "learning",
		Samples: 0,
		Mean:    make(map[string]float64),
		M2:      make(map[string]float64),
	}
	be.save()
	log.Printf("[baseline] reset to learning phase")
}

// Status returns a JSON-compatible summary for IPC.
func (be *BaselineEngine) Status() map[string]any {
	be.mu.Lock()
	defer be.mu.Unlock()
	progress := 0.0
	if learningSamples > 0 {
		progress = float64(be.state.Samples) / float64(learningSamples) * 100
		if progress > 100 {
			progress = 100
		}
	}
	means := make(map[string]float64, len(be.state.Mean))
	for k, v := range be.state.Mean {
		means[k] = v
	}
	return map[string]any{
		"phase":             be.state.Phase,
		"samples":           be.state.Samples,
		"learning_progress": fmt.Sprintf("%.1f%%", progress),
		"means":             means,
	}
}

// save persists state to disk. Must be called with mu held.
func (be *BaselineEngine) save() {
	data, err := json.Marshal(be.state)
	if err != nil {
		log.Printf("[baseline] WARN: marshal state: %v", err)
		return
	}
	if err := writeAtomicAuto(baselineStatePath, string(data)); err != nil {
		log.Printf("[baseline] WARN: save state: %v", err)
	}
}

// load reads persisted state. Called once at startup without lock.
func (be *BaselineEngine) load() {
	data, err := os.ReadFile(baselineStatePath)
	if err != nil {
		return // first run — use defaults
	}
	var s baselineState
	if err := json.Unmarshal(data, &s); err != nil {
		log.Printf("[baseline] WARN: corrupt state file, resetting: %v", err)
		return
	}
	if s.Mean == nil {
		s.Mean = make(map[string]float64)
	}
	if s.M2 == nil {
		s.M2 = make(map[string]float64)
	}
	be.state = s
}

// writeAtomicAuto writes content to path atomically (tmp→sync→rename).
func writeAtomicAuto(path, content string) error {
	dir := dirOf(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".auto-tmp-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	return os.Rename(tmpPath, path)
}

// dirOf returns the directory component of a file path.
func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}

// timeNow is a hook for testing.
var timeNow = time.Now
