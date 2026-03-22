// core/eventbus/eventbus.go — Non-blocking pub/sub event bus for hisnosd.
//
// Design contract:
//   - Publish is never blocking; slow subscribers receive a warning log and
//     the event is dropped for that subscriber (fan-out continues).
//   - Each subscriber gets its own buffered channel (subscriberBufSize).
//   - Subscribe("") receives every event type (wildcard).
//   - Unsubscribe is intentionally omitted for MVP — subscribers are long-lived
//     goroutines whose lifetimes match the daemon lifetime.

package eventbus

import (
	"log"
	"sync"
	"time"
)

// EventType names a specific lifecycle or state-change event.
type EventType string

const (
	EventVaultMounted     EventType = "VaultMounted"
	EventVaultLocked      EventType = "VaultLocked"
	EventLabStarted       EventType = "LabStarted"
	EventLabStopped       EventType = "LabStopped"
	EventGamingStarted    EventType = "GamingStarted"
	EventGamingStopped    EventType = "GamingStopped"
	EventFirewallReloaded EventType = "FirewallReloaded"
	EventFirewallDead     EventType = "FirewallDead"
	EventUpdatePrepared   EventType = "UpdatePrepared"
	EventUpdateApplied    EventType = "UpdateApplied"
	EventRiskScoreChanged EventType = "RiskScoreChanged"
	EventSubsystemCrashed EventType = "SubsystemCrashed"
	EventSubsystemRestored EventType = "SubsystemRestored"
	EventSafeModeEntered  EventType = "SafeModeEntered"
	EventSafeModeExited   EventType = "SafeModeExited"
	EventModeChanged      EventType = "ModeChanged"
	EventPolicyAction     EventType = "PolicyAction"
	EventStateCorruption  EventType = "StateCorruption"
)

// Event carries a typed payload between components.
type Event struct {
	Type      EventType
	Timestamp time.Time
	Payload   map[string]any
}

// subscriberBufSize is the per-subscriber channel depth.
// At 15s supervision intervals and 30s eval cycles, 64 is ample.
const subscriberBufSize = 64

type sub struct {
	ch     chan Event
	filter EventType // empty string = all events
}

// Bus is a goroutine-safe non-blocking pub/sub event bus.
type Bus struct {
	mu   sync.RWMutex
	subs []*sub
}

// New returns a ready Bus.
func New() *Bus {
	return &Bus{}
}

// Subscribe returns a channel that will receive events of the given type.
// Pass an empty string to receive all events.
// The returned channel is closed when the Bus is garbage-collected (never in
// practice — hisnosd runs until SIGTERM).
func (b *Bus) Subscribe(filter EventType) <-chan Event {
	ch := make(chan Event, subscriberBufSize)
	b.mu.Lock()
	b.subs = append(b.subs, &sub{ch: ch, filter: filter})
	b.mu.Unlock()
	return ch
}

// Publish fans out ev to all matching subscribers without blocking.
// Events are dropped (with a warning log) for subscribers whose buffer is full.
func (b *Bus) Publish(ev Event) {
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	b.mu.RLock()
	subs := make([]*sub, len(b.subs))
	copy(subs, b.subs)
	b.mu.RUnlock()

	for _, s := range subs {
		if s.filter != "" && s.filter != ev.Type {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			log.Printf("[hisnosd/eventbus] WARN: subscriber buffer full for event %s — dropped", ev.Type)
		}
	}
}

// Emit is a convenience wrapper: build and publish an event.
func (b *Bus) Emit(t EventType, payload map[string]any) {
	b.Publish(Event{
		Type:      t,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	})
}
