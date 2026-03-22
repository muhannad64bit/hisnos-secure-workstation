// core/orchestrator/threat.go — Threat intelligence daemon orchestrator.

package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"hisnos.local/hisnosd/policy"
)

const defaultThreatStateFile = "/var/lib/hisnos/threat-state.json"

// ThreatOrchestrator manages hisnos-threatd and reads its state file.
type ThreatOrchestrator struct {
	stateFile string
}

// NewThreatOrchestrator returns a ThreatOrchestrator.
func NewThreatOrchestrator() *ThreatOrchestrator {
	stateFile := os.Getenv("HISNOS_THREAT_STATE")
	if stateFile == "" {
		stateFile = defaultThreatStateFile
	}
	return &ThreatOrchestrator{stateFile: stateFile}
}

// Execute handles ActionRestartSubsystem for hisnos-threatd.
func (t *ThreatOrchestrator) Execute(action policy.Action) error {
	if action.Type != policy.ActionRestartSubsystem {
		return fmt.Errorf("threat orchestrator: unsupported action %s", action.Type)
	}
	unit, _ := action.Payload["unit"].(string)
	if unit == "" {
		unit = "hisnos-threatd.service"
	}
	scope, _ := action.Payload["scope"].(string)

	args := []string{"--user", "restart", unit}
	if scope == "system" {
		args = []string{"restart", unit}
	}
	cmd := exec.Command(systemctlBin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("restart %s failed: %w — %s", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// HealthCheck verifies hisnos-threatd.service is active.
func (t *ThreatOrchestrator) HealthCheck() error {
	cmd := exec.Command(systemctlBin, "--user", "is-active", "--quiet", "hisnos-threatd.service")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("hisnos-threatd.service inactive")
	}
	return nil
}

// ThreatSnapshot holds the key fields read from threat-state.json.
type ThreatSnapshot struct {
	RiskScore int    `json:"risk_score"`
	RiskLevel string `json:"risk_level"`
	UpdatedAt string `json:"updated_at"`
	Signals   map[string]bool `json:"signals"`
}

// ReadThreatState reads and parses threat-state.json.
// Returns a zeroed snapshot (not an error) if the file is absent —
// threatd is optional and its absence must not impair hisnosd.
func (t *ThreatOrchestrator) ReadThreatState() ThreatSnapshot {
	data, err := os.ReadFile(t.stateFile)
	if err != nil {
		return ThreatSnapshot{RiskLevel: "low"}
	}
	var snap ThreatSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return ThreatSnapshot{RiskLevel: "low"}
	}
	if snap.RiskLevel == "" {
		snap.RiskLevel = riskLevelFromScore(snap.RiskScore)
	}
	return snap
}

func riskLevelFromScore(score int) string {
	switch {
	case score >= 81:
		return "critical"
	case score >= 51:
		return "high"
	case score >= 21:
		return "medium"
	default:
		return "low"
	}
}
