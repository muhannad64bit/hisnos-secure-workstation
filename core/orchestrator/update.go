// core/orchestrator/update.go — rpm-ostree update lifecycle orchestrator.

package orchestrator

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"hisnos.local/hisnosd/policy"
)

// UpdateOrchestrator wraps hisnos-update.sh for rpm-ostree lifecycle operations.
type UpdateOrchestrator struct {
	updateScript string
	preflightScript string
}

// NewUpdateOrchestrator returns an UpdateOrchestrator.
func NewUpdateOrchestrator(hisnosDir string) *UpdateOrchestrator {
	return &UpdateOrchestrator{
		updateScript:    filepath.Join(hisnosDir, "update", "hisnos-update.sh"),
		preflightScript: filepath.Join(hisnosDir, "update", "hisnos-update-preflight.sh"),
	}
}

// Execute dispatches update actions. UpdateOrchestrator currently only
// supports health checks; lifecycle actions (apply, rollback) are invoked
// directly by the IPC command handlers for SSE-stream compatibility.
func (u *UpdateOrchestrator) Execute(action policy.Action) error {
	return fmt.Errorf("update orchestrator: action %s must be invoked via IPC handler", action.Type)
}

// HealthCheck verifies rpm-ostree and update scripts are available.
func (u *UpdateOrchestrator) HealthCheck() error {
	if _, err := exec.LookPath("rpm-ostree"); err != nil {
		return fmt.Errorf("rpm-ostree not in PATH")
	}
	cmd := exec.Command("/usr/bin/test", "-x", u.updateScript)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("update script not executable: %s", u.updateScript)
	}
	return nil
}

// RunPreflight runs hisnos-update-preflight.sh and returns the exit code and output.
// exit 0 = all clear; exit 1 = warnings; exit 2+ = blocking errors.
func (u *UpdateOrchestrator) RunPreflight() (int, string, error) {
	// #nosec G204 — script path is a validated absolute path from trusted config.
	cmd := exec.Command(u.preflightScript) // #nosec G204
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), output, nil
		}
		return -1, output, fmt.Errorf("preflight exec: %w", err)
	}
	return 0, output, nil
}
