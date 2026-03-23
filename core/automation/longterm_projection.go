// core/automation/longterm_projection.go — Long-term threat trend projection.
//
// Maintains a rolling 24-hour history of 5-minute threat score averages.
// Applies ordinary least squares (OLS) linear regression over the most recent
// 2-hour window to compute the threat slope (score-units per minute).
//
// Threat momentum:
//
//	momentum = slope × (currentScore / 100)
//
// This combines the rate-of-change with the current severity, so a rising
// low-score situation has low momentum while a rising high-score situation
// has high momentum.
//
// Pre-emptive actions are triggered when momentum crosses configured thresholds:
//
//	momentum ≥ 0.5 → emit "threat_momentum_warning"
//	momentum ≥ 1.5 → emit "threat_momentum_critical" + call onPreemptive
//	momentum ≥ 3.0 → emit "threat_momentum_emergency"  + call onPreemptive
//
// Bucket resolution: 5 minutes (300 seconds).
// Ring buffer size:  288 buckets = 24 hours.
// Regression window: 24 buckets = 2 hours.
//
// State is in-memory only (projection is forward-looking; history older than
// 24 h is irrelevant). No persistence needed.
package automation

import (
	"log"
	"sync"
	"time"
)

const (
	bucketDuration    = 5 * time.Minute
	historyBuckets    = 288 // 24 h / 5 min
	regressionBuckets = 24  // 2 h
	momentumWarn      = 0.5
	momentumCritical  = 1.5
	momentumEmergency = 3.0
)

// threatBucket is one 5-minute averaged sample.
type threatBucket struct {
	Sum   float64
	Count int
	At    time.Time // bucket start time
}

func (b *threatBucket) avg() float64 {
	if b.Count == 0 {
		return 0
	}
	return b.Sum / float64(b.Count)
}

// TrendProjection holds current trend analysis output.
type TrendProjection struct {
	Slope        float64 // score units per minute (positive = rising)
	Momentum     float64 // slope × (currentScore/100)
	CurrentScore float64
	Projection2h float64 // projected score in 2 hours if trend continues
	SampleCount  int
}

// LongtermProjector maintains threat score history and computes trend momentum.
type LongtermProjector struct {
	mu          sync.Mutex
	ring        [historyBuckets]threatBucket
	ringHead    int  // next write position
	ringFull    bool
	curBucketAt time.Time // when the current bucket started

	lastMomentum    float64
	lastMomentumAt  time.Time

	onPreemptive func(level string, momentum float64)
	emit         func(category, event string, data map[string]any)
}

// NewLongtermProjector creates a projector with optional pre-emptive callback.
func NewLongtermProjector(
	onPreemptive func(level string, momentum float64),
	emit func(string, string, map[string]any),
) *LongtermProjector {
	if emit == nil {
		emit = func(_, _ string, _ map[string]any) {}
	}
	return &LongtermProjector{
		onPreemptive: onPreemptive,
		emit:         emit,
		curBucketAt:  bucketStart(time.Now()),
	}
}

// Observe feeds the current threat score into the active bucket.
// Call this on every decision cycle (every 30 s or so).
func (lp *LongtermProjector) Observe(score float64) TrendProjection {
	lp.mu.Lock()
	defer lp.mu.Unlock()

	now := time.Now()
	lp.advanceBuckets(now)

	cur := &lp.ring[lp.ringHead]
	cur.Sum += score
	cur.Count++

	proj := lp.computeTrend(score)
	lp.checkMomentum(proj, now)
	return proj
}

// Trend returns the latest computed trend without feeding a new sample.
func (lp *LongtermProjector) Trend() TrendProjection {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	curScore := 0.0
	if lp.ring[lp.ringHead].Count > 0 {
		curScore = lp.ring[lp.ringHead].avg()
	}
	return lp.computeTrend(curScore)
}

// Status returns IPC-ready projection data.
func (lp *LongtermProjector) Status() map[string]any {
	proj := lp.Trend()
	return map[string]any{
		"slope_per_min":   proj.Slope,
		"momentum":        proj.Momentum,
		"current_score":   proj.CurrentScore,
		"projection_2h":   proj.Projection2h,
		"sample_count":    proj.SampleCount,
		"momentum_warn":   momentumWarn,
		"momentum_crit":   momentumCritical,
		"momentum_emerg":  momentumEmergency,
	}
}

// ─── internal ───────────────────────────────────────────────────────────────

// advanceBuckets moves ringHead forward if the current time has crossed
// bucket boundaries since the last observation.
// Must be called with mu held.
func (lp *LongtermProjector) advanceBuckets(now time.Time) {
	bs := bucketStart(now)
	for bs.After(lp.curBucketAt) {
		lp.curBucketAt = lp.curBucketAt.Add(bucketDuration)
		lp.ringHead = (lp.ringHead + 1) % historyBuckets
		if lp.ringHead == 0 {
			lp.ringFull = true
		}
		// Reset new head bucket.
		lp.ring[lp.ringHead] = threatBucket{At: lp.curBucketAt}
	}
}

// computeTrend runs OLS linear regression over the last regressionBuckets
// populated buckets and returns the trend projection.
// Must be called with mu held.
func (lp *LongtermProjector) computeTrend(currentScore float64) TrendProjection {
	// Collect up to regressionBuckets non-empty historical buckets.
	bucketCap := historyBuckets
	if !lp.ringFull {
		bucketCap = lp.ringHead + 1
	}
	limit := regressionBuckets
	if limit > bucketCap {
		limit = bucketCap
	}

	type xy struct{ x, y float64 }
	var pts []xy
	for i := 0; i < limit; i++ {
		idx := (lp.ringHead - i + historyBuckets) % historyBuckets
		b := &lp.ring[idx]
		if b.Count == 0 {
			continue
		}
		// x in minutes ago (negative = past).
		x := float64(-i) * bucketDuration.Minutes()
		pts = append(pts, xy{x, b.avg()})
	}
	pts = append(pts, xy{0, currentScore}) // include current partial bucket

	if len(pts) < 2 {
		return TrendProjection{CurrentScore: currentScore, SampleCount: len(pts)}
	}

	// OLS: y = a + b*x  where b = slope, a = intercept.
	var sumX, sumY, sumXX, sumXY float64
	n := float64(len(pts))
	for _, p := range pts {
		sumX += p.x
		sumY += p.y
		sumXX += p.x * p.x
		sumXY += p.x * p.y
	}
	denom := n*sumXX - sumX*sumX
	var slope float64
	if denom != 0 {
		slope = (n*sumXY - sumX*sumY) / denom
	}

	momentum := slope * (currentScore / 100)

	// Project 2 h forward (120 minutes).
	projection := currentScore + slope*120
	if projection < 0 {
		projection = 0
	}
	if projection > 100 {
		projection = 100
	}

	return TrendProjection{
		Slope:        slope,
		Momentum:     momentum,
		CurrentScore: currentScore,
		Projection2h: projection,
		SampleCount:  len(pts),
	}
}

// checkMomentum emits events and triggers pre-emptive actions when momentum
// crosses thresholds. Rate-limited to once per bucketDuration.
// Must be called with mu held.
func (lp *LongtermProjector) checkMomentum(proj TrendProjection, now time.Time) {
	if now.Sub(lp.lastMomentumAt) < bucketDuration {
		return
	}
	m := proj.Momentum
	if m < momentumWarn {
		return
	}

	lp.lastMomentumAt = now
	lp.lastMomentum = m

	level := "warning"
	switch {
	case m >= momentumEmergency:
		level = "emergency"
	case m >= momentumCritical:
		level = "critical"
	}

	log.Printf("[longterm] threat momentum=%.2f slope=%.3f/min score=%.1f level=%s",
		m, proj.Slope, proj.CurrentScore, level)

	lp.emit("automation", "threat_momentum_"+level, map[string]any{
		"momentum":       m,
		"slope_per_min":  proj.Slope,
		"current_score":  proj.CurrentScore,
		"projection_2h":  proj.Projection2h,
	})

	if lp.onPreemptive != nil && (level == "critical" || level == "emergency") {
		lp.onPreemptive(level, m)
	}
}

// bucketStart rounds t down to the nearest bucketDuration boundary.
func bucketStart(t time.Time) time.Time {
	unix := t.UnixNano()
	d := int64(bucketDuration)
	return time.Unix(0, (unix/d)*d)
}
