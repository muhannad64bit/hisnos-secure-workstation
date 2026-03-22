// api/kernel.go — Kernel validation status API handler
//
// Routes:
//   GET /api/kernel/status — rpm-ostree deployment summary + HisnOS kernel override state
//
// Parses rpm-ostree status --json to extract:
//   - Booted deployment checksum
//   - Whether a kernel override (rpm-ostree override replace) is active
//   - Package names/versions of overridden kernel packages
//   - Whether a staged (pending) deployment exists

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	execpkg "hisnos.local/dashboard/exec"
)

const rpmOstreeBin = "/usr/bin/rpm-ostree"

// KernelStatusResponse is the JSON payload for GET /api/kernel/status.
type KernelStatusResponse struct {
	BootedChecksum   string   `json:"booted_checksum"`
	StagedChecksum   string   `json:"staged_checksum,omitempty"` // non-empty if pending reboot
	KernelOverride   bool     `json:"kernel_override"`
	OverridePackages []string `json:"override_packages,omitempty"`
	Error            string   `json:"error,omitempty"`
}

// KernelStatus calls rpm-ostree status --json and extracts kernel override information.
func (h *Handler) KernelStatus(w http.ResponseWriter, r *http.Request) {
	result, err := execpkg.Run(
		[]string{rpmOstreeBin, "status", "--json"},
		execpkg.Options{Timeout: 20 * time.Second},
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("rpm-ostree exec error: %s", err))
		return
	}
	if result.ExitCode != 0 {
		writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("rpm-ostree status failed (exit %d)", result.ExitCode))
		return
	}

	resp := parseKernelStatus(result.Stdout)
	writeJSON(w, http.StatusOK, resp)
}

// rpmOstreeJSON is a minimal subset of the rpm-ostree JSON schema.
// Only the fields needed for kernel override detection are included.
type rpmOstreeJSON struct {
	Deployments []struct {
		Booted    bool   `json:"booted"`
		Staged    bool   `json:"staged"`
		Checksum  string `json:"checksum"`
		BaseLocalReplacements []struct {
			Name string `json:"name"`
			EVR  string `json:"evr"`
		} `json:"base-local-replacements"`
	} `json:"deployments"`
}

func parseKernelStatus(raw string) KernelStatusResponse {
	resp := KernelStatusResponse{}

	var status rpmOstreeJSON
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		resp.Error = fmt.Sprintf("JSON parse error: %s", err)
		return resp
	}

	for _, d := range status.Deployments {
		if d.Staged {
			resp.StagedChecksum = d.Checksum
		}
		if d.Booted {
			resp.BootedChecksum = d.Checksum
			for _, pkg := range d.BaseLocalReplacements {
				name := strings.ToLower(pkg.Name)
				if strings.Contains(name, "kernel") || strings.Contains(name, "linux") {
					resp.KernelOverride = true
					resp.OverridePackages = append(resp.OverridePackages,
						fmt.Sprintf("%s-%s", pkg.Name, pkg.EVR))
				}
			}
		}
	}

	return resp
}
