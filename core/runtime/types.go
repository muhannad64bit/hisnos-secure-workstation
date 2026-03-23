// core/runtime/types.go
//
// Shared canonical types used across all HisnOS core runtime packages.
// This file is the single source of truth for Mode, RiskLevel, and SchemaVersion.

package runtime

import "time"

// ── Mode ─────────────────────────────────────────────────────────────────────

// Mode is the operating mode of the HisnOS control plane.
type Mode string

const (
	ModeNormal   Mode = "normal"
	ModeGaming   Mode = "gaming"
	ModeSafeMode Mode = "safe-mode"
	ModeRecovery Mode = "recovery"
	// Transient modes used during multi-step operations.
	ModeUpdatePreparing Mode = "update-preparing"
	ModeRollback        Mode = "rollback"
)

func (m Mode) IsMutatingAllowed() bool {
	return m == ModeNormal || m == ModeGaming
}

func (m Mode) IsSafe() bool {
	return m == ModeSafeMode || m == ModeRecovery
}

// ── RiskLevel ────────────────────────────────────────────────────────────────

// RiskLevel is the aggregate threat assessment band.
type RiskLevel string

const (
	RiskMinimal  RiskLevel = "minimal"  // score 0–19
	RiskLow      RiskLevel = "low"      // score 20–39
	RiskMedium   RiskLevel = "medium"   // score 40–59
	RiskHigh     RiskLevel = "high"     // score 60–79
	RiskCritical RiskLevel = "critical" // score 80–100
)

func ScoreToLevel(score float64) RiskLevel {
	switch {
	case score >= 80:
		return RiskCritical
	case score >= 60:
		return RiskHigh
	case score >= 40:
		return RiskMedium
	case score >= 20:
		return RiskLow
	default:
		return RiskMinimal
	}
}

// ── SchemaVersion ─────────────────────────────────────────────────────────────

// SchemaVersion tracks the JSON schema of state files.
// Increment this when the CoreState struct changes incompatibly.
type SchemaVersion int

const CurrentSchemaVersion SchemaVersion = 1

// ── CoreState ─────────────────────────────────────────────────────────────────

// CoreState is the canonical on-disk state for the control plane.
// All mutations go through TransactionManager.Apply().
type CoreState struct {
	SchemaVersion SchemaVersion `json:"schema_version"`
	Mode          Mode          `json:"mode"`
	RiskLevel     RiskLevel     `json:"risk_level"`
	RiskScore     float64       `json:"risk_score"`

	// Safe-mode state.
	SafeModeActive       bool      `json:"safe_mode_active"`
	SafeModeEnteredAt    time.Time `json:"safe_mode_entered_at,omitempty"`
	SafeModeAcknowledged bool      `json:"safe_mode_acknowledged"`

	// Emergency flags.
	LastEmergencyBoot bool `json:"last_emergency_boot"`
	ContainmentActive bool `json:"containment_active"`

	// Subsystem health timestamps.
	LastGoodBoot    time.Time `json:"last_good_boot"`
	UpdatedAt       time.Time `json:"updated_at"`
	StateChecksum   string    `json:"state_checksum,omitempty"`
}

// ── SubsystemHealth ───────────────────────────────────────────────────────────

// SubsystemStatus is the runtime health status of a watched subsystem.
type SubsystemStatus struct {
	Name          string
	Alive         bool
	LastHeartbeat time.Time
	Restarts      int
	CircuitOpen   bool // true = circuit breaker tripped
}
