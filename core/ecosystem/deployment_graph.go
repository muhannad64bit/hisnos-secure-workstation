// core/ecosystem/deployment_graph.go — Deployment history DAG with rollback scoring.
//
// Maintains a directed acyclic graph (DAG) of rpm-ostree deployment states.
// Each node is a DeploymentRecord describing one OSTree commit that was or is
// active on this machine. Edges point from a deployment to its predecessor.
//
// Rollback scoring (0–100) for each historical deployment:
//   +30  Age: deployment was active for ≥ 7 days (indicates stability)
//   +25  Services: all critical systemd units were running normally
//   +20  Integrity: rpm-ostree status reports no package overrides
//   +15  Boot time: boot completed in < 45 s (fast = healthy)
//   +10  No emergency events in the audit log during this deployment
//
// The graph is persisted to /var/lib/hisnos/deployment-graph.json.
// It is updated:
//   - At boot (hisnosd records current deployment hash)
//   - After a successful rpm-ostree deploy/upgrade
//   - After a rollback completes
//
// IPC commands (registered via ecosystem.Manager.IPCHandlers):
//   get_deployment_graph  → full node list with scores
//   suggest_rollback      → sorted list of candidates with scores ≥ 50
package ecosystem

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	deployGraphPath     = "/var/lib/hisnos/deployment-graph.json"
	criticalUnits       = "hisnosd.service NetworkManager.service sshd.service"
	fastBootThresholdS  = 45
	minStableAgeDays    = 7
	minRollbackScore    = 50
)

// DeploymentRecord is one node in the deployment history DAG.
type DeploymentRecord struct {
	CommitHash   string    `json:"commit_hash"`
	Version      string    `json:"version,omitempty"`
	Timestamp    time.Time `json:"timestamp"`
	BootedAt     time.Time `json:"booted_at"`
	RetiredAt    time.Time `json:"retired_at,omitempty"` // zero if currently active
	BootTimeSec  int       `json:"boot_time_sec"`
	Overrides    bool      `json:"overrides"` // rpm-ostree has package overrides
	ServicesOK   bool      `json:"services_ok"`
	AuditClean   bool      `json:"audit_clean"` // no emergency events during tenure
	PredecessorHash string `json:"predecessor_hash,omitempty"`
}

// RollbackCandidate pairs a deployment with its computed rollback score.
type RollbackCandidate struct {
	Deployment *DeploymentRecord
	Score      int
	Reasons    []string
}

// deploymentGraph is the persisted DAG.
type deploymentGraph struct {
	Nodes []*DeploymentRecord `json:"nodes"`
}

// DeploymentGraphManager tracks the deployment history and scores rollback candidates.
type DeploymentGraphManager struct {
	mu    sync.Mutex
	graph deploymentGraph
}

// NewDeploymentGraphManager loads existing graph state.
func NewDeploymentGraphManager() *DeploymentGraphManager {
	m := &DeploymentGraphManager{}
	m.load()
	return m
}

// RecordCurrent introspects the running deployment and adds/updates its node.
func (m *DeploymentGraphManager) RecordCurrent() error {
	rec, err := m.introspectCurrent()
	if err != nil {
		return fmt.Errorf("introspect: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Retire the previous active node.
	for _, n := range m.graph.Nodes {
		if n.RetiredAt.IsZero() && n.CommitHash != rec.CommitHash {
			n.RetiredAt = time.Now()
		}
	}

	// Upsert.
	found := false
	for _, n := range m.graph.Nodes {
		if n.CommitHash == rec.CommitHash {
			n.BootedAt = rec.BootedAt
			n.BootTimeSec = rec.BootTimeSec
			n.ServicesOK = rec.ServicesOK
			n.RetiredAt = time.Time{} // mark active again
			found = true
			break
		}
	}
	if !found {
		m.graph.Nodes = append(m.graph.Nodes, rec)
	}

	m.save()
	log.Printf("[deploy-graph] recorded deployment %s", rec.CommitHash[:12])
	return nil
}

// SuggestRollback returns deployments scored ≥ minRollbackScore, sorted descending.
func (m *DeploymentGraphManager) SuggestRollback() []RollbackCandidate {
	m.mu.Lock()
	defer m.mu.Unlock()

	var candidates []RollbackCandidate
	for _, n := range m.graph.Nodes {
		if n.RetiredAt.IsZero() {
			continue // skip currently active
		}
		score, reasons := m.scoreDeployment(n)
		if score >= minRollbackScore {
			candidates = append(candidates, RollbackCandidate{
				Deployment: n, Score: score, Reasons: reasons,
			})
		}
	}
	// Sort descending by score (insertion sort — small slice).
	for i := 1; i < len(candidates); i++ {
		key := candidates[i]
		j := i - 1
		for j >= 0 && candidates[j].Score < key.Score {
			candidates[j+1] = candidates[j]
			j--
		}
		candidates[j+1] = key
	}
	return candidates
}

// AllNodes returns a copy of all deployment records.
func (m *DeploymentGraphManager) AllNodes() []*DeploymentRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*DeploymentRecord, len(m.graph.Nodes))
	copy(out, m.graph.Nodes)
	return out
}

// Status returns IPC-ready deployment graph summary.
func (m *DeploymentGraphManager) Status() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()

	var activeHash string
	for _, n := range m.graph.Nodes {
		if n.RetiredAt.IsZero() {
			activeHash = n.CommitHash
			break
		}
	}
	candidates := 0
	for _, n := range m.graph.Nodes {
		if n.RetiredAt.IsZero() {
			continue
		}
		score, _ := m.scoreDeployment(n)
		if score >= minRollbackScore {
			candidates++
		}
	}
	return map[string]any{
		"total_deployments":     len(m.graph.Nodes),
		"active_commit":         activeHash,
		"rollback_candidates":   candidates,
		"min_rollback_score":    minRollbackScore,
	}
}

// ─── internal ───────────────────────────────────────────────────────────────

// scoreDeployment computes the rollback score for a historical deployment.
// Must be called with mu held.
func (m *DeploymentGraphManager) scoreDeployment(n *DeploymentRecord) (int, []string) {
	score := 0
	var reasons []string

	// Age stability (+30).
	age := time.Since(n.Timestamp)
	if !n.BootedAt.IsZero() {
		age = n.RetiredAt.Sub(n.Timestamp)
		if n.RetiredAt.IsZero() {
			age = time.Since(n.Timestamp)
		}
	}
	if age >= time.Duration(minStableAgeDays)*24*time.Hour {
		score += 30
		reasons = append(reasons, fmt.Sprintf("stable ≥%dd", minStableAgeDays))
	}

	// Services OK (+25).
	if n.ServicesOK {
		score += 25
		reasons = append(reasons, "services healthy")
	}

	// No package overrides (+20).
	if !n.Overrides {
		score += 20
		reasons = append(reasons, "no overrides")
	}

	// Fast boot (+15).
	if n.BootTimeSec > 0 && n.BootTimeSec < fastBootThresholdS {
		score += 15
		reasons = append(reasons, fmt.Sprintf("boot %ds", n.BootTimeSec))
	}

	// Audit clean (+10).
	if n.AuditClean {
		score += 10
		reasons = append(reasons, "audit clean")
	}

	return score, reasons
}

// introspectCurrent queries the running system for deployment metadata.
func (m *DeploymentGraphManager) introspectCurrent() (*DeploymentRecord, error) {
	// Get OSTree commit hash from rpm-ostree status.
	out, err := exec.Command("rpm-ostree", "status", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("rpm-ostree status: %w", err)
	}

	var status struct {
		Deployments []struct {
			Checksum  string `json:"checksum"`
			Version   string `json:"version"`
			Timestamp int64  `json:"timestamp"`
			Booted    bool   `json:"booted"`
		} `json:"deployments"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return nil, fmt.Errorf("parse status: %w", err)
	}

	var commitHash, version string
	var ts int64
	for _, d := range status.Deployments {
		if d.Booted {
			commitHash = d.Checksum
			version = d.Version
			ts = d.Timestamp
			break
		}
	}
	if commitHash == "" {
		return nil, fmt.Errorf("no booted deployment found")
	}

	rec := &DeploymentRecord{
		CommitHash:  commitHash,
		Version:     version,
		Timestamp:   time.Unix(ts, 0),
		BootedAt:    time.Now(),
		ServicesOK:  m.checkServices(),
		Overrides:   m.checkOverrides(out),
		AuditClean:  true, // set to false by audit pipeline if events found
		BootTimeSec: m.measureBootTime(),
	}
	return rec, nil
}

// checkServices verifies all critical units are active.
func (m *DeploymentGraphManager) checkServices() bool {
	for _, unit := range strings.Fields(criticalUnits) {
		out, err := exec.Command("systemctl", "is-active", unit).Output()
		if err != nil || strings.TrimSpace(string(out)) != "active" {
			return false
		}
	}
	return true
}

// checkOverrides returns true if rpm-ostree status JSON has any overrides.
func (m *DeploymentGraphManager) checkOverrides(statusJSON []byte) bool {
	return strings.Contains(string(statusJSON), "\"overrides\"")
}

// measureBootTime reads systemd boot time from systemd-analyze.
func (m *DeploymentGraphManager) measureBootTime() int {
	out, err := exec.Command("systemd-analyze", "--no-pager").Output()
	if err != nil {
		return 0
	}
	// Parse "Startup finished in ... = Xs.Yms kernel"
	// We just look for a reasonable total in seconds.
	s := string(out)
	// Find "= " then extract seconds.
	idx := strings.Index(s, "= ")
	if idx < 0 {
		return 0
	}
	rest := s[idx+2:]
	var secs int
	var ms int
	fmt.Sscanf(rest, "%dmin %dms", &secs, &ms)
	if secs == 0 {
		fmt.Sscanf(rest, "%d.%ds", &secs, &ms)
	}
	return secs
}

func (m *DeploymentGraphManager) load() {
	data, err := os.ReadFile(deployGraphPath)
	if err != nil {
		return
	}
	var g deploymentGraph
	if err := json.Unmarshal(data, &g); err != nil {
		log.Printf("[deploy-graph] WARN: corrupt graph: %v", err)
		return
	}
	m.graph = g
}

func (m *DeploymentGraphManager) save() {
	data, err := json.Marshal(m.graph)
	if err != nil {
		return
	}
	dir := filepath.Dir(deployGraphPath)
	_ = os.MkdirAll(dir, 0750)
	tmp, err := os.CreateTemp(dir, ".dg-tmp-")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	tmp.Write(data)
	tmp.Sync()
	tmp.Close()
	_ = os.Rename(tmpPath, deployGraphPath)
}
