package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	execpkg "hisnos.local/dashboard/exec"
	cpstate "hisnos.local/dashboard/state"
)

const (
	updateStatePath = "/var/lib/hisnos/update-state"
)

func writeGuardError(w http.ResponseWriter, err cpstate.GuardError) {
	status := http.StatusConflict
	switch err.Code {
	case cpstate.ErrKernelValidationRequired:
		status = http.StatusPreconditionFailed
	case cpstate.ErrConcurrentUpdate:
		status = http.StatusConflict
	}
	writeErrorCode(w, status, err.Message, string(err.Code))
}

func vaultMountedFromFacts() (bool, string) {
	xdgRuntime := os.Getenv("XDG_RUNTIME_DIR")
	if xdgRuntime == "" {
		xdgRuntime = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	lockFile := filepath.Join(xdgRuntime, "hisnos-vault.lock")
	_, err := os.Stat(lockFile)
	return err == nil, lockFile
}

func bootedChecksumFromRpmOstree() (string, error) {
	result, err := execpkg.Run(
		[]string{rpmOstreeBin, "status", "--json"},
		execpkg.Options{Timeout: 30 * time.Second},
	)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("rpm-ostree status failed (exit %d)", result.ExitCode)
	}

	var status struct {
		Deployments []struct {
			Booted bool   `json:"booted"`
			Checksum string `json:"checksum"`
		} `json:"deployments"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &status); err != nil {
		return "", err
	}
	for _, d := range status.Deployments {
		if d.Booted {
			return d.Checksum, nil
		}
	}
	return "", fmt.Errorf("booted deployment not found")
}

func kernelValidationOKForBooted() error {
	// Require last_validate_result=ok and it must match the currently booted deployment checksum.
	facts, err := cpstate.ReadFile(updateStatePath)
	if err != nil {
		return cpstate.GuardError{Code: cpstate.ErrKernelValidationRequired, Message: "cannot read update validation state"}
	}
	lastResult := facts["last_validate_result"]
	if lastResult != "ok" {
		return cpstate.GuardError{Code: cpstate.ErrKernelValidationRequired, Message: "kernel validation not passed (need last_validate_result=ok)"}
	}

	lastDeploy := facts["last_validate_deployment"]
	booted, err := bootedChecksumFromRpmOstree()
	if err != nil {
		return cpstate.GuardError{Code: cpstate.ErrKernelValidationRequired, Message: "cannot determine currently booted deployment for validation check"}
	}
	if lastDeploy == "" || lastDeploy != booted {
		return cpstate.GuardError{Code: cpstate.ErrKernelValidationRequired, Message: "kernel validation passed for a different deployment; reboot and validate before updating"}
	}

	return nil
}

func firewallCompatibilityOK() error {
	result, err := execpkg.Run(
		[]string{nftBin, "list", "table", "inet", "hisnos_egress"},
		execpkg.Options{Timeout: 10 * time.Second},
	)
	if err != nil {
		return cpstate.GuardError{Code: cpstate.ErrFirewallCompatibilityRequired, Message: "cannot query nftables firewall state"}
	}
	if result.ExitCode != 0 {
		return cpstate.GuardError{Code: cpstate.ErrFirewallCompatibilityRequired, Message: "hisnos firewall table is not loaded (run bootstrap/post-install.sh or verify nftables service)"}
	}
	return nil
}

func requireModeAllowsFirewallEnforcement(mode cpstate.Mode) error {
	// Example requirement: firewall enforce must not activate during update-preparing.
	if mode == cpstate.ModeUpdatePreparing {
		return cpstate.GuardError{Code: cpstate.ErrFirewallBlockedPrepare, Message: "firewall enforcement is forbidden while update-preparing is active"}
	}
	if mode == cpstate.ModeUpdatePending {
		return cpstate.GuardError{Code: cpstate.ErrForbiddenByState, Message: "firewall enforcement is forbidden while update-pending-reboot is active"}
	}
	if mode == cpstate.ModeRollbackMode {
		return cpstate.GuardError{Code: cpstate.ErrForbiddenByState, Message: "firewall enforcement is forbidden while rollback-mode is active"}
	}
	return nil
}

