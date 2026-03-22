// api/journal.go — Journal telemetry SSE stream handler
//
// Route:
//   GET /api/journal/stream — streams hisnos-* journal entries as Server-Sent Events
//
// Query parameters:
//   since=<duration>   — how far back to start (e.g. "1h", "24h"; default: "1h")
//   tags=<csv>         — comma-separated syslog identifiers to filter
//                        (default: all hisnos-* tags)
//
// SSE event format:
//   Connected confirmation:
//     event: connected
//     data: {"tags":["hisnos-vault",...]}
//
//   Journal entries (journalctl --output=json lines forwarded as-is):
//     data: {"MESSAGE":"VAULT_LOCKED trigger=screen-lock",...}
//
//   Stream closed (process exited):
//     event: closed
//     data: {}
//
// Security:
//   - since parameter validated: digits + h/m/s/d only (no shell injection)
//   - tags validated: alphanumeric + hyphen/underscore only
//   - journalctl path is hardcoded absolute (no PATH lookup)

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	execpkg "hisnos.local/dashboard/exec"
)

const journalctlBin = "/usr/bin/journalctl"

// defaultJournalTags are the hisnos-* syslog identifiers emitted across all scripts.
var defaultJournalTags = []string{
	"hisnos-vault",
	"hisnos-vault-watcher",
	"hisnos-vault-gamemode",
	"hisnos-egress",
	"hisnos-observe",
	"hisnos-update",
	"hisnos-update-preflight",
	"hisnos-update-check",
	"hisnos-lab-runtime",
	"hisnos-lab-netd",
	"hisnos-lab-dns",
	"hisnos-logd",
}

// JournalStream streams hisnos-* journal entries as Server-Sent Events.
func (h *Handler) JournalStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "response streaming not supported")
		return
	}

	// ── Parameter validation ───────────────────────────────────────────────
	since := r.URL.Query().Get("since")
	if since == "" {
		since = "1h"
	}
	if !isValidDuration(since) {
		writeError(w, http.StatusBadRequest, "invalid since parameter (use e.g. 1h, 24h, 7d)")
		return
	}

	tags := defaultJournalTags
	if customTags := r.URL.Query().Get("tags"); customTags != "" {
		tags = filterValidTags(strings.Split(customTags, ","))
		if len(tags) == 0 {
			writeError(w, http.StatusBadRequest, "no valid tags provided")
			return
		}
	}

	// ── Build journalctl command ────────────────────────────────────────────
	args := []string{
		journalctlBin,
		"--follow",
		"--output=json",
		"--since=-" + since,
	}
	for _, t := range tags {
		args = append(args, "-t", t)
	}

	// ── Start SSE stream ────────────────────────────────────────────────────
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	tagsJSON, _ := json.Marshal(tags)
	fmt.Fprintf(w, "event: connected\ndata: {\"tags\":%s}\n\n", tagsJSON)
	flusher.Flush()

	lines, err := execpkg.Stream(r.Context(), args)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", sseString(err.Error()))
		flusher.Flush()
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-lines:
			if !ok {
				fmt.Fprintf(w, "event: closed\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			// Forward journalctl JSON line as SSE data.
			// Validate it's well-formed JSON before forwarding; wrap as
			// plain message if not (e.g., journalctl startup output).
			fmt.Fprintf(w, "data: %s\n\n", sanitizeJournalLine(line))
			flusher.Flush()
		}
	}
}

// ── Input validation ──────────────────────────────────────────────────────────

// isValidDuration accepts only digits and time unit letters (h, m, s, d).
// This prevents any shell metacharacters from reaching journalctl arguments.
func isValidDuration(s string) bool {
	if len(s) == 0 || len(s) > 10 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || c == 'h' || c == 'm' || c == 's' || c == 'd') {
			return false
		}
	}
	return true
}

// filterValidTags returns only tags that match [a-zA-Z0-9_-] (syslog identifier chars).
func filterValidTags(tags []string) []string {
	valid := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if isValidTag(t) {
			valid = append(valid, t)
		}
	}
	return valid
}

func isValidTag(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// sanitizeJournalLine verifies the line is valid JSON (journalctl --output=json
// produces one object per line). If parsing fails, wraps the raw text as a
// MESSAGE field so SSE framing is never broken by embedded newlines.
func sanitizeJournalLine(line string) string {
	var v any
	if json.Unmarshal([]byte(line), &v) == nil {
		return line
	}
	b, _ := json.Marshal(map[string]string{"MESSAGE": line})
	return string(b)
}
