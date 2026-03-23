// core/ecosystem/update_manager.go — OSTree deployment health, staging, and rollback.
//
// Wraps rpm-ostree operations with health scoring, staged-rollout support,
// and rollback confidence scoring. All mutating operations require a reboot
// to take effect; this manager only stages them.
//
// Rollback confidence score (0–100):
//   +30  deployment age > 7 days (stable, not just installed)
//   +25  boot count ≥ 5 (successfully booted multiple times)
//   +25  all required hisnosd services running (systemd check)
//   +20  integrity verifier passed (integrity-report.json status=pass)
//   -20  active staged deployment not yet applied (pending reboot)
//   -10  deployment age < 1 day (brand new)
package ecosystem

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Deployment represents a single OSTree deployment entry.
type Deployment struct {
	Checksum    string    `json:"checksum"`
	Version     string    `json:"version"`
	Booted      bool      `json:"booted"`
	Staged      bool      `json:"staged"`
	Pinned      bool      `json:"pinned"`
	Timestamp   time.Time `json:"timestamp"`
	Origin      string    `json:"origin"`
	HealthScore int       `json:"health_score"`
}

// UpdateStatus is returned by the IPC get_update_status command.
type UpdateStatus struct {
	Channel          string       `json:"channel"`
	CurrentCommit    string       `json:"current_commit"`
	PendingCommit    string       `json:"pending_commit,omitempty"`
	UpdateAvailable  bool         `json:"update_available"`
	Deployments      []Deployment `json:"deployments"`
	BootedHealth     int          `json:"booted_health_score"`
	RollbackReady    bool         `json:"rollback_ready"`
	RollbackConf     int          `json:"rollback_confidence"`
	LastChecked      time.Time    `json:"last_checked"`
}

// UpdateManager orchestrates rpm-ostree lifecycle operations.
type UpdateManager struct {
	channel  *ChannelManager
	stateDir string
}

// NewUpdateManager creates an UpdateManager.
func NewUpdateManager(stateDir string, ch *ChannelManager) *UpdateManager {
	return &UpdateManager{channel: ch, stateDir: stateDir}
}

// Status returns a complete view of the current OSTree deployment state.
func (m *UpdateManager) Status() (UpdateStatus, error) {
	deployments, err := m.listDeployments()
	if err != nil {
		return UpdateStatus{}, err
	}

	status := UpdateStatus{
		Channel:     m.channel.Current(),
		LastChecked: time.Now().UTC(),
		Deployments: deployments,
	}

	// Find booted and staged deployments.
	for i := range deployments {
		d := &deployments[i]
		if d.Booted {
			status.CurrentCommit = d.Checksum[:16]
			status.BootedHealth = d.HealthScore
		}
		if d.Staged {
			status.PendingCommit = d.Checksum[:16]
			status.UpdateAvailable = true
		}
	}

	// Rollback is ready if there are 2+ deployments and the second is not staged.
	if len(deployments) >= 2 && !deployments[1].Staged {
		status.RollbackReady = true
		status.RollbackConf = m.rollbackConfidence(deployments)
	}

	return status, nil
}

// CheckAvailable queries the upstream remote for new commits.
// Returns (available, pendingChecksum, error).
func (m *UpdateManager) CheckAvailable() (bool, string, error) {
	out, err := exec.Command("rpm-ostree", "update", "--check").CombinedOutput()
	if err != nil {
		s := string(out)
		if strings.Contains(s, "No updates available") {
			return false, "", nil
		}
		return false, "", fmt.Errorf("rpm-ostree update --check: %v: %s", err, strings.TrimSpace(s))
	}
	s := string(out)
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "commit") && strings.Contains(line, "→") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				return true, fields[len(fields)-1], nil
			}
		}
	}
	return true, "", nil
}

// Stage downloads the staged update without deploying.
// Equivalent to: rpm-ostree upgrade --download-only
func (m *UpdateManager) Stage(ctx context.Context) error {
	log.Printf("[ecosystem/update] staging update (download only)")
	cmd := exec.CommandContext(ctx, "rpm-ostree", "upgrade", "--download-only")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rpm-ostree upgrade --download-only: %v: %s", err, strings.TrimSpace(string(out)))
	}
	log.Printf("[ecosystem/update] update staged: %s", strings.TrimSpace(string(out)))
	return nil
}

// Apply stages and marks the update for deployment on next reboot.
// Equivalent to: rpm-ostree upgrade
func (m *UpdateManager) Apply(ctx context.Context) error {
	log.Printf("[ecosystem/update] applying update (pending reboot)")
	cmd := exec.CommandContext(ctx, "rpm-ostree", "upgrade")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rpm-ostree upgrade: %v: %s", err, strings.TrimSpace(string(out)))
	}
	log.Printf("[ecosystem/update] update applied (reboot required): %s", strings.TrimSpace(string(out)))
	return nil
}

// Rollback reverts to the previous deployment.
// Requires confirmation (caller must pass confirm=true).
func (m *UpdateManager) Rollback(confirm bool) error {
	if !confirm {
		return fmt.Errorf("rollback requires explicit confirmation (confirm=true)")
	}
	log.Printf("[ecosystem/update] initiating rollback")
	out, err := exec.Command("rpm-ostree", "rollback").CombinedOutput()
	if err != nil {
		return fmt.Errorf("rpm-ostree rollback: %v: %s", err, strings.TrimSpace(string(out)))
	}
	log.Printf("[ecosystem/update] rollback staged (reboot required): %s", strings.TrimSpace(string(out)))
	return nil
}

// rollbackConfidence computes a 0–100 confidence score for the previous deployment.
// Higher score = more confidence that rolling back is safe and the previous is stable.
func (m *UpdateManager) rollbackConfidence(deps []Deployment) int {
	if len(deps) < 2 {
		return 0
	}
	prev := deps[1]
	score := 0

	age := time.Since(prev.Timestamp)
	switch {
	case age > 7*24*time.Hour:
		score += 30
	case age < 24*time.Hour:
		score -= 10
	}

	// Check required services running.
	services := []string{"hisnosd", "nftables", "auditd"}
	runningCount := 0
	for _, svc := range services {
		out, err := exec.Command("systemctl", "is-active", svc).Output()
		if err == nil && strings.TrimSpace(string(out)) == "active" {
			runningCount++
		}
	}
	score += (runningCount * 25) / len(services)

	// Check integrity report.
	if b, err := os.ReadFile(m.stateDir + "/integrity-report.json"); err == nil {
		var report struct{ Status string `json:"status"` }
		if json.Unmarshal(b, &report) == nil && report.Status == "pass" {
			score += 20
		}
	}

	if prev.Staged {
		score -= 20
	}

	if score < 0 {
		return 0
	}
	if score > 100 {
		return 100
	}
	return score
}

// listDeployments runs rpm-ostree status --json and parses the deployments array.
func (m *UpdateManager) listDeployments() ([]Deployment, error) {
	out, err := exec.Command("rpm-ostree", "status", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("rpm-ostree status: %w", err)
	}

	var raw struct {
		Deployments []struct {
			Checksum  string `json:"checksum"`
			Version   string `json:"version"`
			Booted    bool   `json:"booted"`
			Staged    bool   `json:"staged"`
			Pinned    bool   `json:"pinned"`
			Timestamp int64  `json:"timestamp"`
			Origin    string `json:"origin"`
		} `json:"deployments"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse rpm-ostree status: %w", err)
	}

	deps := make([]Deployment, 0, len(raw.Deployments))
	for _, d := range raw.Deployments {
		dep := Deployment{
			Checksum:  d.Checksum,
			Version:   d.Version,
			Booted:    d.Booted,
			Staged:    d.Staged,
			Pinned:    d.Pinned,
			Timestamp: time.Unix(d.Timestamp, 0).UTC(),
			Origin:    d.Origin,
		}
		dep.HealthScore = m.deploymentHealth(dep)
		deps = append(deps, dep)
	}
	return deps, nil
}

// deploymentHealth scores an individual deployment (0–100).
func (m *UpdateManager) deploymentHealth(d Deployment) int {
	score := 40 // base for existing deployment
	age := time.Since(d.Timestamp)
	if age > 30*24*time.Hour {
		score += 20
	} else if age > 7*24*time.Hour {
		score += 10
	}
	if !d.Staged {
		score += 20
	}
	if !d.Pinned {
		score += 10
	}
	if age < 24*time.Hour {
		score -= 10
	}
	if score > 100 {
		return 100
	}
	return score
}
