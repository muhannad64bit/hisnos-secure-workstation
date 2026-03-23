// core/runtime/policy_enforcer.go
//
// PolicyEnforcer is an asynchronous priority-ordered policy execution queue.
//
// This replaces the synchronous policy dispatch in main.go with a structured
// priority queue that guarantees:
//   - CRITICAL actions execute before SECURITY, before PERFORMANCE, before OPERATOR.
//   - Each action has a per-priority timeout.
//   - Actions that fail are routed to a dead-letter handler.
//   - Actions cancelled by context are removed cleanly.
//   - Dry-run mode executes the queue without performing real side effects.
//
// Priority classes (higher = earlier):
//   CRITICAL    (4) — timeout 10s  — e.g. force vault lock, enter safe-mode
//   SECURITY    (3) — timeout 30s  — e.g. firewall reload, containment
//   PERFORMANCE (2) — timeout 15s  — e.g. gaming mode changes, lab profile
//   OPERATOR    (1) — timeout 60s  — e.g. update preflight, user acknowledgement
//
// Usage:
//   enforcer := NewPolicyEnforcer(ctx, deadLetterFn, dryRun)
//   enforcer.Submit(PolicyAction{Priority: PriorityCritical, Name: "vault_lock", Exec: ...})

package runtime

import (
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ── Priority constants ─────────────────────────────────────────────────────

type Priority int

const (
	PriorityOperator    Priority = 1
	PriorityPerformance Priority = 2
	PrioritySecurity    Priority = 3
	PriorityCritical    Priority = 4
)

var priorityTimeout = map[Priority]time.Duration{
	PriorityCritical:    10 * time.Second,
	PrioritySecurity:    30 * time.Second,
	PriorityPerformance: 15 * time.Second,
	PriorityOperator:    60 * time.Second,
}

var priorityName = map[Priority]string{
	PriorityCritical:    "CRITICAL",
	PrioritySecurity:    "SECURITY",
	PriorityPerformance: "PERFORMANCE",
	PriorityOperator:    "OPERATOR",
}

// ── PolicyAction ──────────────────────────────────────────────────────────

// PolicyAction is a single executable policy decision.
type PolicyAction struct {
	// Priority determines execution order.
	Priority Priority

	// Name is a human-readable identifier for logging and dead-letter handling.
	Name string

	// Reason is the policy rationale (logged, emitted to event stream).
	Reason string

	// Exec performs the actual side effect. ctx is cancelled if the action
	// times out.  The function must return promptly on ctx cancellation.
	Exec func(ctx context.Context) error

	// DryRunExec is called instead of Exec when the enforcer is in dry-run mode.
	// If nil and dry-run is active, the action is logged and skipped.
	DryRunExec func(ctx context.Context) error

	// submittedAt is set by Submit().
	submittedAt time.Time
}

// ── Priority queue (min-heap by descending priority) ──────────────────────

type actionHeap []*PolicyAction

func (h actionHeap) Len() int { return len(h) }
func (h actionHeap) Less(i, j int) bool {
	// Higher priority first; on tie, earlier submission first.
	if h[i].Priority != h[j].Priority {
		return h[i].Priority > h[j].Priority
	}
	return h[i].submittedAt.Before(h[j].submittedAt)
}
func (h actionHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *actionHeap) Push(x any)   { *h = append(*h, x.(*PolicyAction)) }
func (h *actionHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// ── PolicyEnforcer ─────────────────────────────────────────────────────────

// PolicyEnforcer is the asynchronous policy execution runtime.
type PolicyEnforcer struct {
	mu      sync.Mutex
	queue   actionHeap
	signal  chan struct{} // poke the worker when new action arrives
	dryRun  bool

	// DeadLetter is called when an action fails or times out.
	// May be nil (failures are logged but not re-queued).
	DeadLetter func(action *PolicyAction, err error)
}

// NewPolicyEnforcer creates and starts a PolicyEnforcer.
// Call ctx.Done() to stop the worker goroutine.
func NewPolicyEnforcer(ctx context.Context, deadLetter func(*PolicyAction, error), dryRun bool) *PolicyEnforcer {
	e := &PolicyEnforcer{
		signal:     make(chan struct{}, 1),
		dryRun:     dryRun,
		DeadLetter: deadLetter,
	}
	heap.Init(&e.queue)
	go e.worker(ctx)
	return e
}

// Submit adds a PolicyAction to the priority queue.
// Thread-safe; non-blocking.
func (e *PolicyEnforcer) Submit(action PolicyAction) {
	action.submittedAt = time.Now()
	e.mu.Lock()
	heap.Push(&e.queue, &action)
	e.mu.Unlock()

	// Poke the worker.
	select {
	case e.signal <- struct{}{}:
	default:
	}

	log.Printf("[enforcer] queued action %q [%s] reason=%q",
		action.Name, priorityName[action.Priority], action.Reason)
}

// QueueLen returns the number of pending actions.
func (e *PolicyEnforcer) QueueLen() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.queue.Len()
}

// worker drains the queue sequentially, highest-priority first.
// Sequential execution is intentional: it provides deterministic ordering
// and avoids race conditions between related policy actions.
func (e *PolicyEnforcer) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			log.Printf("[enforcer] worker stopping (%d actions dropped)", e.QueueLen())
			return
		case <-e.signal:
			// Drain all available actions.
			for {
				e.mu.Lock()
				if e.queue.Len() == 0 {
					e.mu.Unlock()
					break
				}
				action := heap.Pop(&e.queue).(*PolicyAction)
				e.mu.Unlock()

				e.execute(ctx, action)
			}
		}
	}
}

func (e *PolicyEnforcer) execute(ctx context.Context, action *PolicyAction) {
	timeout := priorityTimeout[action.Priority]
	actCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pName := priorityName[action.Priority]
	log.Printf("[enforcer] executing %q [%s] reason=%q", action.Name, pName, action.Reason)

	start := time.Now()
	var err error

	if e.dryRun {
		if action.DryRunExec != nil {
			err = action.DryRunExec(actCtx)
		} else {
			log.Printf("[enforcer] DRY-RUN: would execute %q [%s]", action.Name, pName)
		}
	} else {
		err = action.Exec(actCtx)
	}

	elapsed := time.Since(start)

	if err != nil {
		if err == context.DeadlineExceeded {
			log.Printf("[enforcer] TIMEOUT: action %q [%s] timed out after %s",
				action.Name, pName, elapsed.Round(time.Millisecond))
		} else {
			log.Printf("[enforcer] FAIL: action %q [%s] error: %v (elapsed %s)",
				action.Name, pName, err, elapsed.Round(time.Millisecond))
		}
		if e.DeadLetter != nil {
			go e.DeadLetter(action, err)
		}
	} else {
		log.Printf("[enforcer] OK: action %q [%s] completed in %s",
			action.Name, pName, elapsed.Round(time.Millisecond))
	}
}

// ── Dead-letter handler factory ───────────────────────────────────────────

// LoggingDeadLetter returns a dead-letter handler that logs failures
// and emits an operator alert JSON file.
func LoggingDeadLetter(stateDir string) func(*PolicyAction, error) {
	return func(action *PolicyAction, err error) {
		msg := fmt.Sprintf("policy action %q [%s] failed: %v",
			action.Name, priorityName[action.Priority], err)
		log.Printf("[enforcer/dead-letter] %s", msg)

		alert := map[string]any{
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"type":      "policy_action_failed",
			"action":    action.Name,
			"priority":  priorityName[action.Priority],
			"reason":    action.Reason,
			"error":     err.Error(),
		}
		writeJSONAlert(stateDir+"/policy-dead-letter.json", alert)
	}
}

func writeJSONAlert(path string, data map[string]any) {
	b, _ := jsonMarshalPretty(data)
	tmp, err := createTempIn(path, ".alert-")
	if err != nil {
		return
	}
	tmp.Write(b)
	tmp.Sync()
	tmp.Close()
	renameFile(tmp.Name(), path)
}

func jsonMarshalPretty(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

func createTempIn(targetPath, prefix string) (*os.File, error) {
	return os.CreateTemp(filepath.Dir(targetPath), prefix+"*.tmp")
}

func renameFile(src, dst string) {
	os.Rename(src, dst)
}
