// firewall/fastpath.go — Phase 5: Firewall Gaming Fast Path
//
// Loads an optimized nftables chain that bypasses heavy logging for
// pre-approved gaming traffic (Steam CDN, game ports, DNS).
//
// Design:
//   - Adds table inet hisnos_gaming_fast with a hook at priority -50
//     (runs BEFORE inet hisnos_egress at priority 0)
//   - Accepted traffic exits the netfilter hook immediately
//   - Unknown traffic falls through to normal hisnos_egress chains
//   - Default-deny policy is preserved: unknown traffic is still rejected
//
// The fast-path file is /etc/nftables/hisnos-gaming-fast.nft.
// On session end: flush and delete inet hisnos_gaming_fast table.
//
// PRIVILEGE: nft table manipulation requires CAP_NET_ADMIN.
//   The user service must have AmbientCapabilities=CAP_NET_ADMIN
//   OR a polkit rule allowing the user to manipulate nftables.
//   Fails gracefully (EPERM logged, gaming continues without fast path).

package firewall

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"hisnos.local/hispowerd/config"
	"hisnos.local/hispowerd/observe"
)

const (
	nftBin         = "/usr/sbin/nft"
	fastTableName  = "hisnos_gaming_fast"
	fastTableFamily = "inet"
)

// FastPath manages the gaming firewall fast path.
type FastPath struct {
	cfg     *config.Config
	log     *observe.Logger
	mu      sync.Mutex
	applied bool
}

// NewFastPath creates a FastPath manager.
func NewFastPath(cfg *config.Config, log *observe.Logger) *FastPath {
	return &FastPath{cfg: cfg, log: log}
}

// Apply loads the gaming fast-path nft ruleset.
func (fp *FastPath) Apply() error {
	fp.mu.Lock()
	defer fp.mu.Unlock()

	if fp.applied {
		return nil
	}

	// Verify the fast path file exists.
	if _, err := os.Stat(fp.cfg.FastNFTFile); err != nil {
		return fmt.Errorf("fast path nft file not found: %s: %w", fp.cfg.FastNFTFile, err)
	}

	// Load the ruleset.
	out, err := runNFT("-f", fp.cfg.FastNFTFile)
	if err != nil {
		if isNFTPermError(out, err) {
			return fmt.Errorf("nft CAP_NET_ADMIN required (EPERM): set AmbientCapabilities=CAP_NET_ADMIN or use polkit helper")
		}
		return fmt.Errorf("nft load %s: %w — %s", fp.cfg.FastNFTFile, err, out)
	}

	fp.applied = true
	fp.log.Info("firewall: fast path loaded from %s", fp.cfg.FastNFTFile)
	return nil
}

// Restore flushes and deletes the gaming fast-path table.
// Always returns nil (errors are logged but not fatal — policy is restored on reboot).
func (fp *FastPath) Restore() error {
	fp.mu.Lock()
	defer fp.mu.Unlock()

	if !fp.applied {
		return nil
	}

	// Flush then delete the gaming table.
	if out, err := runNFT("flush", "table", fastTableFamily, fastTableName); err != nil {
		fp.log.Warn("firewall: flush gaming table: %v — %s", err, out)
		// Try delete anyway.
	}
	if out, err := runNFT("delete", "table", fastTableFamily, fastTableName); err != nil {
		fp.log.Warn("firewall: delete gaming table: %v — %s", err, out)
		// Not fatal: table may not exist or perm error.
	} else {
		fp.log.Info("firewall: fast path table removed")
	}

	fp.applied = false
	return nil
}

// IsApplied returns whether the fast path is currently active.
func (fp *FastPath) IsApplied() bool {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return fp.applied
}

// VerifyBasePolicy checks that the hisnos_egress table is still present.
// Safety check: call this after any nft operation to ensure baseline policy intact.
func (fp *FastPath) VerifyBasePolicy() error {
	out, err := runNFT("list", "table", "inet", "hisnos_egress")
	if err != nil {
		return fmt.Errorf("hisnos_egress table missing — baseline policy BROKEN: %s", out)
	}
	return nil
}

// EmergencyRestore removes the fast path table without locking (crash path).
func (fp *FastPath) EmergencyRestore() {
	_, _ = runNFT("flush", "table", fastTableFamily, fastTableName)
	_, _ = runNFT("delete", "table", fastTableFamily, fastTableName)
	fp.applied = false
	fp.log.Info("firewall: emergency fast path removal complete")
}

func runNFT(args ...string) (string, error) {
	out, err := exec.Command(nftBin, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func isNFTPermError(output string, err error) bool {
	if err == nil {
		return false
	}
	outLower := strings.ToLower(output)
	return strings.Contains(outLower, "permission denied") ||
		strings.Contains(outLower, "operation not permitted")
}
