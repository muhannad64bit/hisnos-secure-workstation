// core/runtime/transaction_manager.go
//
// TransactionManager wraps state.Manager with write-ahead journaling.
//
// Every state mutation is:
//   1. Written to the journal (committed=false) before applying.
//   2. Applied atomically via state.Manager.Update (tmp→fsync→rename).
//   3. Marked committed in the journal.
//
// On startup, ReplayJournal() scans for uncommitted entries:
//   - If the on-disk state matches the intended after-checksum → mark committed.
//   - If the on-disk state matches the before-checksum → the rename failed;
//     the state is already consistent (pre-mutation), mark skipped.
//   - If neither matches → corruption detected; escalate to safe-mode.
//
// Corruption detection: SHA-256 of the current state file is compared
// against the after-checksum in the last committed journal entry.
//
// Schema migration: MigrateSchema() is called at startup to upgrade
// legacy state files. Each migration is idempotent.

package runtime

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"hisnos.local/hisnosd/state"
)

const (
	JournalFile       = "/var/lib/hisnos/state.journal"
	MaxJournalEntries = 500
)

// ── Journal ────────────────────────────────────────────────────────────────

// JournalEntry is one record in the write-ahead log.
type JournalEntry struct {
	ID        string    `json:"id"`
	Op        string    `json:"op"`
	Timestamp time.Time `json:"ts"`
	BeforeSum string    `json:"before_sum"` // SHA-256 of before-state JSON
	AfterSum  string    `json:"after_sum"`  // SHA-256 of intended after-state JSON
	Committed bool      `json:"committed"`  // true once the rename succeeded
}

// journal is a write-serialised append log backed by a JSONL file.
type journal struct {
	mu   sync.Mutex
	path string
}

func openJournal(path string) (*journal, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, err
	}
	// Touch the file to ensure it exists.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		return nil, err
	}
	f.Close()
	return &journal{path: path}, nil
}

func (j *journal) append(entry JournalEntry) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// readAll returns all journal entries, oldest first.
func (j *journal) readAll() ([]JournalEntry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	data, err := os.ReadFile(j.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var entries []JournalEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e JournalEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			log.Printf("[txn] WARN: malformed journal line skipped: %v", err)
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// rotate keeps only the last MaxJournalEntries committed entries.
func (j *journal) rotate() {
	j.mu.Lock()
	defer j.mu.Unlock()

	data, err := os.ReadFile(j.path)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) <= MaxJournalEntries {
		return
	}

	// Keep last MaxJournalEntries lines.
	keep := lines[len(lines)-MaxJournalEntries:]
	tmp, err := os.CreateTemp(filepath.Dir(j.path), ".journal-rotate-*.tmp")
	if err != nil {
		return
	}
	defer tmp.Close()

	for _, l := range keep {
		if l != "" {
			tmp.WriteString(l + "\n")
		}
	}
	tmp.Sync()
	tmp.Close()
	os.Rename(tmp.Name(), j.path)
}

// ── TransactionManager ────────────────────────────────────────────────────

// TransactionManager layers write-ahead journaling over state.Manager.
// All callers should use Apply() instead of state.Manager.Update() directly.
type TransactionManager struct {
	mu  sync.Mutex
	mgr *state.Manager
	jnl *journal
}

// NewTransactionManager creates a TransactionManager backed by the given state manager.
func NewTransactionManager(mgr *state.Manager) (*TransactionManager, error) {
	jnl, err := openJournal(JournalFile)
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	return &TransactionManager{mgr: mgr, jnl: jnl}, nil
}

// Apply runs fn against a copy of the state to compute the intended after-state,
// journals the intent, then commits via state.Manager.Update.
//
// If the journal write fails, the state mutation is NOT applied.
// If the state.Manager.Update fails, the journal entry remains uncommitted.
func (tm *TransactionManager) Apply(op string, fn func(*state.SystemState)) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// Snapshot before-state.
	before := tm.mgr.Get()
	beforeJSON, _ := json.Marshal(before)
	beforeSum := sha256hex(beforeJSON)

	// Compute after-state (apply fn to copy — no persistence yet).
	after := before
	fn(&after)
	after.UpdatedAt = time.Now().UTC()
	afterJSON, _ := json.Marshal(after)
	afterSum := sha256hex(afterJSON)

	txnID := newTxnID()
	entry := JournalEntry{
		ID:        txnID,
		Op:        op,
		Timestamp: time.Now().UTC(),
		BeforeSum: beforeSum,
		AfterSum:  afterSum,
		Committed: false,
	}

	// Write intent to journal (crash-safe point 1).
	if err := tm.jnl.append(entry); err != nil {
		return fmt.Errorf("journal intent: %w", err)
	}

	// Apply mutation atomically via state.Manager.
	if err := tm.mgr.Update(fn); err != nil {
		// Rename failed — state on disk is still the before-state.
		// Journal entry is uncommitted; ReplayJournal() will skip it.
		return fmt.Errorf("state apply: %w", err)
	}

	// Mark committed (crash-safe point 2 — best-effort).
	entry.Committed = true
	if err := tm.jnl.append(entry); err != nil {
		log.Printf("[txn] WARN: failed to mark txn %s committed: %v", txnID, err)
	}

	return nil
}

// StateChecksum returns the SHA-256 of the current state file content.
func (tm *TransactionManager) StateChecksum() (string, error) {
	data, err := os.ReadFile(state.DefaultStateFile)
	if err != nil {
		return "", err
	}
	return sha256hex(data), nil
}

// CorruptionDetected returns true if the on-disk state checksum does not match
// the last committed journal entry's after-checksum.
// A false positive is possible if the state was written outside TransactionManager;
// callers should treat this as a warning, not a hard halt.
func (tm *TransactionManager) CorruptionDetected() (bool, error) {
	entries, err := tm.jnl.readAll()
	if err != nil {
		return false, err
	}

	// Find last committed entry.
	var last *JournalEntry
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Committed {
			e := entries[i]
			last = &e
			break
		}
	}
	if last == nil {
		return false, nil // no committed entries yet
	}

	current, err := tm.StateChecksum()
	if err != nil {
		return false, err
	}

	if current != last.AfterSum {
		log.Printf("[txn] CORRUPTION DETECTED: disk=%s expected=%s (txn %s op=%s)",
			current, last.AfterSum, last.ID, last.Op)
		return true, nil
	}
	return false, nil
}

// ReplayJournal scans uncommitted entries and determines whether:
//   - The state is already consistent (mutation succeeded, commit mark missed) → mark committed.
//   - The state needs no action (before-state on disk → mutation never happened) → skip.
//   - Neither → corruption; returns error so caller can escalate to safe-mode.
func (tm *TransactionManager) ReplayJournal() (corrupted bool, err error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	entries, err := tm.jnl.readAll()
	if err != nil {
		return false, err
	}

	current, err := tm.StateChecksum()
	if err != nil {
		return false, err
	}

	for _, e := range entries {
		if e.Committed {
			continue
		}

		log.Printf("[txn] replaying uncommitted txn %s op=%s", e.ID, e.Op)

		switch {
		case current == e.AfterSum:
			// Mutation succeeded; only the commit-mark was lost. Safe.
			log.Printf("[txn] txn %s: state matches after-sum → marking committed", e.ID)
			e.Committed = true
			_ = tm.jnl.append(e)

		case current == e.BeforeSum:
			// Mutation never completed. State is consistent at before-state. Safe.
			log.Printf("[txn] txn %s: state matches before-sum → mutation rolled back cleanly", e.ID)

		default:
			// State matches neither before nor after → corruption.
			log.Printf("[txn] txn %s: CORRUPTION — disk=%s before=%s after=%s",
				e.ID, current, e.BeforeSum, e.AfterSum)
			corrupted = true
		}
	}

	// Rotate the journal to prevent unbounded growth.
	go tm.jnl.rotate()

	return corrupted, nil
}

// MigrateSchema upgrades legacy state files to the current schema version.
// Idempotent — safe to call on every startup.
func (tm *TransactionManager) MigrateSchema() error {
	s := tm.mgr.Get()
	if s.Version == state.StateVersion {
		return nil // already current
	}
	log.Printf("[txn] migrating state schema from v%d to v%d", s.Version, state.StateVersion)
	return tm.Apply("schema_migration", func(st *state.SystemState) {
		st.Version = state.StateVersion
		// v0 → v1: ensure mode is set.
		if st.Mode == "" {
			st.Mode = state.ModeNormal
		}
		// v0 → v1: ensure risk level is set.
		if st.Risk.Level == "" {
			st.Risk.Level = "low"
		}
	})
}

// ── Helpers ───────────────────────────────────────────────────────────────

func sha256hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newTxnID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "txn-" + hex.EncodeToString(b)
}
