// core/performance/frame_predictor.go — Frame latency predictor with jitter detection.
//
// Reads frame-time data from MangoHud log files (written when MANGOHUD_OUTPUT
// is set) or hispowerd's frame metrics. Computes P50/P95/P99 frame times using
// an insertion-sorted ring buffer and triggers performance escalation when
// jitter exceeds the threshold (P99 > 2× P50).
//
// MangoHud log path discovery:
//   $XDG_RUNTIME_DIR/MangoHud/ → *.csv files (newest wins)
//   /tmp/MangoHud/              → fallback
//
// MangoHud CSV format (header row):
//   fps,cpu_load,gpu_load,cpu_temp,gpu_temp,ram,vram,frametime
//   60,25,45,65,70,8192,4096,16.67
//
// hispowerd frame metric: /run/hispowerd/frame-stats (one float per line = ms)
//
// Escalation: when jitter spike detected, calls onJitterSpike callback which
// wires to performance.Manager.Apply("ultra") or IRQ rebalancer.
package performance

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	frameRingSize    = 120             // last 120 frames (~2s at 60fps)
	jitterRatio      = 2.0            // P99 > ratio*P50 → spike
	frameMinSamples  = 30             // need at least N samples before evaluating
	escalateCooldown = 30 * time.Second
)

// FramePredictor monitors frame pacing and detects latency spikes.
type FramePredictor struct {
	mu           sync.Mutex
	ring         []float64 // frame times in ms (ring buffer)
	ringPos      int
	ringFull     bool
	lastEscalate time.Time
	logDirs      []string // directories to search for MangoHud logs

	onJitterSpike func(p50, p99 float64) // callback: performance escalation
	emit          func(string, string, map[string]any)
}

// FrameStats holds current percentile latency statistics.
type FrameStats struct {
	SampleCount int
	P50         float64
	P95         float64
	P99         float64
	Jitter      float64 // P99/P50 ratio
	JitterSpike bool
}

// NewFramePredictor creates a predictor with a jitter spike callback.
func NewFramePredictor(
	onJitterSpike func(p50, p99 float64),
	emit func(string, string, map[string]any),
) *FramePredictor {
	if emit == nil {
		emit = func(_, _ string, _ map[string]any) {}
	}
	uid := os.Getuid()
	logDirs := []string{
		fmt.Sprintf("/run/user/%d/MangoHud", uid),
		"/tmp/MangoHud",
		"/run/hispowerd",
	}
	return &FramePredictor{
		ring:          make([]float64, frameRingSize),
		logDirs:       logDirs,
		onJitterSpike: onJitterSpike,
		emit:          emit,
	}
}

// Tick reads new frame data and evaluates jitter. Call on each eval interval.
func (fp *FramePredictor) Tick() {
	frames, err := fp.readFrameTimes()
	if err != nil || len(frames) == 0 {
		return
	}

	fp.mu.Lock()
	for _, ft := range frames {
		fp.ring[fp.ringPos] = ft
		fp.ringPos = (fp.ringPos + 1) % frameRingSize
		if fp.ringPos == 0 {
			fp.ringFull = true
		}
	}
	stats := fp.computeStats()
	fp.mu.Unlock()

	if stats.SampleCount < frameMinSamples {
		return
	}

	if stats.JitterSpike && time.Since(fp.lastEscalate) > escalateCooldown {
		fp.lastEscalate = time.Now()
		fp.emit("performance", "frame_jitter_spike", map[string]any{
			"p50_ms": stats.P50, "p99_ms": stats.P99, "ratio": stats.Jitter,
		})
		if fp.onJitterSpike != nil {
			fp.onJitterSpike(stats.P50, stats.P99)
		}
	}
}

// Stats returns the current frame latency statistics (thread-safe).
func (fp *FramePredictor) Stats() FrameStats {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return fp.computeStats()
}

// computeStats computes percentile latencies from the ring buffer.
// Must be called with mu held.
func (fp *FramePredictor) computeStats() FrameStats {
	n := fp.ringPos
	if fp.ringFull {
		n = frameRingSize
	}
	if n == 0 {
		return FrameStats{}
	}

	// Copy and sort for percentile computation.
	sorted := make([]float64, n)
	copy(sorted, fp.ring[:n])
	sort.Float64s(sorted)

	p50 := percentile(sorted, 50)
	p95 := percentile(sorted, 95)
	p99 := percentile(sorted, 99)
	ratio := float64(0)
	if p50 > 0 {
		ratio = p99 / p50
	}

	return FrameStats{
		SampleCount: n,
		P50:         p50,
		P95:         p95,
		P99:         p99,
		Jitter:      ratio,
		JitterSpike: ratio > jitterRatio,
	}
}

// readFrameTimes reads recent frame times from the best available source.
// Returns frame times in milliseconds.
func (fp *FramePredictor) readFrameTimes() ([]float64, error) {
	// Try hispowerd frame stats first (most reliable).
	if frames, err := fp.readHispowerdFrames(); err == nil && len(frames) > 0 {
		return frames, nil
	}
	// Fall back to MangoHud log.
	return fp.readMangoHudFrames()
}

// readHispowerdFrames reads from /run/hispowerd/frame-stats.
// hispowerd writes one frame time per line when gaming mode is active.
func (fp *FramePredictor) readHispowerdFrames() ([]float64, error) {
	path := "/run/hispowerd/frame-stats"
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var frames []float64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		v, err := strconv.ParseFloat(strings.TrimSpace(sc.Text()), 64)
		if err == nil && v > 0 {
			frames = append(frames, v)
		}
	}
	return frames, sc.Err()
}

// readMangoHudFrames reads the newest MangoHud CSV log and extracts frame times.
func (fp *FramePredictor) readMangoHudFrames() ([]float64, error) {
	logPath := fp.newestMangoHudLog()
	if logPath == "" {
		return nil, fmt.Errorf("no MangoHud log found")
	}

	f, err := os.Open(logPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var frames []float64
	sc := bufio.NewScanner(f)
	frametimeIdx := -1
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Split(line, ",")

		// Find header row.
		if frametimeIdx < 0 {
			for i, h := range fields {
				if strings.TrimSpace(h) == "frametime" {
					frametimeIdx = i
				}
			}
			continue
		}

		if frametimeIdx >= len(fields) {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(fields[frametimeIdx]), 64)
		if err == nil && v > 0 {
			frames = append(frames, v)
		}
	}
	// Return only the most recent frameRingSize entries.
	if len(frames) > frameRingSize {
		frames = frames[len(frames)-frameRingSize:]
	}
	return frames, sc.Err()
}

// newestMangoHudLog returns the path to the most recently modified MangoHud CSV.
func (fp *FramePredictor) newestMangoHudLog() string {
	var newest string
	var newestTime time.Time
	for _, dir := range fp.logDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".csv") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().After(newestTime) {
				newestTime = info.ModTime()
				newest = filepath.Join(dir, e.Name())
			}
		}
	}
	return newest
}

// percentile computes the Nth percentile of a sorted slice.
func percentile(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (p * len(sorted)) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
