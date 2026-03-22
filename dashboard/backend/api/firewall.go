// api/firewall.go — Firewall lifecycle state API handlers
//
// Routes:
//   GET  /api/firewall/status  — nft table check + rule count
//   POST /api/firewall/reload  — reload nftables service (confirm required)
//
// The firewall status check specifically looks for the "hisnos_egress" table,
// which is the authoritative signal that HisnOS egress rules are loaded.
// nft must be available at /usr/sbin/nft (standard Fedora path).

package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	execpkg "hisnos.local/dashboard/exec"

	cpstate "hisnos.local/dashboard/state"
)

const (
	nftBin       = "/usr/sbin/nft"
	systemctlBin = "/usr/bin/systemctl"
	// nftTableFamily and nftTableName are passed as separate arguments to nft.
	// Combining them ("inet hisnos_egress") into one arg would fail.
	nftTableFamily = "inet"
	nftTableName   = "hisnos_egress"
)

// FirewallStatusResponse is the JSON payload for GET /api/firewall/status.
type FirewallStatusResponse struct {
	NftAvailable bool   `json:"nft_available"`
	TableLoaded  bool   `json:"table_loaded"`  // hisnos_egress table is present
	RuleCount    int    `json:"rule_count"`    // terminal rules (accept/drop/reject/queue)
	Error        string `json:"error,omitempty"`
}

// FirewallStatus checks whether the hisnos_egress nftables table is loaded.
func (h *Handler) FirewallStatus(w http.ResponseWriter, r *http.Request) {
	resp := FirewallStatusResponse{}

	// First: verify nft is executable
	result, err := execpkg.Run(
		[]string{nftBin, "list", "table", nftTableFamily, nftTableName},
		execpkg.Options{Timeout: 5 * time.Second},
	)
	if err != nil {
		resp.Error = fmt.Sprintf("nft exec error: %s", err)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	resp.NftAvailable = true

	if result.ExitCode == 0 {
		resp.TableLoaded = true
		// Count terminal rules as a health signal
		for _, line := range strings.Split(result.Stdout, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "accept") ||
				strings.HasPrefix(trimmed, "drop") ||
				strings.HasPrefix(trimmed, "reject") ||
				strings.HasPrefix(trimmed, "queue") {
				resp.RuleCount++
			}
		}
	} else {
		// Table not found — check whether nft itself is functional at all
		broader, err2 := execpkg.Run(
			[]string{nftBin, "list", "ruleset"},
			execpkg.Options{Timeout: 5 * time.Second},
		)
		if err2 != nil || broader.ExitCode != 0 {
			resp.Error = "nft ruleset unavailable (nftables not running?)"
		} else {
			resp.Error = "hisnos_egress table not found — firewall policy not loaded"
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// FirewallReload reloads the nftables service. Requires confirmation header.
//
// Security note: this reloads the policy from the on-disk nftables configuration.
// If the configuration file has been tampered with, this will activate malicious rules.
// The confirmation requirement prevents accidental reload from the UI.
func (h *Handler) FirewallReload(w http.ResponseWriter, r *http.Request) {
	if !h.requireConfirm(w, r) {
		return
	}

	// Route through hisnosd when available — policy guards are enforced there.
	if h.hisnosdAvailable() {
		data, err := h.hisnosd.ReloadFirewall()
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("hisnosd: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"via":     "hisnosd",
			"data":    data,
		})
		return
	}

	// Fallback: direct exec with local state guard.
	s, err := h.stateMgr.GetSnapshot()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "control plane state unavailable")
		return
	}
	if guardErr := requireModeAllowsFirewallEnforcement(s.Mode); guardErr != nil {
		if ge, ok := guardErr.(cpstate.GuardError); ok {
			writeGuardError(w, ge)
			return
		}
		writeError(w, http.StatusInternalServerError, "state validation failed")
		return
	}

	result, err := execpkg.Run(
		[]string{systemctlBin, "reload-or-restart", "nftables"},
		execpkg.Options{Timeout: 15 * time.Second},
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("exec error: %s", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":   result.ExitCode == 0,
		"exit_code": result.ExitCode,
		"stderr":    result.Stderr,
	})
}
