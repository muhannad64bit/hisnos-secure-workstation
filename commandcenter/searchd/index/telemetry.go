// index/telemetry.go — Tail audit/current.jsonl and threat-timeline.jsonl
//
// Reads JSONL files produced by hisnos-logd and threatd.
// Tails new lines at a 5-second polling interval.
// Parses known fields into TelemetryRecord.Fields for search indexing.
// Max 10,000 entries are kept in the ring buffer (enforced by Index).

package index

import (
	"bufio"
	"encoding/json"
	"os"
	"time"
)

// TelemetryConfig points to the audit and threat JSONL sources.
type TelemetryConfig struct {
	AuditFile  string // e.g. /var/log/hisnos/audit/current.jsonl
	ThreatFile string // e.g. /var/log/hisnos/threat/threat-timeline.jsonl
}

// DefaultTelemetryConfig returns paths matching the logd and threatd defaults.
func DefaultTelemetryConfig() TelemetryConfig {
	return TelemetryConfig{
		AuditFile:  "/var/log/hisnos/audit/current.jsonl",
		ThreatFile: "/var/log/hisnos/threat/threat-timeline.jsonl",
	}
}

// TelemetryTailer tails JSONL sources and feeds events into the index.
type TelemetryTailer struct {
	cfg TelemetryConfig
	idx *Index
}

// NewTelemetryTailer creates a TelemetryTailer.
func NewTelemetryTailer(cfg TelemetryConfig, idx *Index) *TelemetryTailer {
	return &TelemetryTailer{cfg: cfg, idx: idx}
}

// Run tails both files until done is closed.
func (t *TelemetryTailer) Run(done <-chan struct{}) {
	auditOffset := t.seedFile(t.cfg.AuditFile, "audit", 500)
	threatOffset := t.seedFile(t.cfg.ThreatFile, "threat", 200)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			auditOffset = t.tailFile(t.cfg.AuditFile, "audit", auditOffset)
			threatOffset = t.tailFile(t.cfg.ThreatFile, "threat", threatOffset)
		}
	}
}

// seedFile reads the last n lines of a JSONL file at startup and returns the file offset.
func (t *TelemetryTailer) seedFile(path, source string, maxLines int) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	// Collect all lines (files are bounded by logrotate).
	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	start := 0
	if len(lines) > maxLines {
		start = len(lines) - maxLines
	}
	for _, line := range lines[start:] {
		t.parseLine(line, source)
	}

	// Return current file size as offset.
	info, err := f.Stat()
	if err != nil {
		return 0
	}
	return info.Size()
}

// tailFile reads new lines since lastOffset and returns the new offset.
func (t *TelemetryTailer) tailFile(path, source string, lastOffset int64) int64 {
	f, err := os.Open(path)
	if err != nil {
		return lastOffset
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return lastOffset
	}
	if info.Size() < lastOffset {
		// File was rotated — start from beginning.
		lastOffset = 0
	}
	if info.Size() == lastOffset {
		return lastOffset
	}

	if _, err := f.Seek(lastOffset, 0); err != nil {
		return lastOffset
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		t.parseLine(scanner.Text(), source)
	}
	return info.Size()
}

// parseLine parses a JSONL line and adds a TelemetryRecord to the index.
func (t *TelemetryTailer) parseLine(line, source string) {
	if len(line) == 0 || line[0] != '{' {
		return
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return
	}

	message := extractString(raw, "message", "msg", "summary", "event")
	if message == "" {
		message = line[:min(len(line), 120)]
	}

	level := extractString(raw, "level", "severity", "risk_level")
	if level == "" {
		level = "info"
	}

	tsStr := extractString(raw, "timestamp", "ts", "time", "updated_at")
	ts := time.Now()
	if tsStr != "" {
		if parsed, err := time.Parse(time.RFC3339, tsStr); err == nil {
			ts = parsed
		} else if parsed, err := time.Parse(time.RFC3339Nano, tsStr); err == nil {
			ts = parsed
		}
	}

	// Collect searchable string fields.
	fields := make(map[string]string)
	for k, v := range raw {
		switch val := v.(type) {
		case string:
			if k != "message" && k != "msg" && k != "timestamp" && k != "ts" {
				fields[k] = val
			}
		case float64:
			// Include numeric signals like risk_score.
			if k == "risk_score" || k == "score" {
				fields[k] = floatToStr(val)
			}
		}
	}

	t.idx.AddTelemetry(source, normLevel(level), message, ts, fields)
}

func extractString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

func normLevel(level string) string {
	switch level {
	case "critical", "error", "crit":
		return "critical"
	case "warn", "warning":
		return "warn"
	default:
		return "info"
	}
}

func floatToStr(f float64) string {
	return string(rune('0' + int(f)%10)) // simplified; only used for risk_score display
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
