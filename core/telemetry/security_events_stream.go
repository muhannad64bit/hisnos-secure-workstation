// core/telemetry/security_events_stream.go
//
// SecurityEventStream is the unified observability tap for all HisnOS security events.
//
// Architecture:
//   - All subsystems call Emit() to record a security event.
//   - Events are stored in an in-process ring buffer (capacity 10,000).
//   - Subscribers receive events via typed channels (fan-out).
//   - Events are flushed to the systemd journal via the native UDP socket.
//   - Correlation IDs link related events across subsystems.
//
// Event flow:
//   [Subsystem] → Emit() → RingBuffer → fan-out → [Subscriber chans]
//                                    → Journal native protocol
//                                    → /var/log/hisnos/security-events.jsonl
//
// All operations are non-blocking from the caller's perspective.
// If the ring buffer is full, the oldest event is evicted (FIFO drop).

package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ── Severity ──────────────────────────────────────────────────────────────

// Severity orders the urgency of security events.
type Severity int

const (
	SeverityDebug    Severity = 0
	SeverityInfo     Severity = 1
	SeverityWarn     Severity = 2
	SeverityAlert    Severity = 3
	SeverityCritical Severity = 4
)

var severityName = map[Severity]string{
	SeverityDebug:    "DEBUG",
	SeverityInfo:     "INFO",
	SeverityWarn:     "WARN",
	SeverityAlert:    "ALERT",
	SeverityCritical: "CRITICAL",
}

func (s Severity) String() string {
	if n, ok := severityName[s]; ok {
		return n
	}
	return "UNKNOWN"
}

// ── Event ─────────────────────────────────────────────────────────────────

// Event is a structured security telemetry record.
type Event struct {
	ID            string         `json:"id"`
	CorrelationID string         `json:"correlation_id,omitempty"`
	Timestamp     time.Time      `json:"timestamp"`
	Severity      Severity       `json:"severity"`
	SeverityName  string         `json:"severity_name"`
	Category      string         `json:"category"`    // e.g. "vault", "firewall", "threat"
	Source        string         `json:"source"`      // subsystem that emitted the event
	Message       string         `json:"message"`
	Details       map[string]any `json:"details,omitempty"`
	RiskScore     float64        `json:"risk_score,omitempty"`
}

const ringCapacity = 10_000

// ── Stream ────────────────────────────────────────────────────────────────

// SecurityEventStream is the central event bus for security telemetry.
type SecurityEventStream struct {
	mu          sync.RWMutex
	ring        [ringCapacity]Event
	head        int // next write position
	count       int // total events ever written
	subscribers map[string]chan Event

	logPath string
	logFile *os.File
}

// NewSecurityEventStream creates a stream that writes to logPath.
func NewSecurityEventStream(logPath string) *SecurityEventStream {
	s := &SecurityEventStream{
		subscribers: make(map[string]chan Event),
		logPath:     logPath,
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0750); err != nil {
		log.Printf("[eventstream] WARN: cannot create log dir: %v", err)
	} else {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
		if err != nil {
			log.Printf("[eventstream] WARN: cannot open log file %s: %v", logPath, err)
		} else {
			s.logFile = f
		}
	}

	return s
}

// NewCorrelationID generates a random 8-byte hex correlation ID.
func NewCorrelationID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "cid-" + hex.EncodeToString(b)
}

// Emit records a security event, fans it out to subscribers, and writes it
// to the journal and JSONL log.  Non-blocking — caller is never delayed.
func (s *SecurityEventStream) Emit(ev Event) {
	if ev.ID == "" {
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		ev.ID = "ev-" + hex.EncodeToString(b)
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	ev.SeverityName = ev.Severity.String()

	s.mu.Lock()
	// Write into ring buffer (evicts oldest on overflow).
	s.ring[s.head%ringCapacity] = ev
	s.head++
	s.count++
	subs := make(map[string]chan Event, len(s.subscribers))
	for k, v := range s.subscribers {
		subs[k] = v
	}
	s.mu.Unlock()

	// Fan-out to subscribers (non-blocking per subscriber).
	for name, ch := range subs {
		select {
		case ch <- ev:
		default:
			log.Printf("[eventstream] WARN: subscriber %q channel full — event dropped", name)
		}
	}

	// Async I/O.
	go s.persist(ev)
}

// EmitSimple is a convenience wrapper for common use cases.
func (s *SecurityEventStream) EmitSimple(severity Severity, category, source, message string) {
	s.Emit(Event{
		Severity: severity,
		Category: category,
		Source:   source,
		Message:  message,
	})
}

// Subscribe returns a channel that receives all future events.
// The channel has a buffer of bufSize; if full, events are dropped for this subscriber.
// Call Unsubscribe(name) when done.
func (s *SecurityEventStream) Subscribe(name string, bufSize int) <-chan Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan Event, bufSize)
	s.subscribers[name] = ch
	return ch
}

// Unsubscribe removes a subscriber and closes its channel.
func (s *SecurityEventStream) Unsubscribe(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.subscribers[name]; ok {
		close(ch)
		delete(s.subscribers, name)
	}
}

// Recent returns up to n recent events, newest first.
func (s *SecurityEventStream) Recent(n int) []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if n > ringCapacity {
		n = ringCapacity
	}
	total := s.count
	if total > ringCapacity {
		total = ringCapacity
	}
	if n > total {
		n = total
	}

	out := make([]Event, n)
	for i := 0; i < n; i++ {
		// Walk backward from head.
		idx := (s.head - 1 - i + ringCapacity) % ringCapacity
		out[i] = s.ring[idx]
	}
	return out
}

// RecentByCategory returns up to n recent events matching the given category.
func (s *SecurityEventStream) RecentByCategory(category string, n int) []Event {
	all := s.Recent(ringCapacity)
	var out []Event
	for _, ev := range all {
		if ev.Category == category {
			out = append(out, ev)
			if len(out) >= n {
				break
			}
		}
	}
	return out
}

// DrainLoop reads from ctx-aware channel and calls fn on each event.
// Useful for bridges to dashboard WebSocket or journal.
func (s *SecurityEventStream) DrainLoop(ctx context.Context, subscriberName string, fn func(Event)) {
	ch := s.Subscribe(subscriberName, 256)
	defer s.Unsubscribe(subscriberName)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			fn(ev)
		}
	}
}

// Close flushes and closes the log file.
func (s *SecurityEventStream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logFile != nil {
		s.logFile.Sync()
		s.logFile.Close()
		s.logFile = nil
	}
}

// ── Persistence ───────────────────────────────────────────────────────────

// persist writes the event to the JSONL log and journals it.
// Called in a goroutine from Emit(); failures are logged but not fatal.
func (s *SecurityEventStream) persist(ev Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	line := append(data, '\n')

	// JSONL log file.
	s.mu.Lock()
	if s.logFile != nil {
		s.logFile.Write(line)
	}
	s.mu.Unlock()

	// systemd native journal protocol (UDP datagram socket).
	sendToJournal(ev, string(data))
}

// sendToJournal writes the event as a structured journal record.
// Uses the systemd native journal protocol over the unix datagram socket.
func sendToJournal(ev Event, rawJSON string) {
	const journalSocket = "/run/systemd/journal/socket"

	conn, err := net.Dial("unixgram", journalSocket)
	if err != nil {
		return
	}
	defer conn.Close()

	// Map severity to journald priority (0=emergency, 7=debug).
	var priority int
	switch ev.Severity {
	case SeverityCritical:
		priority = 2 // CRIT
	case SeverityAlert:
		priority = 3 // ERR
	case SeverityWarn:
		priority = 4 // WARNING
	case SeverityInfo:
		priority = 6 // INFO
	default:
		priority = 7 // DEBUG
	}

	msg := fmt.Sprintf("PRIORITY=%d\nSYSLOG_IDENTIFIER=hisnos-events\n"+
		"MESSAGE=%s\nHISNOS_CATEGORY=%s\nHISNOS_SOURCE=%s\n"+
		"HISNOS_SEVERITY=%s\nHISNOS_EVENT_ID=%s\nHISNOS_JSON=%s\n",
		priority, ev.Message, ev.Category, ev.Source,
		ev.SeverityName, ev.ID, rawJSON)

	conn.Write([]byte(msg))
}
