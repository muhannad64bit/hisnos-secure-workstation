// core/security/integrity/verifier.go
//
// IntegrityVerifier monitors the integrity of critical HisnOS system files.
//
// Checks (run at startup and every 5 minutes):
//   1. Systemd unit files (user + system HisnOS units) — SHA-256 vs baseline.
//   2. nftables configuration files — checksum vs baseline.
//   3. Kernel cmdline (/proc/cmdline) — required flags present.
//   4. rpm-ostree booted deployment hash — matches expected hash in baseline.
//
// Baseline management:
//   - First run (no baseline): all current hashes are recorded as good.
//     Score = 0. Emit "baseline built" event.
//   - Subsequent runs: compare against stored baseline.
//     Score += per-violation weight.
//
// Baseline file: /var/lib/hisnos/integrity-baseline.json
//
// Violations are emitted as security events (via callback) and contribute
// to the integrity sub-score which the threat engine can read.

package integrity

import (
	"crypto/sha256"
	"encoding/hex"
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
	BaselineFile    = "/var/lib/hisnos/integrity-baseline.json"
	IntegrityReport = "/var/lib/hisnos/integrity-report.json"
)

// ── Baseline types ─────────────────────────────────────────────────────────

// Baseline holds expected values for all integrity checks.
type Baseline struct {
	Version        int               `json:"version"`
	UpdatedAt      time.Time         `json:"updated_at"`
	UnitHashes     map[string]string `json:"unit_hashes"`     // path → sha256
	NFTHashes      map[string]string `json:"nft_hashes"`      // path → sha256
	NFTRulesetSum  string            `json:"nft_ruleset_sum"` // sha256 of nft list ruleset output
	KernelCmdline  []string          `json:"kernel_cmdline"`  // required flags
	OSTreeCommit   string            `json:"ostree_commit"`   // expected booted commit
}

// Violation describes a single integrity failure.
type Violation struct {
	Check    string `json:"check"`
	Path     string `json:"path,omitempty"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
	Message  string `json:"message"`
	Score    float64 `json:"score"`
}

// Report is the output of a verification run.
type Report struct {
	Timestamp  time.Time   `json:"timestamp"`
	Score      float64     `json:"score"`
	OK         bool        `json:"ok"`
	Violations []Violation `json:"violations"`
}

// ── Verifier ──────────────────────────────────────────────────────────────

// Verifier runs integrity checks on a schedule.
type Verifier struct {
	mu       sync.RWMutex
	baseline *Baseline
	onEvent  func(viol Violation)
	interval time.Duration
}

// NewVerifier creates a Verifier. onEvent is called for each detected violation.
func NewVerifier(onEvent func(Violation), interval time.Duration) *Verifier {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	v := &Verifier{onEvent: onEvent, interval: interval}
	v.loadBaseline()
	return v
}

// Run starts the periodic integrity check loop. Blocks until ctx is done.
func (v *Verifier) RunLoop(done <-chan struct{}) {
	// Initial check at startup.
	v.runOnce()

	ticker := time.NewTicker(v.interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			v.runOnce()
		}
	}
}

// runOnce performs a single full verification pass.
func (v *Verifier) runOnce() {
	report := v.Verify()
	persistReport(report)

	if !report.OK {
		log.Printf("[integrity] violations=%d score=%.1f", len(report.Violations), report.Score)
		for _, viol := range report.Violations {
			log.Printf("[integrity] VIOLATION: %s — %s", viol.Check, viol.Message)
		}
	}
}

// Verify performs all integrity checks and returns a Report.
func (v *Verifier) Verify() Report {
	v.mu.RLock()
	baseline := v.baseline
	v.mu.RUnlock()

	var violations []Violation

	// Build baseline on first call.
	if baseline == nil {
		log.Printf("[integrity] no baseline found — building now (score will be 0 this run)")
		b := v.buildBaseline()
		v.mu.Lock()
		v.baseline = &b
		v.mu.Unlock()
		saveBaseline(&b)
		return Report{
			Timestamp: time.Now(),
			Score:     0,
			OK:        true,
			Violations: []Violation{{
				Check:   "baseline",
				Message: "baseline built — integrity monitoring active",
			}},
		}
	}

	// Check 1: Systemd HisnOS unit files.
	unitViolations := v.checkUnits(baseline)
	violations = append(violations, unitViolations...)

	// Check 2: nftables configuration files.
	nftViolations := v.checkNFT(baseline)
	violations = append(violations, nftViolations...)

	// Check 3: Kernel cmdline.
	cmdlineViolations := v.checkCmdline(baseline)
	violations = append(violations, cmdlineViolations...)

	// Check 4: OSTree deployment.
	ostreeViolations := v.checkOSTree(baseline)
	violations = append(violations, ostreeViolations...)

	// Compute aggregate score.
	totalScore := 0.0
	for _, viol := range violations {
		totalScore += viol.Score
		if v.onEvent != nil {
			v.onEvent(viol)
		}
	}
	if totalScore > 100 {
		totalScore = 100
	}

	return Report{
		Timestamp:  time.Now(),
		Score:      totalScore,
		OK:         len(violations) == 0,
		Violations: violations,
	}
}

// ── Check implementations ──────────────────────────────────────────────────

func (v *Verifier) checkUnits(b *Baseline) []Violation {
	var violations []Violation

	for path, expectedHash := range b.UnitHashes {
		currentHash := hashFile(path)
		if currentHash == "" {
			violations = append(violations, Violation{
				Check:    "unit_missing",
				Path:     path,
				Expected: expectedHash,
				Message:  fmt.Sprintf("systemd unit missing: %s", path),
				Score:    20.0,
			})
		} else if currentHash != expectedHash {
			violations = append(violations, Violation{
				Check:    "unit_modified",
				Path:     path,
				Expected: expectedHash[:12],
				Actual:   currentHash[:12],
				Message:  fmt.Sprintf("systemd unit modified: %s", path),
				Score:    30.0,
			})
		}
	}
	return violations
}

func (v *Verifier) checkNFT(b *Baseline) []Violation {
	var violations []Violation

	// File-level checks.
	for path, expectedHash := range b.NFTHashes {
		currentHash := hashFile(path)
		if currentHash != "" && currentHash != expectedHash {
			violations = append(violations, Violation{
				Check:    "nft_config_modified",
				Path:     path,
				Expected: expectedHash[:12],
				Actual:   currentHash[:12],
				Message:  fmt.Sprintf("nftables config modified: %s", path),
				Score:    25.0,
			})
		}
	}

	// Live ruleset checksum.
	if b.NFTRulesetSum != "" {
		out, err := exec.Command("nft", "list", "ruleset").Output()
		if err == nil {
			currentSum := hashBytes(out)
			if currentSum != b.NFTRulesetSum {
				violations = append(violations, Violation{
					Check:    "nft_ruleset_changed",
					Expected: b.NFTRulesetSum[:12],
					Actual:   currentSum[:12],
					Message:  "live nftables ruleset does not match baseline",
					Score:    35.0,
				})
			}
		}
	}

	return violations
}

func (v *Verifier) checkCmdline(b *Baseline) []Violation {
	var violations []Violation
	data, _ := os.ReadFile("/proc/cmdline")
	cmdline := string(data)

	for _, required := range b.KernelCmdline {
		if !strings.Contains(cmdline, required) {
			violations = append(violations, Violation{
				Check:   "cmdline_missing_flag",
				Message: fmt.Sprintf("required kernel cmdline flag missing: %q", required),
				Score:   10.0,
			})
		}
	}
	return violations
}

func (v *Verifier) checkOSTree(b *Baseline) []Violation {
	if b.OSTreeCommit == "" {
		return nil
	}

	out, err := exec.Command("rpm-ostree", "status", "--json").Output()
	if err != nil {
		return nil // rpm-ostree not available or not an OSTree system
	}

	var status struct {
		Deployments []struct {
			Booted   bool   `json:"booted"`
			Checksum string `json:"checksum"`
		} `json:"deployments"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return nil
	}

	for _, d := range status.Deployments {
		if d.Booted {
			if d.Checksum != b.OSTreeCommit {
				return []Violation{{
					Check:    "ostree_commit_changed",
					Expected: b.OSTreeCommit[:12],
					Actual:   d.Checksum[:12],
					Message:  "booted OSTree commit does not match baseline",
					Score:    15.0,
				}}
			}
			return nil
		}
	}
	return nil
}

// ── Baseline builder ──────────────────────────────────────────────────────

func (v *Verifier) buildBaseline() Baseline {
	b := Baseline{
		Version:       1,
		UpdatedAt:     time.Now(),
		UnitHashes:    make(map[string]string),
		NFTHashes:     make(map[string]string),
		KernelCmdline: []string{"quiet", "splash", "loglevel=3", "rd.systemd.show_status=false"},
	}

	// HisnOS unit files.
	unitDirs := []string{
		"/usr/lib/systemd/user",
		"/usr/lib/systemd/system",
		"/etc/systemd/user",
		"/etc/systemd/system",
	}
	for _, dir := range unitDirs {
		entries, _ := filepath.Glob(filepath.Join(dir, "hisnos-*.service"))
		entries2, _ := filepath.Glob(filepath.Join(dir, "hisnos-*.socket"))
		entries = append(entries, entries2...)
		for _, path := range entries {
			if h := hashFile(path); h != "" {
				b.UnitHashes[path] = h
			}
		}
	}

	// nftables config files.
	nftPaths := []string{"/etc/nftables.conf"}
	nftGlob, _ := filepath.Glob("/etc/nftables/*.nft")
	nftPaths = append(nftPaths, nftGlob...)
	for _, path := range nftPaths {
		if h := hashFile(path); h != "" {
			b.NFTHashes[path] = h
		}
	}

	// Live ruleset.
	if out, err := exec.Command("nft", "list", "ruleset").Output(); err == nil {
		b.NFTRulesetSum = hashBytes(out)
	}

	// OSTree booted commit.
	if out, err := exec.Command("rpm-ostree", "status", "--json").Output(); err == nil {
		var status struct {
			Deployments []struct {
				Booted   bool   `json:"booted"`
				Checksum string `json:"checksum"`
			} `json:"deployments"`
		}
		if json.Unmarshal(out, &status) == nil {
			for _, d := range status.Deployments {
				if d.Booted {
					b.OSTreeCommit = d.Checksum
					break
				}
			}
		}
	}

	return b
}

// ── I/O helpers ────────────────────────────────────────────────────────────

func hashFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return hashBytes(data)
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func saveBaseline(b *Baseline) {
	data, _ := json.MarshalIndent(b, "", "  ")
	dir := filepath.Dir(BaselineFile)
	os.MkdirAll(dir, 0750)
	tmp, err := os.CreateTemp(dir, ".baseline-*.tmp")
	if err != nil {
		return
	}
	tmp.Write(data)
	tmp.Sync()
	tmp.Close()
	os.Rename(tmp.Name(), BaselineFile)
	log.Printf("[integrity] baseline written: %s (%d units, %d nft files)",
		BaselineFile, len(b.UnitHashes), len(b.NFTHashes))
}

func (v *Verifier) loadBaseline() {
	data, err := os.ReadFile(BaselineFile)
	if err != nil {
		return
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		log.Printf("[integrity] WARN: cannot parse baseline: %v", err)
		return
	}
	v.mu.Lock()
	v.baseline = &b
	v.mu.Unlock()
	log.Printf("[integrity] baseline loaded from %s (updated %s)",
		BaselineFile, b.UpdatedAt.Format(time.RFC3339))
}

func persistReport(r Report) {
	data, _ := json.MarshalIndent(r, "", "  ")
	dir := filepath.Dir(IntegrityReport)
	os.MkdirAll(dir, 0750)
	tmp, _ := os.CreateTemp(dir, ".integrity-report-*.tmp")
	if tmp == nil {
		return
	}
	tmp.Write(data)
	tmp.Sync()
	tmp.Close()
	os.Rename(tmp.Name(), IntegrityReport)
}
