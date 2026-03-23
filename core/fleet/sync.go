// core/fleet/sync.go — Fleet identity and remote policy synchronisation.
//
// Fleet identity:
//   The fleet ID is a privacy-preserving derivative of the machine-id:
//   FleetID = hex(SHA-256("hisnos-fleet-v1:" + machine-id))[:16]
//   This is stable across reboots but cannot be reversed to the machine-id.
//   The machine-id itself is never transmitted to any remote system.
//
// Policy bundles:
//   Operators can host a signed policy bundle at a configurable HTTPS URL.
//   The bundle is a JSON array of PolicyRule objects, signed with the same
//   GPG keyring used by the marketplace (/etc/hisnos/marketplace-keyring.gpg).
//
//   PolicyRule fields:
//     rule_id      — unique identifier
//     description  — human-readable description
//     target       — "firewall" | "cgroup" | "sysctl" | "profile"
//     action       — target-specific action string
//     params       — key-value pairs passed to the action executor
//     priority     — higher wins on conflict
//
//   Bundle format:
//     { "version": 1, "fleet_id": "...", "rules": [...], "signature": "..." }
//
// Sync behaviour:
//   - Pull-only (fleet nodes never accept push connections)
//   - Air-gap tolerant: if the remote is unreachable, the last-known-good
//     bundle remains active; a "stale_policy" warning is emitted after
//     staleness exceeds policyStaleThreshold
//   - Sync interval: configurable (default 15 minutes)
//   - The applied bundle is cached to /var/lib/hisnos/fleet-policy.json
//   - Each rule application is emitted as a journald event
//
// Remote policy URL is read from /etc/hisnos/fleet.conf (key=value format):
//   policy_url = https://fleet.example.com/hisnos-policy.json
//   sync_interval_min = 15
package fleet

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	fleetConfPath       = "/etc/hisnos/fleet.conf"
	fleetPolicyPath     = "/var/lib/hisnos/fleet-policy.json"
	fleetIdentityPath   = "/var/lib/hisnos/fleet-identity.json"
	fleetKeyringPath    = "/etc/hisnos/marketplace-keyring.gpg"
	machineIDPath       = "/etc/machine-id"
	defaultSyncInterval = 15 * time.Minute
	policyStaleThreshold = 2 * time.Hour
	httpTimeout         = 20 * time.Second
)

// PolicyRule describes one remotely-pushed configuration directive.
type PolicyRule struct {
	RuleID      string         `json:"rule_id"`
	Description string         `json:"description"`
	Target      string         `json:"target"` // firewall|cgroup|sysctl|profile
	Action      string         `json:"action"`
	Params      map[string]any `json:"params,omitempty"`
	Priority    int            `json:"priority"`
}

// PolicyBundle is the signed remote policy document.
type PolicyBundle struct {
	Version   int          `json:"version"`
	FleetID   string       `json:"fleet_id"`
	IssuedAt  time.Time    `json:"issued_at"`
	Rules     []PolicyRule `json:"rules"`
	Signature string       `json:"signature"`
}

// fleetIdentity is the cached device identity document.
type fleetIdentity struct {
	FleetID   string    `json:"fleet_id"`
	CreatedAt time.Time `json:"created_at"`
}

// fleetConfig holds parsed /etc/hisnos/fleet.conf values.
type fleetConfig struct {
	PolicyURL    string
	SyncInterval time.Duration
}

// FleetSync manages fleet identity and policy synchronisation.
type FleetSync struct {
	mu           sync.Mutex
	fleetID      string
	cfg          fleetConfig
	lastSync     time.Time
	lastBundle   *PolicyBundle
	client       *http.Client
	applyRule    func(rule PolicyRule) error // injected rule executor
	emit         func(category, event string, data map[string]any)
}

// NewFleetSync initialises fleet identity and loads configuration.
func NewFleetSync(
	applyRule func(PolicyRule) error,
	emit func(string, string, map[string]any),
) *FleetSync {
	if emit == nil {
		emit = func(_, _ string, _ map[string]any) {}
	}
	if applyRule == nil {
		applyRule = func(r PolicyRule) error {
			log.Printf("[fleet] rule %s: no executor configured", r.RuleID)
			return nil
		}
	}
	fs := &FleetSync{
		client:    &http.Client{Timeout: httpTimeout},
		applyRule: applyRule,
		emit:      emit,
	}
	fs.fleetID = fs.deriveFleetID()
	fs.cfg = fs.loadConfig()
	fs.loadCachedBundle()
	log.Printf("[fleet] identity fleet_id=%s policy_url=%s", fs.fleetID, fs.cfg.PolicyURL)
	return fs
}

// FleetID returns the privacy-preserving fleet identifier.
func (fs *FleetSync) FleetID() string {
	return fs.fleetID
}

// Sync performs one policy sync cycle. Safe to call on a timer.
func (fs *FleetSync) Sync() error {
	if fs.cfg.PolicyURL == "" {
		return nil // fleet sync not configured
	}

	bundle, err := fs.fetchBundle()
	if err != nil {
		age := time.Since(fs.lastSync)
		if age > policyStaleThreshold {
			fs.emit("fleet", "stale_policy_warning", map[string]any{
				"stale_minutes": int(age.Minutes()),
				"url":           fs.cfg.PolicyURL,
			})
			log.Printf("[fleet] WARN: policy stale for %v: %v", age.Round(time.Minute), err)
		}
		return err
	}

	if err := fs.verifyBundle(bundle); err != nil {
		return fmt.Errorf("bundle verification: %w", err)
	}

	if bundle.FleetID != "" && bundle.FleetID != fs.fleetID {
		return fmt.Errorf("bundle fleet_id mismatch: got=%s want=%s", bundle.FleetID, fs.fleetID)
	}

	if err := fs.applyBundle(bundle); err != nil {
		return fmt.Errorf("apply bundle: %w", err)
	}

	fs.mu.Lock()
	fs.lastSync = time.Now()
	fs.lastBundle = bundle
	fs.mu.Unlock()

	fs.saveBundle(bundle)
	log.Printf("[fleet] sync complete: %d rules applied", len(bundle.Rules))
	return nil
}

// Status returns IPC-ready fleet state.
func (fs *FleetSync) Status() map[string]any {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	ruleCount := 0
	if fs.lastBundle != nil {
		ruleCount = len(fs.lastBundle.Rules)
	}
	stale := false
	if !fs.lastSync.IsZero() && time.Since(fs.lastSync) > policyStaleThreshold {
		stale = true
	}
	return map[string]any{
		"fleet_id":         fs.fleetID,
		"policy_url":       fs.cfg.PolicyURL,
		"last_sync":        fs.lastSync,
		"active_rules":     ruleCount,
		"policy_stale":     stale,
		"sync_interval_min": int(fs.cfg.SyncInterval.Minutes()),
	}
}

// SyncInterval returns the configured sync period.
func (fs *FleetSync) SyncInterval() time.Duration {
	return fs.cfg.SyncInterval
}

// ─── internal ───────────────────────────────────────────────────────────────

func (fs *FleetSync) deriveFleetID() string {
	// Try to load cached identity first.
	if data, err := os.ReadFile(fleetIdentityPath); err == nil {
		var id fleetIdentity
		if json.Unmarshal(data, &id) == nil && id.FleetID != "" {
			return id.FleetID
		}
	}

	// Derive from machine-id.
	machineIDRaw, err := os.ReadFile(machineIDPath)
	if err != nil {
		log.Printf("[fleet] WARN: cannot read machine-id: %v", err)
		machineIDRaw = []byte("unknown")
	}
	machineID := strings.TrimSpace(string(machineIDRaw))
	h := sha256.Sum256([]byte("hisnos-fleet-v1:" + machineID))
	fleetID := hex.EncodeToString(h[:])[:16]

	// Cache to disk.
	id := fleetIdentity{FleetID: fleetID, CreatedAt: time.Now()}
	if data, err := json.Marshal(id); err == nil {
		_ = writeFleetAtomic(fleetIdentityPath, data)
	}
	return fleetID
}

func (fs *FleetSync) loadConfig() fleetConfig {
	cfg := fleetConfig{SyncInterval: defaultSyncInterval}
	data, err := os.ReadFile(fleetConfPath)
	if err != nil {
		return cfg
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		k := strings.TrimSpace(parts[0])
		v := strings.TrimSpace(parts[1])
		switch k {
		case "policy_url":
			cfg.PolicyURL = v
		case "sync_interval_min":
			n := 0
			for _, c := range v {
				if c >= '0' && c <= '9' {
					n = n*10 + int(c-'0')
				}
			}
			if n > 0 {
				cfg.SyncInterval = time.Duration(n) * time.Minute
			}
		}
	}
	return cfg
}

func (fs *FleetSync) fetchBundle() (*PolicyBundle, error) {
	if !strings.HasPrefix(fs.cfg.PolicyURL, "https://") {
		return nil, fmt.Errorf("policy_url must use HTTPS")
	}
	resp, err := fs.client.Get(fs.cfg.PolicyURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var b PolicyBundle
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &b, nil
}

func (fs *FleetSync) verifyBundle(b *PolicyBundle) error {
	if _, err := os.Stat(fleetKeyringPath); err != nil {
		// No keyring → skip verification (air-gap / development mode).
		log.Printf("[fleet] WARN: no keyring at %s, skipping GPG verify", fleetKeyringPath)
		return nil
	}
	// Build canonical payload (signature field zeroed).
	sigless := *b
	sigless.Signature = ""
	payload, err := json.Marshal(sigless)
	if err != nil {
		return err
	}
	payloadFile, err := os.CreateTemp("", "hisnos-fleet-bundle-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(payloadFile.Name())
	payloadFile.Write(payload)
	payloadFile.Close()

	sigFile, err := os.CreateTemp("", "hisnos-fleet-sig-*.asc")
	if err != nil {
		return err
	}
	defer os.Remove(sigFile.Name())
	sigFile.WriteString(b.Signature)
	sigFile.Close()

	out, err := exec.Command("gpg", "--no-default-keyring",
		"--keyring", fleetKeyringPath,
		"--verify", sigFile.Name(), payloadFile.Name()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("GPG: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (fs *FleetSync) applyBundle(b *PolicyBundle) error {
	for _, rule := range b.Rules {
		if err := fs.applyRule(rule); err != nil {
			log.Printf("[fleet] WARN: apply rule %s: %v", rule.RuleID, err)
			fs.emit("fleet", "rule_apply_failed", map[string]any{
				"rule_id": rule.RuleID, "error": err.Error(),
			})
			continue
		}
		fs.emit("fleet", "rule_applied", map[string]any{
			"rule_id": rule.RuleID, "target": rule.Target, "action": rule.Action,
		})
	}
	return nil
}

func (fs *FleetSync) loadCachedBundle() {
	data, err := os.ReadFile(fleetPolicyPath)
	if err != nil {
		return
	}
	var b PolicyBundle
	if err := json.Unmarshal(data, &b); err != nil {
		return
	}
	fs.lastBundle = &b
	log.Printf("[fleet] loaded cached bundle: %d rules", len(b.Rules))
}

func (fs *FleetSync) saveBundle(b *PolicyBundle) {
	data, err := json.Marshal(b)
	if err != nil {
		return
	}
	_ = writeFleetAtomic(fleetPolicyPath, data)
}

func writeFleetAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".fleet-tmp-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	return os.Rename(tmpPath, path)
}
