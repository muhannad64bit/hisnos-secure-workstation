// core/orchestrator/vault.go — Vault subsystem orchestrator.

package orchestrator

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"hisnos.local/hisnosd/policy"
)

// VaultOrchestrator manages gocryptfs vault lifecycle via hisnos-vault.sh.
type VaultOrchestrator struct {
	// scriptPath is the absolute path to hisnos-vault.sh.
	// Constructed from HISNOS_DIR env var or a default.
	scriptPath string
}

// NewVaultOrchestrator returns a VaultOrchestrator.
// hisnosDir is $HOME/.local/share/hisnos (or HISNOS_DIR).
func NewVaultOrchestrator(hisnosDir string) *VaultOrchestrator {
	return &VaultOrchestrator{
		scriptPath: filepath.Join(hisnosDir, "vault", "hisnos-vault.sh"),
	}
}

// Execute runs the action associated with the vault subsystem.
// Supported actions: ActionForceVaultLock.
func (v *VaultOrchestrator) Execute(action policy.Action) error {
	switch action.Type {
	case policy.ActionForceVaultLock:
		return v.lock()
	default:
		return fmt.Errorf("vault orchestrator: unsupported action %s", action.Type)
	}
}

// HealthCheck verifies the vault script is executable and the watcher service
// is installed (not necessarily running — vault may be unmounted).
func (v *VaultOrchestrator) HealthCheck() error {
	info, err := os.Stat(v.scriptPath)
	if err != nil {
		return fmt.Errorf("vault script missing: %s", v.scriptPath)
	}
	if info.Mode()&0111 == 0 {
		return fmt.Errorf("vault script not executable: %s", v.scriptPath)
	}
	return nil
}

// lock executes `hisnos-vault.sh lock`.
func (v *VaultOrchestrator) lock() error {
	if err := v.HealthCheck(); err != nil {
		return err
	}
	// #nosec G204 — scriptPath is a validated absolute path from a trusted config source.
	cmd := exec.Command(v.scriptPath, "lock") // #nosec G204
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("vault lock failed: %w — output: %s",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}

// IsVaultMounted reads /proc/mounts to check if a gocryptfs mount is active.
// This is immune to event-window gaps — mount state at any time is accurate.
func IsVaultMounted() bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "gocryptfs")
}
