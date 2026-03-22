// core/state/state.go — Authoritative persistent system state for hisnosd.
//
// SystemState is the single source of truth for all HisnOS subsystem state.
// Writes are mutex-protected and atomically persisted via tmp→fsync→rename.
// Reads are always a value copy (no data races via pointer aliasing).
//
// Corruption recovery: on unmarshal failure or missing version field the Manager
// falls back to default state and logs a warning. The caller (main.go) must
// still decide whether to halt or continue.

package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StateVersion is incremented when the struct layout changes incompatibly.
const StateVersion = 1

// DefaultStateFile is the canonical location for persistent state.
const DefaultStateFile = "/var/lib/hisnos/core-state.json"

// Mode represents the high-level operating mode of the workstation.
type Mode string

const (
	ModeNormal              Mode = "normal"
	ModeGaming              Mode = "gaming"
	ModeLabActive           Mode = "lab-active"
	ModeUpdatePreparing     Mode = "update-preparing"
	ModeUpdatePendingReboot Mode = "update-pending-reboot"
	ModeRollbackMode        Mode = "rollback-mode"
	ModeSafeMode            Mode = "safe-mode"
)

// RiskState holds the current threat intelligence state.
type RiskState struct {
	Score      int       `json:"score"`
	Level      string    `json:"level"`
	LastUpdate time.Time `json:"last_update"`
}

// VaultState holds the encryption vault status.
type VaultState struct {
	Mounted         bool  `json:"mounted"`
	ExposureSeconds int64 `json:"exposure_seconds"`
}

// LabState holds the isolation lab session status.
type LabState struct {
	Active         bool   `json:"active"`
	SessionID      string `json:"session_id"`
	NetworkProfile string `json:"network_profile"`
}

// FirewallState holds the nftables enforcement status.
type FirewallState struct {
	Active          bool      `json:"active"`
	EnforcedProfile string    `json:"enforced_profile"`
	LastReload      time.Time `json:"last_reload"`
}

// UpdateState holds the rpm-ostree update lifecycle status.
type UpdateState struct {
	Phase            string `json:"phase"`
	TargetDeployment string `json:"target_deployment"`
}

// SubsystemState holds the last-known alive status of each child daemon.
type SubsystemState struct {
	DashboardAlive bool `json:"dashboard_alive"`
	ThreatdAlive   bool `json:"threatd_alive"`
	LogdAlive      bool `json:"logd_alive"`
	NftablesAlive  bool `json:"nftables_alive"`
}

// SystemState is the complete authoritative state record.
type SystemState struct {
	Version    int            `json:"version"`
	Mode       Mode           `json:"mode"`
	UpdatedAt  time.Time      `json:"updated_at"`
	Risk       RiskState      `json:"risk"`
	Vault      VaultState     `json:"vault"`
	Lab        LabState       `json:"lab"`
	Firewall   FirewallState  `json:"firewall"`
	Update     UpdateState    `json:"update"`
	Subsystems SubsystemState `json:"subsystems"`
}

func defaultState() SystemState {
	return SystemState{
		Version:   StateVersion,
		Mode:      ModeNormal,
		UpdatedAt: time.Now().UTC(),
		Risk: RiskState{
			Score: 0,
			Level: "low",
		},
		Firewall: FirewallState{
			EnforcedProfile: "default",
		},
	}
}

// Manager is the authoritative in-process state holder.
// All mutations go through Update(); all reads go through Get().
type Manager struct {
	mu       sync.RWMutex
	state    SystemState
	filePath string
}

// NewManager creates a Manager, loading existing state from filePath.
// If the file is missing or corrupt, defaults are used and an error is
// returned alongside a valid Manager — the caller decides whether to abort.
func NewManager(filePath string) (*Manager, error) {
	m := &Manager{filePath: filePath}
	err := m.load()
	if err != nil {
		m.state = defaultState()
		return m, fmt.Errorf("state load: %w — starting from defaults", err)
	}
	return m, nil
}

// Get returns a deep copy of the current system state.
// Safe to call from any goroutine.
func (m *Manager) Get() SystemState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// Update applies fn to a mutable copy of the state, then persists atomically.
// fn must not call Get() or Update() (deadlock).
func (m *Manager) Update(fn func(*SystemState)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(&m.state)
	m.state.UpdatedAt = time.Now().UTC()
	if m.state.Version == 0 {
		m.state.Version = StateVersion
	}
	return m.persist()
}

// load reads and parses the state file. Called only during init (no lock needed).
func (m *Manager) load() error {
	data, err := os.ReadFile(m.filePath)
	if err != nil {
		return err
	}
	var s SystemState
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	if s.Version == 0 {
		return fmt.Errorf("version field missing — file may be corrupt")
	}
	m.state = s
	return nil
}

// persist writes state atomically: tmp file → fsync → rename.
// Called with m.mu held (write lock).
func (m *Manager) persist() error {
	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	dir := filepath.Dir(m.filePath)
	tmp, err := os.CreateTemp(dir, ".core-state-*.json")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	tmpName := tmp.Name()
	// Ensure tmp is removed on any early exit.
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmpName, m.filePath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	cleanup = false // rename succeeded; tmp is now the live file
	return nil
}
