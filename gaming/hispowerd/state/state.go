// state/state.go — hispowerd gaming state persistence
//
// Two JSON files managed here:
//   /var/lib/hisnos/gaming-state.json   — hispowerd-owned gaming session state
//   /var/lib/hisnos/core-state.json     — shared control plane state (mode field only)
//
// All writes are atomic: tmp → fsync → rename.
// Reads are best-effort: returns zero-value on file missing or corrupt.

package state

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// GamingState is persisted to gaming-state.json.
type GamingState struct {
	GamingActive        bool   `json:"gaming_active"`
	GamePID             int    `json:"game_pid,omitempty"`
	GameName            string `json:"game_name,omitempty"`
	StartTimestamp      string `json:"start_timestamp,omitempty"`
	SessionType         string `json:"session_type,omitempty"` // steam|proton|wine|manual

	// Applied subsystem flags — dashboard reads these.
	CPUIsolationApplied bool   `json:"cpu_isolation_applied"`
	IRQTuned            bool   `json:"irq_tuned"`
	FirewallFastPath    bool   `json:"firewall_fastpath"`
	GovernorSet         string `json:"governor_set,omitempty"`
	DaemonsThrottled    bool   `json:"daemons_throttled"`

	// Updated every scan cycle when active.
	UpdatedAt string `json:"updated_at"`
}

// controlPlaneState is the minimal read/write surface of core-state.json.
// hispowerd only touches the "mode" field; hisnosd owns the rest.
type controlPlaneState struct {
	Mode      string `json:"mode"`
	UpdatedAt string `json:"updated_at"`
	// other hisnosd fields preserved via RawMessage
}

// Manager owns state persistence for hispowerd.
type Manager struct {
	mu              sync.Mutex
	gamingPath      string
	cpStatePath     string
	hisnosdSocket   string
	current         GamingState
}

// NewManager creates a state Manager.
func NewManager(gamingPath, cpStatePath, hisnosdSocket string) *Manager {
	return &Manager{
		gamingPath:    gamingPath,
		cpStatePath:   cpStatePath,
		hisnosdSocket: hisnosdSocket,
	}
}

// Update applies fn to the current state and persists it atomically.
func (m *Manager) Update(fn func(*GamingState)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(&m.current)
	m.current.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return m.persist(m.gamingPath, m.current)
}

// Get returns a copy of the current in-memory state.
func (m *Manager) Get() GamingState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// Load reads gaming-state.json into memory (best-effort at startup).
func (m *Manager) Load() {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := os.ReadFile(m.gamingPath)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &m.current)
}

// SetControlPlaneMode transitions the control plane mode.
// First tries hisnosd IPC; falls back to direct JSON write.
func (m *Manager) SetControlPlaneMode(mode string) error {
	// Try hisnosd IPC first.
	if err := m.hisnosdSetMode(mode); err == nil {
		return nil
	}
	// Fallback: direct atomic write to core-state.json.
	return m.writeControlPlaneMode(mode)
}

// hisnosdSetMode sends a set_mode command to hisnosd IPC socket.
func (m *Manager) hisnosdSetMode(mode string) error {
	if m.hisnosdSocket == "" {
		return fmt.Errorf("no hisnosd socket configured")
	}
	if _, err := os.Stat(m.hisnosdSocket); err != nil {
		return fmt.Errorf("hisnosd socket not present: %w", err)
	}
	conn, err := net.DialTimeout("unix", m.hisnosdSocket, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	req := map[string]any{"id": "hispowerd-1", "command": "set_mode", "params": map[string]string{"mode": mode}}
	enc := json.NewEncoder(conn)
	if err := enc.Encode(req); err != nil {
		return err
	}
	var resp map[string]any
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&resp); err != nil {
		return err
	}
	if ok, _ := resp["ok"].(bool); !ok {
		errMsg, _ := resp["error"].(string)
		return fmt.Errorf("hisnosd rejected: %s", errMsg)
	}
	return nil
}

// writeControlPlaneMode performs a best-effort direct write to core-state.json.
// Preserves existing content except the mode and updated_at fields.
func (m *Manager) writeControlPlaneMode(mode string) error {
	// Read existing state to preserve all fields.
	var raw map[string]any
	if data, err := os.ReadFile(m.cpStatePath); err == nil {
		_ = json.Unmarshal(data, &raw)
	}
	if raw == nil {
		raw = make(map[string]any)
	}
	raw["mode"] = mode
	raw["updated_at"] = time.Now().UTC().Format(time.RFC3339)
	return m.persist(m.cpStatePath, raw)
}

// persist atomically writes v as JSON to path.
func (m *Manager) persist(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".hispowerd-state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op if rename succeeded
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	tmp.Close()
	return os.Rename(tmpName, path)
}

// ClearGamingState resets gaming-state.json to inactive.
func (m *Manager) ClearGamingState() error {
	return m.Update(func(s *GamingState) {
		*s = GamingState{} // zero value = gaming_active: false
	})
}
