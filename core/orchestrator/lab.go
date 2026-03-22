// core/orchestrator/lab.go — Lab isolation session orchestrator.

package orchestrator

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"hisnos.local/hisnosd/policy"
)

// LabOrchestrator manages lab session lifecycle via hisnos-lab-runtime.sh.
type LabOrchestrator struct {
	runtimeScript string // absolute path to hisnos-lab-runtime.sh
}

// NewLabOrchestrator returns a LabOrchestrator.
func NewLabOrchestrator(hisnosDir string) *LabOrchestrator {
	return &LabOrchestrator{
		runtimeScript: filepath.Join(hisnosDir, "lab", "runtime", "hisnos-lab-runtime.sh"),
	}
}

// Execute dispatches lab actions.
// ActionRejectLabStart is a policy gate — no system operation needed here;
// the IPC handler rejects the start_lab command before calling Execute.
func (l *LabOrchestrator) Execute(action policy.Action) error {
	switch action.Type {
	case policy.ActionRejectLabStart:
		// No-op: rejection is communicated back to the IPC caller by policy.CanStartLab.
		return nil
	default:
		return fmt.Errorf("lab orchestrator: unsupported action %s", action.Type)
	}
}

// HealthCheck verifies the lab runtime script is executable.
func (l *LabOrchestrator) HealthCheck() error {
	info, err := exec.LookPath(l.runtimeScript)
	_ = info
	if err != nil {
		// LookPath only works for PATH; use direct stat for absolute paths.
		cmd := exec.Command("/usr/bin/test", "-x", l.runtimeScript)
		if err2 := cmd.Run(); err2 != nil {
			return fmt.Errorf("lab runtime script not executable: %s", l.runtimeScript)
		}
	}
	return nil
}

// StopSession stops the running lab session unit.
func (l *LabOrchestrator) StopSession(sessionID string) error {
	if sessionID == "" {
		// Stop all known lab units as a fallback.
		return l.stopAll()
	}
	unitName := fmt.Sprintf("hisnos-lab-%s.service", sessionID)
	cmd := exec.Command(systemctlBin, "--user", "stop", unitName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("lab stop %s failed: %w — %s", unitName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (l *LabOrchestrator) stopAll() error {
	// Pattern-stop: stop any user service matching hisnos-lab-*.service.
	cmd := exec.Command(systemctlBin, "--user", "stop", "hisnos-lab-*.service")
	_ = cmd.Run() // best-effort
	return nil
}
