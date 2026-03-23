// core/orchestrator/global_rollback.go — Atomic multi-subsystem rollback coordinator.
//
// Provides a two-phase commit style rollback across all registered subsystems.
// Each subsystem registers a pair of functions:
//   - Snapshot() (any, error)  — capture current state
//   - Restore(any) error       — restore previously captured state
//
// Rollback flow:
//  1. TakeSnapshot() — snapshots every registered subsystem in registration order
//  2. CommitPoint()  — marks the snapshot as a stable restore point
//  3. RollbackTo(id) — restores all subsystems in reverse order; on partial
//     failure, logs the error and continues (best-effort)
//
// Snapshot IDs are monotonically increasing integers. The last maxSnapshots
// snapshots are retained. Older ones are evicted.
//
// Trigger points (external callers invoke RollbackToLatest):
//   - boot_scorer rolling score < 40 (severe degradation)
//   - safe-mode escalation with correlated failures
//   - operator IPC command "global_rollback"
//   - thermal tier critical + profile downgrade request
//
// The coordinator itself does not read or write sysfs — all state knowledge
// lives in the registered subsystem closures.
package orchestrator

import (
	"fmt"
	"log"
	"sync"
	"time"
)

const maxSnapshots = 5

// SubsystemHook is a named pair of snapshot/restore functions.
type SubsystemHook struct {
	Name     string
	Snapshot func() (any, error)
	Restore  func(any) error
}

// snapshotEntry is one point-in-time capture of all subsystem states.
type snapshotEntry struct {
	ID        int
	CreatedAt time.Time
	States    map[string]any // subsystem name → captured state
}

// RollbackResult summarises the outcome of a rollback attempt.
type RollbackResult struct {
	SnapshotID int
	Succeeded  []string
	Failed     map[string]error
	Duration   time.Duration
}

// GlobalRollback coordinates atomic rollback across all registered subsystems.
type GlobalRollback struct {
	mu        sync.Mutex
	hooks     []SubsystemHook      // registration order
	snapshots []*snapshotEntry     // bounded ring
	nextID    int

	emit func(category, event string, data map[string]any)
}

// NewGlobalRollback creates the rollback coordinator.
func NewGlobalRollback(emit func(string, string, map[string]any)) *GlobalRollback {
	if emit == nil {
		emit = func(_, _ string, _ map[string]any) {}
	}
	return &GlobalRollback{emit: emit}
}

// Register adds a subsystem to the rollback coordinator.
// Registration order determines snapshot order (restore is reversed).
func (gr *GlobalRollback) Register(hook SubsystemHook) {
	gr.mu.Lock()
	defer gr.mu.Unlock()
	gr.hooks = append(gr.hooks, hook)
	log.Printf("[rollback] registered subsystem %q (total=%d)", hook.Name, len(gr.hooks))
}

// TakeSnapshot captures all subsystem states and returns a snapshot ID.
func (gr *GlobalRollback) TakeSnapshot() (int, error) {
	gr.mu.Lock()
	defer gr.mu.Unlock()

	gr.nextID++
	entry := &snapshotEntry{
		ID:        gr.nextID,
		CreatedAt: time.Now(),
		States:    make(map[string]any, len(gr.hooks)),
	}

	var errs []string
	for _, h := range gr.hooks {
		state, err := h.Snapshot()
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", h.Name, err))
			continue
		}
		entry.States[h.Name] = state
	}

	gr.snapshots = append(gr.snapshots, entry)
	// Evict old snapshots.
	if len(gr.snapshots) > maxSnapshots {
		gr.snapshots = gr.snapshots[len(gr.snapshots)-maxSnapshots:]
	}

	if len(errs) > 0 {
		log.Printf("[rollback] snapshot %d: partial errors: %v", entry.ID, errs)
		gr.emit("orchestrator", "snapshot_partial", map[string]any{
			"snapshot_id": entry.ID, "errors": errs,
		})
	} else {
		log.Printf("[rollback] snapshot %d taken (%d subsystems)", entry.ID, len(entry.States))
		gr.emit("orchestrator", "snapshot_taken", map[string]any{
			"snapshot_id": entry.ID, "subsystems": len(entry.States),
		})
	}
	return entry.ID, nil
}

// RollbackToLatest restores the most recent snapshot.
func (gr *GlobalRollback) RollbackToLatest() (*RollbackResult, error) {
	gr.mu.Lock()
	if len(gr.snapshots) == 0 {
		gr.mu.Unlock()
		return nil, fmt.Errorf("no snapshots available")
	}
	entry := gr.snapshots[len(gr.snapshots)-1]
	gr.mu.Unlock()
	return gr.restore(entry)
}

// RollbackTo restores a specific snapshot by ID.
func (gr *GlobalRollback) RollbackTo(id int) (*RollbackResult, error) {
	gr.mu.Lock()
	var entry *snapshotEntry
	for _, s := range gr.snapshots {
		if s.ID == id {
			entry = s
			break
		}
	}
	gr.mu.Unlock()
	if entry == nil {
		return nil, fmt.Errorf("snapshot %d not found", id)
	}
	return gr.restore(entry)
}

// ListSnapshots returns metadata for all retained snapshots.
func (gr *GlobalRollback) ListSnapshots() []map[string]any {
	gr.mu.Lock()
	defer gr.mu.Unlock()
	out := make([]map[string]any, 0, len(gr.snapshots))
	for _, s := range gr.snapshots {
		out = append(out, map[string]any{
			"id":         s.ID,
			"created_at": s.CreatedAt,
			"subsystems": len(s.States),
		})
	}
	return out
}

// Status returns IPC-ready coordinator state.
func (gr *GlobalRollback) Status() map[string]any {
	gr.mu.Lock()
	defer gr.mu.Unlock()
	latestID := 0
	if len(gr.snapshots) > 0 {
		latestID = gr.snapshots[len(gr.snapshots)-1].ID
	}
	subsystems := make([]string, 0, len(gr.hooks))
	for _, h := range gr.hooks {
		subsystems = append(subsystems, h.Name)
	}
	return map[string]any{
		"snapshot_count": len(gr.snapshots),
		"latest_snapshot": latestID,
		"subsystems":     subsystems,
		"max_snapshots":  maxSnapshots,
	}
}

// ─── internal ───────────────────────────────────────────────────────────────

// restore applies a snapshot entry to all subsystems in reverse order.
func (gr *GlobalRollback) restore(entry *snapshotEntry) (*RollbackResult, error) {
	start := time.Now()
	result := &RollbackResult{
		SnapshotID: entry.ID,
		Failed:     make(map[string]error),
	}

	gr.emit("orchestrator", "rollback_started", map[string]any{
		"snapshot_id": entry.ID,
		"subsystems":  len(entry.States),
	})
	log.Printf("[rollback] starting rollback to snapshot %d (%d subsystems)",
		entry.ID, len(entry.States))

	gr.mu.Lock()
	hooks := make([]SubsystemHook, len(gr.hooks))
	copy(hooks, gr.hooks)
	gr.mu.Unlock()

	// Restore in reverse registration order.
	for i := len(hooks) - 1; i >= 0; i-- {
		h := hooks[i]
		state, ok := entry.States[h.Name]
		if !ok {
			log.Printf("[rollback] skip %s (not in snapshot)", h.Name)
			continue
		}
		if err := h.Restore(state); err != nil {
			result.Failed[h.Name] = err
			log.Printf("[rollback] WARN: restore %s: %v", h.Name, err)
		} else {
			result.Succeeded = append(result.Succeeded, h.Name)
			log.Printf("[rollback] restored %s", h.Name)
		}
	}

	result.Duration = time.Since(start)

	if len(result.Failed) == 0 {
		log.Printf("[rollback] complete in %v (%d subsystems OK)", result.Duration, len(result.Succeeded))
		gr.emit("orchestrator", "rollback_complete", map[string]any{
			"snapshot_id":  entry.ID,
			"succeeded":    len(result.Succeeded),
			"duration_ms":  result.Duration.Milliseconds(),
		})
	} else {
		failNames := make([]string, 0, len(result.Failed))
		for k := range result.Failed {
			failNames = append(failNames, k)
		}
		log.Printf("[rollback] partial: %d OK, %d failed: %v",
			len(result.Succeeded), len(result.Failed), failNames)
		gr.emit("orchestrator", "rollback_partial", map[string]any{
			"snapshot_id": entry.ID,
			"succeeded":   len(result.Succeeded),
			"failed":      failNames,
			"duration_ms": result.Duration.Milliseconds(),
		})
	}
	return result, nil
}
