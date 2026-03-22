package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Mode is the deterministic Control Plane "global posture".
// It is intentionally mutually exclusive. Orthogonal facts (vault_mounted)
// are stored separately as fields on the same state file.
type Mode string

const (
	ModeNormal            Mode = "normal"
	ModeGaming            Mode = "gaming"
	ModeLabActive         Mode = "lab-active"
	ModeVaultMounted     Mode = "vault-mounted"
	ModeUpdatePreparing  Mode = "update-preparing"
	ModeUpdatePending    Mode = "update-pending-reboot"
	ModeRollbackMode     Mode = "rollback-mode"
)

type Transition struct {
	From   Mode      `json:"from"`
	To     Mode      `json:"to"`
	Action string    `json:"action"`
	At     time.Time `json:"at"`
	Meta   any       `json:"meta,omitempty"`
}

// LabNetworkProfile is the operator-selected network containment posture for lab sessions.
// Stored in LabFacts. Applied when the next session starts; cannot change mid-session.
type LabNetworkProfile string

const (
	LabNetOffline    LabNetworkProfile = "offline"       // no network (default, kernel-enforced)
	LabNetAllowlist  LabNetworkProfile = "allowlist-cidr" // outbound to explicit CIDRs only
	LabNetDNSSinkhole LabNetworkProfile = "dns-sinkhole"  // DNS intercepted → NXDOMAIN; no outbound
	LabNetHTTPProxy  LabNetworkProfile = "http-proxy"     // outbound only via explicit HTTP proxy
)

// LabFacts records the lab network containment posture.
// NetworkProfile is set by POST /api/lab/network-profile; all other fields
// are set at session start and cleared at session stop.
type LabFacts struct {
	NetworkProfile  LabNetworkProfile `json:"network_profile"`
	AllowedCIDRs    []string          `json:"allowed_cidrs,omitempty"`
	ProxyAddr       string            `json:"proxy_addr,omitempty"`
	// VethHostIface and NftSessionSet are runtime state — populated at session start.
	VethHostIface   string            `json:"veth_host_iface,omitempty"`
	NftSessionSet   string            `json:"nft_session_set,omitempty"`
}

type KernelValidation struct {
	LastResult     string `json:"last_result,omitempty"`      // ok|fail|unknown
	LastTime       string `json:"last_time,omitempty"`        // ISO-8601
	LastDeployment string `json:"last_deployment,omitempty"`  // rpm-ostree checksum
}

type UpdateFacts struct {
	StagedDeployment   string `json:"staged_deployment,omitempty"`
	StagedPrepareTime  string `json:"staged_prepare_time,omitempty"`
	LastApplyTime       string `json:"last_apply_time,omitempty"`
	LastRollbackTime   string `json:"last_rollback_time,omitempty"`
}

// SystemState is the persisted Control Plane state exposed to the dashboard.
type SystemState struct {
	Version int      `json:"version"`
	Mode    Mode     `json:"mode"`
	VaultMounted bool `json:"vault_mounted"`

	UpdateFacts      UpdateFacts      `json:"update,omitempty"`
	KernelValidation KernelValidation `json:"kernel_validation,omitempty"`
	Lab              LabFacts         `json:"lab"`

	LastTransition Transition `json:"last_transition"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type GuardErrorCode string

const (
	ErrInvalidTransition GuardErrorCode = "E_CP_INVALID_TRANSITION"
	ErrForbiddenByState  GuardErrorCode = "E_CP_FORBIDDEN_BY_STATE"
	ErrVaultMustBeLocked GuardErrorCode = "E_CP_VAULT_MUST_BE_LOCKED"
	ErrRollbackBlocksPrepare GuardErrorCode = "E_CP_ROLLBACK_BLOCKS_PREPARE"
	ErrFirewallBlockedPrepare GuardErrorCode = "E_CP_FIREWALL_BLOCKED_DURING_PREPARE"
	ErrKernelValidationRequired GuardErrorCode = "E_CP_KERNEL_VALIDATION_REQUIRED"
	ErrFirewallCompatibilityRequired GuardErrorCode = "E_CP_FIREWALL_COMPATIBILITY_REQUIRED"
	ErrConcurrentUpdate GuardErrorCode = "E_CP_CONCURRENT_UPDATE"
)

type GuardError struct {
	Code    GuardErrorCode
	Message string
	// HTTP status is set by API handlers.
}

func (e GuardError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

type Manager struct {
	stateFile string
	mu         sync.Mutex

	cached     SystemState
	cacheValid bool
}

const defaultStateFile = "/var/lib/hisnos/control-plane-state.json"

func NewManager() *Manager {
	return &Manager{stateFile: defaultStateFile}
}

func (m *Manager) statePath() string {
	return m.stateFile
}

func (m *Manager) EnsureLoaded() (SystemState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensureLoadedLocked()
}

func (m *Manager) ensureLoadedLocked() (SystemState, error) {
	if m.cacheValid {
		return m.cached, nil
	}

	// Attempt to load from disk.
	path := m.statePath()
	raw, err := os.ReadFile(path)
	if err == nil {
		var s SystemState
		if jerr := json.Unmarshal(raw, &s); jerr == nil {
			m.cached = s
			m.cacheValid = true
			return s, nil
		}
		// Fall through to default state if JSON is invalid.
	}

	// Default initial state; callers will refine fields from facts.
	s := SystemState{
		Version:  1,
		Mode:     ModeNormal,
		VaultMounted: false,
		KernelValidation: KernelValidation{},
		UpdateFacts: UpdateFacts{},
		LastTransition: Transition{
			From:    ModeNormal,
			To:      ModeNormal,
			Action:  "init",
			At:      time.Now().UTC(),
		},
		UpdatedAt: time.Now().UTC(),
	}
	m.cached = s
	m.cacheValid = true

	// Persist default so GET /api/system/state works immediately.
	if err := m.persistLocked(s); err != nil {
		return s, err
	}
	return s, nil
}

func (m *Manager) persistLocked(s SystemState) error {
	path := m.statePath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp := fmt.Sprintf("%s.tmp.%d", path, time.Now().UnixNano())
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	// Atomic rename ensures readers never see partial writes.
	return os.Rename(tmp, path)
}

func (m *Manager) GetSnapshot() (SystemState, error) {
	return m.EnsureLoaded()
}

func (m *Manager) setModeLocked(from, to Mode, action string, meta any) (SystemState, error) {
	s := m.cached
	s.LastTransition = Transition{
		From:   from,
		To:     to,
		Action: action,
		At:     time.Now().UTC(),
		Meta:   meta,
	}
	s.Mode = to
	s.UpdatedAt = s.LastTransition.At
	m.cached = s
	return s, m.persistLocked(s)
}

func allowedTransitions(from Mode) map[Mode]bool {
	// Allowed transitions between global modes.
	// Guards that depend on facts (e.g. vaultMounted) are handled separately.
	switch from {
	case ModeNormal:
		return map[Mode]bool{
			ModeVaultMounted: true,
			ModeGaming:       true,
			ModeLabActive:    true,
			ModeUpdatePreparing: true,
			ModeUpdatePending:   true,
			ModeRollbackMode:    true,
		}
	case ModeVaultMounted:
		return map[Mode]bool{
			ModeNormal:      true,
			ModeGaming:      true,
			ModeLabActive:   true,
			ModeUpdatePreparing: true,
			ModeUpdatePending:   true,
			ModeRollbackMode:    true,
		}
	case ModeGaming:
		return map[Mode]bool{
			ModeNormal:      true,
			ModeVaultMounted: true,
			ModeLabActive:   true,
			ModeUpdatePreparing: true,
			ModeUpdatePending:   true,
			ModeRollbackMode:    true,
		}
	case ModeLabActive:
		return map[Mode]bool{
			ModeNormal:      true,
			ModeVaultMounted: true,
			ModeGaming:      true,
			ModeUpdatePreparing: true,
			ModeUpdatePending:   true,
			ModeRollbackMode:    true,
		}
	case ModeUpdatePreparing:
		return map[Mode]bool{
			ModeUpdatePending: true,
			ModeRollbackMode:   true,
			// Recovery/failure paths: if update prepare fails, return to the safest posture.
			ModeNormal:         true,
			ModeVaultMounted:   true,
			ModeGaming:         true,
			ModeLabActive:     true,
		}
	case ModeUpdatePending:
		return map[Mode]bool{
			ModeNormal:      true,
			ModeVaultMounted: true,
			ModeRollbackMode: true,
			// Recovery/failure paths after apply --defer.
			ModeGaming:       true,
			ModeLabActive:   true,
		}
	case ModeRollbackMode:
		// Critical: rollback-mode blocks update-preparing.
		// Example requirement: "rollback-mode blocks update-preparing"
		return map[Mode]bool{
			ModeNormal:      true,
			ModeVaultMounted: true,
		}
	default:
		return map[Mode]bool{}
	}
}

func (m *Manager) TransitionMode(to Mode, action string, meta any) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Ensure we have a state cached.
	s, err := m.ensureLoadedLocked()
	if err != nil {
		return GuardError{Code: ErrInvalidTransition, Message: "state load failed"}
	}

	from := s.Mode
	if from == to {
		// No-op: still persist action metadata for auditability.
		m.cached.LastTransition = Transition{
			From:   from,
			To:     to,
			Action: action,
			At:     time.Now().UTC(),
			Meta:   meta,
		}
		m.cached.UpdatedAt = m.cached.LastTransition.At
		if err := m.persistLocked(m.cached); err != nil {
			return err
		}
		return nil
	}

	allowed := allowedTransitions(from)
	if !allowed[to] {
		return GuardError{
			Code:    ErrInvalidTransition,
			Message: fmt.Sprintf("mode transition forbidden: from=%s to=%s (action=%s)", from, to, action),
		}
	}

	_, err = m.setModeLocked(from, to, action, meta)
	if err != nil {
		return err
	}

	// Observability: structured event in the journal (text handler will show key=val).
	slog.Info("hisnos.control_plane.transition",
		"from", from, "to", to, "action", action, "meta", meta)
	return nil
}

func (m *Manager) SetVaultMounted(vaultMounted bool, action string, meta any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.ensureLoadedLocked()
	if err != nil {
		return err
	}

	// Persist both the flag and any required mode change atomically.
	fromMode := s.Mode
	s.VaultMounted = vaultMounted
	m.cached = s

	// Mode changes only happen when we are in the "normal family" of modes.
	// During update/rollback, we keep the global posture.
	switch fromMode {
	case ModeNormal:
		if vaultMounted {
			// normal -> vault-mounted
			if err := m.SetModeWithGuardLocked(ModeVaultMounted, action, meta); err != nil {
				return err
			}
			slog.Info("hisnos.control_plane.transition",
				"from", fromMode, "to", ModeVaultMounted, "action", action, "meta", meta)
			return nil
		}
	case ModeVaultMounted:
		if !vaultMounted {
			// vault-mounted -> normal
			if err := m.SetModeWithGuardLocked(ModeNormal, action, meta); err != nil {
				return err
			}
			slog.Info("hisnos.control_plane.transition",
				"from", fromMode, "to", ModeNormal, "action", action, "meta", meta)
			return nil
		}
	}

	// Global posture stays unchanged; update last_transition + persist the flag.
	m.cached.LastTransition = Transition{
		From:   fromMode,
		To:     fromMode,
		Action: action,
		At:     time.Now().UTC(),
		Meta:   meta,
	}
	m.cached.UpdatedAt = m.cached.LastTransition.At
	if err := m.persistLocked(m.cached); err != nil {
		return err
	}
	slog.Info("hisnos.control_plane.vault_flag_update",
		"mode", fromMode, "vault_mounted", vaultMounted, "action", action)
	return nil
}

// SetModeWithGuardLocked performs mode transition checks while already holding mu.
// It bypasses allowedTransitions when from==to (handled by TransitionMode).
func (m *Manager) SetModeWithGuardLocked(to Mode, action string, meta any) error {
	// At this point, mu is held by caller.
	s := m.cached
	from := s.Mode
	if from == to {
		m.cached.LastTransition = Transition{
			From:   from,
			To:     to,
			Action: action,
			At:     time.Now().UTC(),
			Meta:   meta,
		}
		m.cached.UpdatedAt = m.cached.LastTransition.At
		return m.persistLocked(m.cached)
	}
	allowed := allowedTransitions(from)
	if !allowed[to] {
		return GuardError{
			Code:    ErrInvalidTransition,
			Message: fmt.Sprintf("mode transition forbidden: from=%s to=%s (action=%s)", from, to, action),
		}
	}
	_, err := m.setModeLocked(from, to, action, meta)
	return err
}

func (m *Manager) SetUpdateFacts(facts UpdateFacts, action string, meta any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.ensureLoadedLocked()
	if err != nil {
		return err
	}
	s.UpdateFacts = facts
	// Keep mode unchanged; still record transition for observability/audit.
	m.cached = s
	m.cached.LastTransition = Transition{
		From:   s.Mode,
		To:     s.Mode,
		Action: action,
		At:     time.Now().UTC(),
		Meta:   meta,
	}
	m.cached.UpdatedAt = m.cached.LastTransition.At
	if err := m.persistLocked(m.cached); err != nil {
		return err
	}
	slog.Info("hisnos.control_plane.update_facts_update",
		"mode", s.Mode, "action", action)
	return nil
}

func (m *Manager) SetKernelValidation(k KernelValidation, action string, meta any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.ensureLoadedLocked()
	if err != nil {
		return err
	}
	s.KernelValidation = k
	m.cached = s
	m.cached.LastTransition = Transition{
		From:   s.Mode,
		To:     s.Mode,
		Action: action,
		At:     time.Now().UTC(),
		Meta:   meta,
	}
	m.cached.UpdatedAt = m.cached.LastTransition.At
	if err := m.persistLocked(m.cached); err != nil {
		return err
	}
	slog.Info("hisnos.control_plane.kernel_validation_update",
		"mode", s.Mode, "action", action, "result", k.LastResult)
	return nil
}

// SetLabFacts updates the lab facts in the persisted state.
//
// Two usage patterns:
//  1. POST /api/lab/network-profile — operator changes the network profile
//     (only when no session is active; guards enforced by the caller).
//  2. POST /api/lab/start — runtime populates VethHostIface + NftSessionSet
//     after the privileged netd completes setup.
func (m *Manager) SetLabFacts(facts LabFacts, action string, meta any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.ensureLoadedLocked()
	if err != nil {
		return err
	}
	s.Lab = facts
	m.cached = s
	m.cached.LastTransition = Transition{
		From:   s.Mode,
		To:     s.Mode,
		Action: action,
		At:     time.Now().UTC(),
		Meta:   meta,
	}
	m.cached.UpdatedAt = m.cached.LastTransition.At
	if err := m.persistLocked(m.cached); err != nil {
		return err
	}
	slog.Info("hisnos.control_plane.lab_facts_update",
		"mode", s.Mode, "action", action, "network_profile", facts.NetworkProfile)
	return nil
}

// ParseLabNetworkProfile validates and returns a LabNetworkProfile from a string.
func ParseLabNetworkProfile(s string) (LabNetworkProfile, error) {
	switch LabNetworkProfile(s) {
	case LabNetOffline, LabNetAllowlist, LabNetDNSSinkhole, LabNetHTTPProxy:
		return LabNetworkProfile(s), nil
	default:
		return "", errors.New("unknown lab network profile — valid: offline, allowlist-cidr, dns-sinkhole, http-proxy")
	}
}

func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeNormal, ModeGaming, ModeLabActive, ModeVaultMounted, ModeUpdatePreparing, ModeUpdatePending, ModeRollbackMode:
		return Mode(s), nil
	default:
		return "", errors.New("unknown mode")
	}
}

