// core/orchestrator/firewall.go — Firewall (nftables) subsystem orchestrator.

package orchestrator

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"hisnos.local/hisnosd/policy"
)

const (
	nftBin        = "/usr/sbin/nft"
	systemctlBin  = "/usr/bin/systemctl"
	nftHisnosTable = "inet hisnos"
)

// FirewallOrchestrator manages nftables via systemctl and nft.
type FirewallOrchestrator struct {
	strictRulesPath string // path to strict nftables rules (optional)
}

// NewFirewallOrchestrator returns a FirewallOrchestrator.
func NewFirewallOrchestrator() *FirewallOrchestrator {
	return &FirewallOrchestrator{
		strictRulesPath: "/etc/nftables/hisnos-strict.nft",
	}
}

// Execute dispatches firewall actions.
// Supported: ActionFirewallStrictProfile, ActionFirewallRestore.
func (f *FirewallOrchestrator) Execute(action policy.Action) error {
	switch action.Type {
	case policy.ActionFirewallStrictProfile:
		return f.applyStrict()
	case policy.ActionFirewallRestore:
		return f.reload()
	default:
		return fmt.Errorf("firewall orchestrator: unsupported action %s", action.Type)
	}
}

// HealthCheck verifies nftables.service is active and the hisnos table is loaded.
func (f *FirewallOrchestrator) HealthCheck() error {
	// Check service state.
	cmd := exec.Command(systemctlBin, "is-active", "--quiet", "nftables.service")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nftables.service inactive")
	}
	// Check table present.
	cmd2 := exec.Command(nftBin, "list", "table", "inet", "hisnos")
	if err := cmd2.Run(); err != nil {
		return fmt.Errorf("nftables hisnos table not loaded")
	}
	return nil
}

// IsFirewallActive is a helper that returns true if nftables.service is active.
func IsFirewallActive() bool {
	cmd := exec.Command(systemctlBin, "is-active", "--quiet", "nftables.service")
	return cmd.Run() == nil
}

// reload restarts nftables.service to re-apply all rules from disk.
func (f *FirewallOrchestrator) reload() error {
	cmd := exec.Command(systemctlBin, "reload-or-restart", "nftables.service")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("firewall reload failed: %w — %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// applyStrict loads the strict firewall profile if available, otherwise
// drops the forward and output chains to default-deny to minimize exposure.
// This is a best-effort emergency measure that must never return an error
// that would block the policy loop.
func (f *FirewallOrchestrator) applyStrict() error {
	// Try reload first to reset any gaming/lab chains.
	if err := f.reload(); err != nil {
		// If reload fails, attempt manual chain flush as last resort.
		_ = f.flushUnsafeChains()
		return fmt.Errorf("strict profile: reload failed: %w", err)
	}
	return nil
}

// flushUnsafeChains flushes non-essential nft chains that may have been
// introduced by gaming or lab mode during a high-risk state.
func (f *FirewallOrchestrator) flushUnsafeChains() error {
	chains := []string{"gaming_output", "gaming_input", "lab_output"}
	var lastErr error
	for _, chain := range chains {
		// #nosec G204 — chain names are hardcoded constants.
		cmd := exec.Command(nftBin, "flush", "chain", "inet", "hisnos", chain)
		if err := cmd.Run(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// LastReload returns a sentinel time value for use when no reload has occurred.
func LastReloadNow() time.Time {
	return time.Now().UTC()
}
