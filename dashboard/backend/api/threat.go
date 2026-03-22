// api/threat.go — Threat Intelligence and Risk Scoring API
//
// Routes:
//   GET /api/threat/status    — current ThreatState from threat-state.json
//   GET /api/threat/timeline  — last 720 timeline entries (6h at 30s cadence)
//
// Data source: /var/lib/hisnos/threat-state.json and
// /var/lib/hisnos/threat-timeline.jsonl, both written by hisnos-threatd.
//
// Safety: if threatd has not started or either file does not exist, the
// endpoints return a zeroed/empty response rather than an error. The threat
// pipeline must never impair workstation usability.

package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
)

const (
	threatStateFilename    = "threat-state.json"
	threatTimelineFilename = "threat-timeline.jsonl"
	// timelineMaxEntries: 720 entries = 6 hours at the default 30-second cadence.
	timelineMaxEntries = 720
)

// zeroThreatState returns the default state served before threatd has written
// its first evaluation. All signals are false; score and level are zeroed.
func zeroThreatState() map[string]any {
	return map[string]any{
		"current_risk_level": "low",
		"risk_score":         0,
		"signals": map[string]bool{
			"lab_session_active": false,
			"ns_burst":           false,
			"fw_block_rate":      false,
			"nft_modified":       false,
			"vault_exposure":     false,
			"priv_exec_burst":    false,
		},
		"score_breakdown": map[string]int{
			"lab_session_active": 0,
			"ns_burst":           0,
			"fw_block_rate":      0,
			"nft_modified":       0,
			"vault_exposure":     0,
			"priv_exec_burst":    0,
		},
		"event_count": 0,
		"updated_at":  "",
	}
}

// ThreatStatus returns the current ThreatState written by hisnos-threatd.
// The file is read and forwarded as-is (JSON passthrough) after content
// validation. If the file does not exist, a zeroed state is returned.
func (h *Handler) ThreatStatus(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(h.threatDir, threatStateFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, zeroThreatState())
			return
		}
		writeError(w, http.StatusInternalServerError, "cannot read threat state")
		return
	}

	// Validate well-formed JSON before forwarding.
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		writeJSON(w, http.StatusOK, zeroThreatState())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// ThreatTimeline returns the last timelineMaxEntries entries from
// threat-timeline.jsonl as a JSON array, newest last (chronological order).
// Returns an empty array if the file does not exist or is empty.
func (h *Handler) ThreatTimeline(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(h.threatDir, threatTimelineFilename)

	entries, err := readTimelineTail(path, timelineMaxEntries)
	if err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, "cannot read threat timeline")
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// readTimelineTail reads up to maxEntries lines from the tail of a JSONL file
// and returns them as a slice of decoded JSON objects. Lines that fail to parse
// are silently skipped.
func readTimelineTail(path string, maxEntries int) ([]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Collect all lines — threat-timeline.jsonl is pruned to 48h at startup
	// (≈690 KB worst case), so a full scan is acceptable.
	var rawLines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 32*1024), 32*1024)
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			rawLines = append(rawLines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// Take the tail.
	if len(rawLines) > maxEntries {
		rawLines = rawLines[len(rawLines)-maxEntries:]
	}

	// Decode each line to any (preserves all fields from threatd without
	// requiring the dashboard to mirror the exact struct).
	entries := make([]any, 0, len(rawLines))
	for _, raw := range rawLines {
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			continue
		}
		entries = append(entries, v)
	}
	return entries, nil
}
