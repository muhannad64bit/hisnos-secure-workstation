// core/telemetry/observability_bus.go — Full observability event bus with correlation IDs.
//
// The ObservabilityBus is the central event routing system for HisnOS.
// All subsystems emit events through this bus rather than directly to journald,
// enabling:
//
//   - Correlation ID injection: every event chain is tagged with a UUID-like
//     correlation ID so timeline reconstruction is possible
//   - Timeline reconstruction: events can be queried by correlation ID to
//     replay the full causal chain
//   - Multi-sink fan-out: events go to journald (via log), the IPC event
//     stream (for dashboard), and optional external sinks
//   - Taxonomy enforcement: event categories and types are validated against
//     the registered taxonomy
//
// Event structure:
//
//	{
//	  "correlation_id": "c7f3a1b2",      // shared across related events
//	  "event_id":       "e9d1c0f4",      // unique per event
//	  "ts":             "2026-03-22T...", // RFC3339Nano
//	  "category":       "security",
//	  "event":          "rt_escalation_blocked",
//	  "data":           {...},
//	  "source":         "rt_guard"
//	}
//
// Correlation ID lifecycle:
//   - StartCorrelation(reason) → returns a new correlation ID
//   - Emit with correlation ID → events inherit the same ID
//   - EndCorrelation(id)       → marks the chain complete
//
// Timeline reconstruction:
//   - QueryByCorrelation(id) → returns all events for that chain (in-memory)
//   - The in-memory timeline buffer holds the last timelineCapacity events
//
// Taxonomy: validated category+event combinations.
// Unknown combinations are accepted but tagged with "unknown=true" for triage.
package telemetry

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"
)

const (
	timelineCapacity = 2000 // ring buffer for recent events
	maxSinksCount    = 16
)

// BusEvent is one structured event on the observability bus.
type BusEvent struct {
	CorrelationID string         `json:"correlation_id"`
	EventID       string         `json:"event_id"`
	Timestamp     time.Time      `json:"ts"`
	Category      string         `json:"category"`
	Event         string         `json:"event"`
	Data          map[string]any `json:"data,omitempty"`
	Source        string         `json:"source,omitempty"`
	UnknownType   bool           `json:"unknown,omitempty"`
}

// EventSink receives events from the bus.
type EventSink interface {
	// Receive is called synchronously for each event.
	// Implementations must not block; use buffered channels internally.
	Receive(evt BusEvent)
}

// EventSinkFunc adapts a function to the EventSink interface.
type EventSinkFunc func(BusEvent)

func (f EventSinkFunc) Receive(evt BusEvent) { f(evt) }

// ObservabilityBus routes events to all registered sinks.
type ObservabilityBus struct {
	mu       sync.RWMutex
	sinks    []EventSink
	timeline [timelineCapacity]BusEvent
	tlHead   int
	tlFull   bool
	// Active correlations: id → start time + reason
	correlations map[string]correlationEntry
}

type correlationEntry struct {
	Reason    string
	StartedAt time.Time
}

// NewObservabilityBus creates a bus with a journald sink pre-registered.
func NewObservabilityBus() *ObservabilityBus {
	ob := &ObservabilityBus{
		correlations: make(map[string]correlationEntry),
	}
	// Always attach journald (structured log) sink.
	ob.RegisterSink(EventSinkFunc(journaldSink))
	return ob
}

// RegisterSink adds a new event sink. Thread-safe.
func (ob *ObservabilityBus) RegisterSink(sink EventSink) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if len(ob.sinks) >= maxSinksCount {
		log.Printf("[obs-bus] WARN: max sinks reached, ignoring RegisterSink")
		return
	}
	ob.sinks = append(ob.sinks, sink)
}

// StartCorrelation begins a new correlated event chain.
// Returns the correlation ID to pass to subsequent Emit calls.
func (ob *ObservabilityBus) StartCorrelation(reason string) string {
	id := newID()
	ob.mu.Lock()
	ob.correlations[id] = correlationEntry{Reason: reason, StartedAt: time.Now()}
	ob.mu.Unlock()
	log.Printf("[obs-bus] correlation %s started: %s", id, reason)
	return id
}

// EndCorrelation marks a correlated chain as complete.
func (ob *ObservabilityBus) EndCorrelation(id string) {
	ob.mu.Lock()
	entry, ok := ob.correlations[id]
	delete(ob.correlations, id)
	ob.mu.Unlock()
	if ok {
		duration := time.Since(entry.StartedAt)
		log.Printf("[obs-bus] correlation %s ended (%v, reason=%s)", id, duration.Round(time.Millisecond), entry.Reason)
	}
}

// Emit broadcasts one event on the bus.
// correlationID may be "" to generate a standalone event.
func (ob *ObservabilityBus) Emit(correlationID, source, category, event string, data map[string]any) {
	if correlationID == "" {
		correlationID = newID()
	}
	evt := BusEvent{
		CorrelationID: correlationID,
		EventID:       newID(),
		Timestamp:     time.Now(),
		Category:      category,
		Event:         event,
		Data:          data,
		Source:        source,
		UnknownType:   !isKnownEvent(category, event),
	}

	ob.mu.Lock()
	ob.timeline[ob.tlHead] = evt
	ob.tlHead = (ob.tlHead + 1) % timelineCapacity
	if ob.tlHead == 0 {
		ob.tlFull = true
	}
	sinks := ob.sinks
	ob.mu.Unlock()

	for _, s := range sinks {
		s.Receive(evt)
	}
}

// EmitFunc returns a convenience emit function bound to a fixed source.
// This is what subsystems receive as their "emit" parameter.
func (ob *ObservabilityBus) EmitFunc(source string) func(string, string, map[string]any) {
	return func(category, event string, data map[string]any) {
		ob.Emit("", source, category, event, data)
	}
}

// EmitCorrelated returns an emit function that tags all events with correlationID.
func (ob *ObservabilityBus) EmitCorrelated(correlationID, source string) func(string, string, map[string]any) {
	return func(category, event string, data map[string]any) {
		ob.Emit(correlationID, source, category, event, data)
	}
}

// QueryByCorrelation returns all timeline events matching the given correlation ID.
func (ob *ObservabilityBus) QueryByCorrelation(correlationID string) []BusEvent {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	var out []BusEvent
	n := timelineCapacity
	if !ob.tlFull {
		n = ob.tlHead
	}
	for i := 0; i < n; i++ {
		evt := ob.timeline[i]
		if evt.CorrelationID == correlationID {
			out = append(out, evt)
		}
	}
	return out
}

// RecentEvents returns the last n events from the timeline (newest first).
func (ob *ObservabilityBus) RecentEvents(n int) []BusEvent {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	total := timelineCapacity
	if !ob.tlFull {
		total = ob.tlHead
	}
	if n > total {
		n = total
	}
	out := make([]BusEvent, 0, n)
	for i := 1; i <= n; i++ {
		idx := (ob.tlHead - i + timelineCapacity) % timelineCapacity
		out = append(out, ob.timeline[idx])
	}
	return out
}

// Status returns a summary for IPC.
func (ob *ObservabilityBus) Status() map[string]any {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	total := timelineCapacity
	if !ob.tlFull {
		total = ob.tlHead
	}
	return map[string]any{
		"timeline_events":       total,
		"timeline_capacity":     timelineCapacity,
		"sinks":                 len(ob.sinks),
		"active_correlations":   len(ob.correlations),
	}
}

// ─── event taxonomy ──────────────────────────────────────────────────────────

// knownEvents is the authoritative taxonomy of category → event type mappings.
// Unmapped events are accepted but flagged unknown=true.
var knownEvents = map[string]map[string]bool{
	"security": {
		"rt_escalation_blocked":    true,
		"threat_score_elevated":    true,
		"threat_score_critical":    true,
		"safe_mode_entered":        true,
		"safe_mode_exited":         true,
		"containment_applied":      true,
		"namespace_anomaly":        true,
		"privilege_escalation":     true,
		"kernel_exploit_attempt":   true,
		"lateral_movement_detected": true,
		"exfil_detected":           true,
		"persistence_detected":     true,
	},
	"performance": {
		"profile_applied":          true,
		"profile_reverted":         true,
		"thermal_throttle_detected": true,
		"thermal_critical":         true,
		"frame_jitter_spike":       true,
		"irq_rebalanced":          true,
		"numa_pins_applied":        true,
	},
	"automation": {
		"baseline_active":           true,
		"baseline_anomaly_detected": true,
		"threat_momentum_warning":   true,
		"threat_momentum_critical":  true,
		"threat_momentum_emergency": true,
		"attack_session_opened":     true,
		"attack_session_closed":     true,
		"action_pending_confirmation": true,
		"automation_override":       true,
	},
	"health": {
		"boot_recorded":             true,
		"boot_reliability_degraded": true,
		"service_restarted":         true,
		"service_failed":            true,
		"safe_mode_triggered":       true,
	},
	"orchestrator": {
		"snapshot_taken":   true,
		"snapshot_partial": true,
		"rollback_started": true,
		"rollback_complete": true,
		"rollback_partial": true,
	},
	"ecosystem": {
		"update_available":    true,
		"update_staged":       true,
		"update_applied":      true,
		"rollback_suggested":  true,
	},
	"marketplace": {
		"plugin_installed":   true,
		"plugin_uninstalled": true,
		"plugin_enabled":     true,
		"plugin_disabled":    true,
	},
	"fleet": {
		"stale_policy_warning": true,
		"rule_applied":         true,
		"rule_apply_failed":    true,
	},
	"vault": {
		"unlocked": true,
		"locked":   true,
	},
	"egress": {
		"rule_blocked": true,
		"gaming_mode":  true,
	},
}

func isKnownEvent(category, event string) bool {
	events, ok := knownEvents[category]
	if !ok {
		return false
	}
	return events[event]
}

// ─── journald sink ───────────────────────────────────────────────────────────

func journaldSink(evt BusEvent) {
	dataJSON, _ := json.Marshal(evt.Data)
	log.Printf("[obs] corr=%s src=%s cat=%s evt=%s data=%s",
		evt.CorrelationID[:8], evt.Source, evt.Category, evt.Event, dataJSON)
}

// ─── ID generation ───────────────────────────────────────────────────────────

// newID generates an 8-character hex ID (32-bit random). Not cryptographically
// secure; used for correlation and event tracking only.
func newID() string {
	n := rand.Uint32()
	return fmt.Sprintf("%08x", n)
}
