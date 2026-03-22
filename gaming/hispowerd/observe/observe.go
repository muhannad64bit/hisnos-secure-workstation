// observe/observe.go — Phase 8: structured journald event emission
//
// Implements the systemd-journald native protocol over a UNIX datagram socket.
// Each event is sent as a datagram where fields are "FIELD=value\n" concatenated.
// Falls back to stderr (journald captures it via StandardError=journal) if socket unavailable.
//
// Fields always included:
//   MESSAGE         human-readable summary
//   PRIORITY        syslog priority (6=info, 4=warning, 3=err)
//   SYSLOG_IDENTIFIER hispowerd
//   HISNOS_EVENT    event name constant
//   HISNOS_COMPONENT hispowerd

package observe

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

// journald event name constants (Phase 8).
const (
	EventGamingStart           = "HISNOS_GAMING_START"
	EventGamingStop            = "HISNOS_GAMING_STOP"
	EventCPUIsolationApplied   = "HISNOS_CPU_ISOLATION_APPLIED"
	EventCPUIsolationRestored  = "HISNOS_CPU_ISOLATION_RESTORED"
	EventIRQTuned              = "HISNOS_IRQ_TUNED"
	EventIRQRestored           = "HISNOS_IRQ_RESTORED"
	EventFirewallFastPathOn    = "HISNOS_FIREWALL_FASTPATH_ENABLED"
	EventFirewallFastPathOff   = "HISNOS_FIREWALL_FASTPATH_DISABLED"
	EventDaemonsThrottled      = "HISNOS_DAEMONS_THROTTLED"
	EventDaemonsRestored       = "HISNOS_DAEMONS_RESTORED"
	EventGovernorChanged       = "HISNOS_GOVERNOR_CHANGED"
	EventGovernorRestored      = "HISNOS_GOVERNOR_RESTORED"
	EventSessionDetected       = "HISNOS_SESSION_DETECTED"
	EventSessionLost           = "HISNOS_SESSION_LOST"
	EventCrashRecovery         = "HISNOS_CRASH_RECOVERY"
	EventSafeModeBlocked       = "HISNOS_GAMING_BLOCKED_SAFEMODE"
)

const journaldSocket = "/run/systemd/journal/socket"

// Logger wraps the journald connection with a fallback logger.
type Logger struct {
	std *log.Logger
}

// New creates a Logger.
func New() *Logger {
	return &Logger{
		std: log.New(os.Stderr, "[hispowerd] ", log.Ltime|log.Lshortfile),
	}
}

// Emit sends a structured event to journald.
func (l *Logger) Emit(event, message string, extra map[string]string) {
	fields := map[string]string{
		"PRIORITY":            "6", // INFO
		"SYSLOG_IDENTIFIER":  "hispowerd",
		"HISNOS_EVENT":       event,
		"HISNOS_COMPONENT":   "hispowerd",
		"HISNOS_TIMESTAMP":   time.Now().UTC().Format(time.RFC3339),
		"MESSAGE":            message,
	}
	for k, v := range extra {
		fields[k] = v
	}

	if err := l.sendToJournal(fields); err != nil {
		// Fallback: write to stderr, journald captures it.
		l.std.Printf("EVENT=%s %s", event, message)
		for k, v := range extra {
			l.std.Printf("  %s=%s", k, v)
		}
	}
}

// EmitWarning sends a warning-priority event.
func (l *Logger) EmitWarning(event, message string, extra map[string]string) {
	if extra == nil {
		extra = make(map[string]string)
	}
	extra["PRIORITY"] = "4" // WARNING
	l.Emit(event, message, extra)
}

// Info logs an informational message (not a structured event).
func (l *Logger) Info(format string, args ...any) {
	l.std.Printf(format, args...)
}

// Warn logs a warning message.
func (l *Logger) Warn(format string, args ...any) {
	l.std.Printf("[WARN] "+format, args...)
}

// Error logs an error message.
func (l *Logger) Error(format string, args ...any) {
	l.std.Printf("[ERROR] "+format, args...)
}

// sendToJournal sends the field map as a journald native protocol datagram.
// Protocol: each field is "FIELD=value\n", all concatenated into one datagram.
// Multi-line values have newlines replaced with spaces (simplified encoding).
func (l *Logger) sendToJournal(fields map[string]string) error {
	conn, err := net.Dial("unixgram", journaldSocket)
	if err != nil {
		return err
	}
	defer conn.Close()

	var sb strings.Builder
	// Write MESSAGE first (journald convention).
	if msg, ok := fields["MESSAGE"]; ok {
		msg = strings.ReplaceAll(msg, "\n", " ")
		fmt.Fprintf(&sb, "MESSAGE=%s\n", msg)
	}
	for k, v := range fields {
		if k == "MESSAGE" {
			continue
		}
		v = strings.ReplaceAll(v, "\n", " ")
		fmt.Fprintf(&sb, "%s=%s\n", k, v)
	}

	_, err = conn.Write([]byte(sb.String()))
	return err
}
