// core/orchestrator/gaming.go — Gaming performance integration orchestrator.

package orchestrator

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"hisnos.local/hisnosd/policy"
)

// GamingOrchestrator manages gaming mode via hisnos-gaming.service (user scope).
type GamingOrchestrator struct {
	gamingScript string // absolute path to hisnos-gaming.sh
}

// NewGamingOrchestrator returns a GamingOrchestrator.
func NewGamingOrchestrator(hisnosDir string) *GamingOrchestrator {
	return &GamingOrchestrator{
		gamingScript: filepath.Join(hisnosDir, "gaming", "hisnos-gaming.sh"),
	}
}

// Execute dispatches gaming actions.
// ActionRejectGamingStart is a policy gate — no system operation needed here.
func (g *GamingOrchestrator) Execute(action policy.Action) error {
	switch action.Type {
	case policy.ActionRejectGamingStart:
		// No-op: rejection is communicated to the IPC caller by policy.CanStartGaming.
		return nil
	default:
		return fmt.Errorf("gaming orchestrator: unsupported action %s", action.Type)
	}
}

// HealthCheck verifies the gaming user service unit is installed.
func (g *GamingOrchestrator) HealthCheck() error {
	cmd := exec.Command(systemctlBin, "--user", "cat", "hisnos-gaming.service")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("hisnos-gaming.service not installed or unknown")
	}
	return nil
}

// Start activates gaming mode via systemctl --user start.
func (g *GamingOrchestrator) Start() error {
	cmd := exec.Command(systemctlBin, "--user", "start", "hisnos-gaming.service")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gaming start failed: %w — %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Stop deactivates gaming mode via systemctl --user stop.
func (g *GamingOrchestrator) Stop() error {
	cmd := exec.Command(systemctlBin, "--user", "stop", "hisnos-gaming.service")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gaming stop failed: %w — %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// IsGamingActive returns true if the gaming service is running.
func IsGamingActive() bool {
	cmd := exec.Command(systemctlBin, "--user", "is-active", "--quiet", "hisnos-gaming.service")
	return cmd.Run() == nil
}
