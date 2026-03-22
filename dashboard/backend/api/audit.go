// api/audit.go — Security Telemetry & Audit Pipeline API
//
// Routes:
//   GET /api/audit/summary         — pipeline health + storage metrics
//   GET /api/audit/sessions        — recent lab session lifecycle events
//   GET /api/audit/firewall-events — recent firewall/audit kernel events
//
// Data source: /var/lib/hisnos/audit/current.jsonl written by hisnos-logd.
// Each line is one AuditEvent JSON object (normalized by logd from journald).
// The audit directory path is configurable via HISNOS_AUDIT_DIR env (default:
// /var/lib/hisnos/audit).
//
// Safety: if hisnos-logd is not running or the file does not exist, all three
// endpoints return empty/zero results rather than errors. The audit pipeline
// must not break workstation usability.

package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	auditCurrentFile = "current.jsonl"
	// auditTailLines is the maximum number of lines read from current.jsonl per
	// query. current.jsonl is rotated at 50 MB, so this is always a bounded scan.
	auditTailLines = 5000
)

// auditLogEvent mirrors the JSON envelope written by hisnos-logd.
type auditLogEvent struct {
	Timestamp string `json:"timestamp"`
	Subsystem string `json:"subsystem"`
	Severity  string `json:"severity"`
	SessionID string `json:"session_id,omitempty"`
	Message   string `json:"message"`
}

// AuditSummary is the response for GET /api/audit/summary.
type AuditSummary struct {
	LogdActive    bool   `json:"logd_active"`
	AuditdActive  bool   `json:"auditd_active"`
	SegmentCount  int    `json:"segment_count"`
	TotalBytes    int64  `json:"total_bytes"`
	OldestSegment string `json:"oldest_segment,omitempty"`
	NewestSegment string `json:"newest_segment,omitempty"`
	AuditDir      string `json:"audit_dir"`
}

// LabSessionRecord is a session summary built from lab lifecycle events in the log.
type LabSessionRecord struct {
	SessionID  string `json:"session_id"`
	Start      string `json:"start,omitempty"`
	Stop       string `json:"stop,omitempty"`
	NetProfile string `json:"net_profile,omitempty"`
	EventCount int    `json:"event_count"`
}

// AuditSummary returns pipeline health and storage metrics.
func (h *Handler) AuditSummary(w http.ResponseWriter, r *http.Request) {
	entries, _ := os.ReadDir(h.auditDir) // ignore error — dir may not exist yet

	var totalBytes int64
	var oldest, newest string
	segCount := 0

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name != auditCurrentFile &&
			!strings.HasSuffix(name, ".jsonl") &&
			!strings.HasSuffix(name, ".jsonl.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		totalBytes += info.Size()
		segCount++
		modStr := info.ModTime().UTC().Format(time.RFC3339)
		if oldest == "" || modStr < oldest {
			oldest = modStr
		}
		if newest == "" || modStr > newest {
			newest = modStr
		}
	}

	writeJSON(w, http.StatusOK, AuditSummary{
		LogdActive:    isUserServiceActive("hisnos-logd.service"),
		AuditdActive:  isSystemServiceActive("auditd.service"),
		SegmentCount:  segCount,
		TotalBytes:    totalBytes,
		OldestSegment: oldest,
		NewestSegment: newest,
		AuditDir:      h.auditDir,
	})
}

// AuditSessions returns recent lab session lifecycle records derived from the
// audit log. Sessions are grouped by session_id and returned newest-first.
func (h *Handler) AuditSessions(w http.ResponseWriter, r *http.Request) {
	lines, err := tailAuditFile(h.auditDir, auditTailLines)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, []LabSessionRecord{})
			return
		}
		writeError(w, http.StatusInternalServerError, "cannot read audit log")
		return
	}

	sessions := map[string]*LabSessionRecord{}
	var order []string

	for _, line := range lines {
		var ev auditLogEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Subsystem != "lab" || ev.SessionID == "" {
			continue
		}
		rec, exists := sessions[ev.SessionID]
		if !exists {
			rec = &LabSessionRecord{SessionID: ev.SessionID}
			sessions[ev.SessionID] = rec
			order = append(order, ev.SessionID)
		}
		rec.EventCount++
		if strings.Contains(ev.Message, "HISNOS_LAB_STARTED") {
			rec.Start = ev.Timestamp
			if np := auditExtractKV(ev.Message, "net_profile"); np != "" {
				rec.NetProfile = np
			}
		}
		if strings.Contains(ev.Message, "HISNOS_LAB_STOPPED") ||
			strings.Contains(ev.Message, "HISNOS_LAB_CLEANUP") {
			rec.Stop = ev.Timestamp
		}
	}

	// Return most-recent session first.
	result := make([]LabSessionRecord, 0, len(order))
	for i := len(order) - 1; i >= 0; i-- {
		result = append(result, *sessions[order[i]])
	}
	writeJSON(w, http.StatusOK, result)
}

// AuditFirewallEvents returns the last 100 firewall and kernel audit events.
func (h *Handler) AuditFirewallEvents(w http.ResponseWriter, r *http.Request) {
	lines, err := tailAuditFile(h.auditDir, auditTailLines)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, []auditLogEvent{})
			return
		}
		writeError(w, http.StatusInternalServerError, "cannot read audit log")
		return
	}

	var events []auditLogEvent
	for _, line := range lines {
		var ev auditLogEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Subsystem == "firewall" || ev.Subsystem == "audit" {
			events = append(events, ev)
		}
	}
	// Cap at last 100 matching events.
	if len(events) > 100 {
		events = events[len(events)-100:]
	}
	writeJSON(w, http.StatusOK, events)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// tailAuditFile reads up to maxLines lines from the tail of current.jsonl.
// current.jsonl is rotated at 50 MB, so a full scan is bounded.
func tailAuditFile(auditDir string, maxLines int) ([]string, error) {
	path := filepath.Join(auditDir, auditCurrentFile)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines, nil
}

// isUserServiceActive returns true if the given systemd --user unit is active.
func isUserServiceActive(unit string) bool {
	cmd := exec.Command("/usr/bin/systemctl", "--user", "is-active", "--quiet", unit)
	return cmd.Run() == nil
}

// isSystemServiceActive returns true if the given system-level unit is active.
// Checking status does not require elevated privileges.
func isSystemServiceActive(unit string) bool {
	cmd := exec.Command("/usr/bin/systemctl", "is-active", "--quiet", unit)
	return cmd.Run() == nil
}

// auditExtractKV parses key=value from a log message string.
func auditExtractKV(msg, key string) string {
	prefix := key + "="
	idx := strings.Index(msg, prefix)
	if idx < 0 {
		return ""
	}
	rest := msg[idx+len(prefix):]
	end := strings.IndexAny(rest, " \t\n")
	if end < 0 {
		return rest
	}
	return rest[:end]
}
