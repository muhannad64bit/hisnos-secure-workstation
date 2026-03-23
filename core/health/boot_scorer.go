// core/health/boot_scorer.go — Rolling boot reliability scorer.
//
// Maintains a ring buffer of the last 7 boot records and computes a
// composite reliability score (0–100) after each boot.
//
// Per-boot metrics collected at startup:
//   - failed_units:    number of systemd units that failed during boot
//   - degraded_units:  number of systemd units in degraded state
//   - boot_time_ms:    time from kernel start to multi-user.target (ms)
//   - emergency:       true if the system entered emergency mode
//   - safe_mode:       true if hisnosd entered safe-mode at boot
//
// Scoring formula (deductions from 100):
//   - Each failed unit:        -15 (max deduction 45)
//   - Each degraded unit:      -5  (max deduction 20)
//   - boot_time > 60s:         -10
//   - boot_time > 120s:        -20 (cumulative)
//   - emergency mode:          -40
//   - safe_mode entered:       -10
//
// Rolling score = weighted average of last bootRingSize scores,
// with more recent boots weighted 2× older boots.
//
// The score is persisted to /var/lib/hisnos/boot-health.json.
// A score < 50 for the rolling window triggers a "boot_reliability_degraded"
// event so the operator can investigate.
package health

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	bootHealthPath  = "/var/lib/hisnos/boot-health.json"
	bootRingSize    = 7
	degradedScoreThreshold = 50
)

// BootRecord captures metrics for one boot cycle.
type BootRecord struct {
	BootID       string    `json:"boot_id"`
	RecordedAt   time.Time `json:"recorded_at"`
	FailedUnits  int       `json:"failed_units"`
	DegradedUnits int     `json:"degraded_units"`
	BootTimeMs   int64     `json:"boot_time_ms"`
	Emergency    bool      `json:"emergency"`
	SafeMode     bool      `json:"safe_mode"`
	Score        int       `json:"score"`
}

// bootHealthState is the persisted ring buffer.
type bootHealthState struct {
	Ring    [bootRingSize]*BootRecord `json:"ring"`
	Head    int                       `json:"head"` // next write position
	Full    bool                      `json:"full"`
}

// BootScorer records boot health and computes reliability scores.
type BootScorer struct {
	mu    sync.Mutex
	state bootHealthState
	emit  func(category, event string, data map[string]any)
}

// NewBootScorer loads existing history and prepares to record the current boot.
func NewBootScorer(emit func(string, string, map[string]any)) *BootScorer {
	if emit == nil {
		emit = func(_, _ string, _ map[string]any) {}
	}
	bs := &BootScorer{emit: emit}
	bs.load()
	return bs
}

// RecordBoot introspects the current boot and adds a record to the ring.
// Call once per boot at startup (after systemd reaches multi-user.target).
func (bs *BootScorer) RecordBoot(safeModeActive bool) {
	rec := bs.introspect(safeModeActive)
	rec.Score = computeBootScore(rec)

	bs.mu.Lock()
	bs.state.Ring[bs.state.Head] = rec
	bs.state.Head = (bs.state.Head + 1) % bootRingSize
	if bs.state.Head == 0 {
		bs.state.Full = true
	}
	rolling := bs.rollingScore()
	bs.mu.Unlock()

	bs.save()

	log.Printf("[boot-scorer] boot %s score=%d rolling=%.1f failed=%d degraded=%d boot_time=%dms",
		rec.BootID[:8], rec.Score, rolling,
		rec.FailedUnits, rec.DegradedUnits, rec.BootTimeMs)

	bs.emit("health", "boot_recorded", map[string]any{
		"boot_id":       rec.BootID,
		"score":         rec.Score,
		"rolling_score": rolling,
		"failed_units":  rec.FailedUnits,
		"boot_time_ms":  rec.BootTimeMs,
	})

	if rolling < degradedScoreThreshold {
		log.Printf("[boot-scorer] WARN: rolling reliability score %.1f < %d", rolling, degradedScoreThreshold)
		bs.emit("health", "boot_reliability_degraded", map[string]any{
			"rolling_score":   rolling,
			"threshold":       degradedScoreThreshold,
			"samples":         bs.sampleCount(),
		})
	}
}

// RollingScore returns the current weighted reliability score.
func (bs *BootScorer) RollingScore() float64 {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	return bs.rollingScore()
}

// Status returns IPC-ready health data.
func (bs *BootScorer) Status() map[string]any {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	rolling := bs.rollingScore()
	samples := bs.sampleCount()
	lastScore := 0
	if samples > 0 {
		prev := (bs.state.Head - 1 + bootRingSize) % bootRingSize
		if bs.state.Ring[prev] != nil {
			lastScore = bs.state.Ring[prev].Score
		}
	}
	return map[string]any{
		"rolling_score":     rolling,
		"last_boot_score":   lastScore,
		"samples":           samples,
		"ring_size":         bootRingSize,
		"degraded":          rolling < degradedScoreThreshold,
	}
}

// ─── internal ───────────────────────────────────────────────────────────────

func (bs *BootScorer) introspect(safeModeActive bool) *BootRecord {
	rec := &BootRecord{
		RecordedAt: time.Now(),
		SafeMode:   safeModeActive,
	}

	// Boot ID.
	if id, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		rec.BootID = strings.TrimSpace(strings.ReplaceAll(string(id), "-", ""))
	} else {
		rec.BootID = fmt.Sprintf("%d", time.Now().UnixNano())
	}

	// Failed and degraded units.
	rec.FailedUnits = countUnitsByState("failed")
	rec.DegradedUnits = countUnitsByState("degraded")

	// Boot time via systemd-analyze.
	rec.BootTimeMs = measureBootTimeMs()

	// Emergency mode: check if emergency.target was reached.
	out, err := exec.Command("systemctl", "is-active", "emergency.target").Output()
	if err == nil && strings.TrimSpace(string(out)) == "active" {
		rec.Emergency = true
	}

	return rec
}

// computeBootScore computes the score for a single boot record.
func computeBootScore(r *BootRecord) int {
	score := 100

	// Failed units: -15 each, max -45.
	fd := r.FailedUnits * 15
	if fd > 45 {
		fd = 45
	}
	score -= fd

	// Degraded units: -5 each, max -20.
	dd := r.DegradedUnits * 5
	if dd > 20 {
		dd = 20
	}
	score -= dd

	// Boot time.
	if r.BootTimeMs > 120000 {
		score -= 20
	} else if r.BootTimeMs > 60000 {
		score -= 10
	}

	// Emergency mode.
	if r.Emergency {
		score -= 40
	}

	// Safe-mode.
	if r.SafeMode {
		score -= 10
	}

	if score < 0 {
		score = 0
	}
	return score
}

// rollingScore computes the weighted rolling average.
// More-recent boots are weighted 2×. Must be called with mu held.
func (bs *BootScorer) rollingScore() float64 {
	n := bs.sampleCount()
	if n == 0 {
		return 100
	}
	var weightedSum, totalWeight float64
	for i := 0; i < n; i++ {
		// i=0 is oldest, i=n-1 is newest.
		idx := (bs.state.Head - n + i + bootRingSize) % bootRingSize
		rec := bs.state.Ring[idx]
		if rec == nil {
			continue
		}
		weight := 1.0 + float64(i)/float64(n) // 1.0 → 2.0
		weightedSum += float64(rec.Score) * weight
		totalWeight += weight
	}
	if totalWeight == 0 {
		return 100
	}
	return weightedSum / totalWeight
}

func (bs *BootScorer) sampleCount() int {
	if bs.state.Full {
		return bootRingSize
	}
	return bs.state.Head
}

func (bs *BootScorer) load() {
	data, err := os.ReadFile(bootHealthPath)
	if err != nil {
		return
	}
	var s bootHealthState
	if err := json.Unmarshal(data, &s); err != nil {
		log.Printf("[boot-scorer] WARN: corrupt state: %v", err)
		return
	}
	bs.state = s
}

func (bs *BootScorer) save() {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	data, err := json.Marshal(bs.state)
	if err != nil {
		return
	}
	dir := filepath.Dir(bootHealthPath)
	_ = os.MkdirAll(dir, 0750)
	tmp, err := os.CreateTemp(dir, ".boot-tmp-")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	tmp.Write(data)
	tmp.Sync()
	tmp.Close()
	_ = os.Rename(tmpPath, bootHealthPath)
}

// countUnitsByState returns the number of systemd units in the given state.
func countUnitsByState(state string) int {
	out, err := exec.Command("systemctl", "list-units",
		"--state="+state, "--no-legend", "--no-pager", "-q").Output()
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

// measureBootTimeMs returns the total boot time in milliseconds
// by parsing systemd-analyze output.
func measureBootTimeMs() int64 {
	out, err := exec.Command("systemd-analyze", "--no-pager").Output()
	if err != nil {
		return 0
	}
	// Output example:
	// Startup finished in 1.234s (kernel) + 5.678s (initrd) + 23.456s (userspace) = 30.368s
	// We want the total after "= ".
	s := string(out)
	idx := strings.LastIndex(s, "= ")
	if idx < 0 {
		return 0
	}
	rest := strings.TrimSpace(s[idx+2:])
	// rest = "30.368s" or "1min 5.3s"
	ms := parseTimeMs(rest)
	return ms
}

// parseTimeMs parses strings like "30.368s", "1min 5s", "500ms" into ms.
func parseTimeMs(s string) int64 {
	s = strings.TrimSpace(s)
	var total int64
	// Handle "Xmin Y.Zs"
	if idx := strings.Index(s, "min"); idx >= 0 {
		minStr := strings.TrimSpace(s[:idx])
		mins, _ := strconv.ParseInt(minStr, 10, 64)
		total += mins * 60000
		s = strings.TrimSpace(s[idx+3:])
	}
	// Handle "X.Ys" or "Xms"
	if strings.HasSuffix(s, "ms") {
		val, _ := strconv.ParseFloat(strings.TrimSuffix(s, "ms"), 64)
		total += int64(val)
	} else if strings.HasSuffix(s, "s") {
		val, _ := strconv.ParseFloat(strings.TrimSuffix(s, "s"), 64)
		total += int64(val * 1000)
	}
	return total
}
