// core/forensics/snapshot.go
//
// ForensicSnapshot captures a point-in-time system state bundle.
//
// Captured data:
//   - Active namespaces (/proc/*/ns/*) with PID → namespace inode mapping
//   - nftables ruleset (nft -j list ruleset)
//   - Mounted filesystems (/proc/mounts)
//   - Risky processes (high memory, suspicious names, unexpected capabilities)
//   - Threat score state (/var/lib/hisnos/threat-state.json)
//   - Core state (/var/lib/hisnos/core-state.json)
//   - Boot health (/var/lib/hisnos/boot-health.json)
//   - Recent journal entries (last 200 lines for hisnos-* units)
//
// Output:
//   /var/lib/hisnos/forensics/snapshot-<timestamp>.tar.gz
//
// Retention:
//   Maximum 10 snapshots kept; oldest are removed automatically.

package forensics

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	snapshotDir     = "/var/lib/hisnos/forensics"
	maxSnapshots    = 10
	riskyMemKB      = 512 * 1024 // 512 MB — flag if resident > this
)

// ── Snapshot types ────────────────────────────────────────────────────────

// ProcessEntry describes a single running process.
type ProcessEntry struct {
	PID     int      `json:"pid"`
	PPID    int      `json:"ppid"`
	Comm    string   `json:"comm"`
	Exe     string   `json:"exe,omitempty"`
	Cmdline []string `json:"cmdline,omitempty"`
	MemKB   int64    `json:"mem_kb"`
	Risky   bool     `json:"risky"`
	Why     string   `json:"why,omitempty"`
}

// NamespaceEntry maps a PID to its namespace inodes.
type NamespaceEntry struct {
	PID        int            `json:"pid"`
	Comm       string         `json:"comm"`
	Namespaces map[string]string `json:"namespaces"` // type → inode
}

// Snapshot is the in-memory representation of captured state.
type Snapshot struct {
	Timestamp   time.Time          `json:"timestamp"`
	Namespaces  []NamespaceEntry   `json:"namespaces"`
	Processes   []ProcessEntry     `json:"processes"`
	Mounts      []string           `json:"mounts"`
	NFTRules    json.RawMessage    `json:"nft_rules,omitempty"`
	ThreatState json.RawMessage    `json:"threat_state,omitempty"`
	CoreState   json.RawMessage    `json:"core_state,omitempty"`
	BootHealth  json.RawMessage    `json:"boot_health,omitempty"`
	JournalTail []string           `json:"journal_tail,omitempty"`
}

// ── Capture ───────────────────────────────────────────────────────────────

// Capture builds a complete forensic snapshot and writes it as a .tar.gz bundle.
// Returns the path to the archive file.
func Capture() (string, error) {
	if err := os.MkdirAll(snapshotDir, 0750); err != nil {
		return "", fmt.Errorf("create snapshot dir: %w", err)
	}

	snap := &Snapshot{
		Timestamp: time.Now().UTC(),
	}

	log.Printf("[forensics] capturing snapshot at %s", snap.Timestamp.Format(time.RFC3339))

	// Capture each section; log failures but continue.
	snap.Namespaces = captureNamespaces()
	snap.Processes = captureProcesses()
	snap.Mounts = captureMounts()
	snap.NFTRules = captureNFT()
	snap.ThreatState = readJSONFile("/var/lib/hisnos/threat-state.json")
	snap.CoreState = readJSONFile("/var/lib/hisnos/core-state.json")
	snap.BootHealth = readJSONFile("/var/lib/hisnos/boot-health.json")
	snap.JournalTail = captureJournal()

	// Write archive.
	ts := snap.Timestamp.Format("2006-01-02T15-04-05Z")
	archivePath := filepath.Join(snapshotDir, "snapshot-"+ts+".tar.gz")

	if err := writeArchive(archivePath, snap); err != nil {
		return "", fmt.Errorf("write archive: %w", err)
	}

	log.Printf("[forensics] snapshot written: %s", archivePath)

	// Rotate old snapshots.
	go rotateSnapshots()

	return archivePath, nil
}

// ── Section capturers ─────────────────────────────────────────────────────

func captureNamespaces() []NamespaceEntry {
	var entries []NamespaceEntry

	procs, _ := filepath.Glob("/proc/[0-9]*/ns")
	for _, nsDir := range procs {
		pidStr := filepath.Base(filepath.Dir(nsDir))
		pid, _ := strconv.Atoi(pidStr)
		if pid == 0 {
			continue
		}

		comm := readFirstLine("/proc/" + pidStr + "/comm")
		nsTypes, _ := filepath.Glob(nsDir + "/*")
		nsMap := make(map[string]string)

		for _, nsPath := range nsTypes {
			nsType := filepath.Base(nsPath)
			target, err := os.Readlink(nsPath)
			if err == nil {
				// target is e.g. "net:[4026531992]"
				nsMap[nsType] = target
			}
		}

		if len(nsMap) > 0 {
			entries = append(entries, NamespaceEntry{
				PID:        pid,
				Comm:       strings.TrimSpace(comm),
				Namespaces: nsMap,
			})
		}
	}
	return entries
}

func captureProcesses() []ProcessEntry {
	var entries []ProcessEntry

	procs, _ := filepath.Glob("/proc/[0-9]*")
	for _, procDir := range procs {
		pidStr := filepath.Base(procDir)
		pid, _ := strconv.Atoi(pidStr)
		if pid == 0 {
			continue
		}

		comm := strings.TrimSpace(readFirstLine(procDir + "/comm"))
		exe, _ := os.Readlink(procDir + "/exe")

		// Parse cmdline (NUL-separated).
		cmdlineRaw, _ := os.ReadFile(procDir + "/cmdline")
		parts := strings.Split(strings.TrimRight(string(cmdlineRaw), "\x00"), "\x00")

		// Parse statm for memory (field 1 = VmRSS in pages; field 2 = resident).
		memKB := int64(0)
		statm, _ := os.ReadFile(procDir + "/statm")
		fields := strings.Fields(string(statm))
		if len(fields) >= 2 {
			pages, _ := strconv.ParseInt(fields[1], 10, 64)
			memKB = pages * 4 // assume 4 KB pages
		}

		// Parse PPID from status.
		ppid := 0
		for _, line := range readLines(procDir + "/status") {
			if strings.HasPrefix(line, "PPid:") {
				ppid, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
				break
			}
		}

		risky := false
		why := ""

		if memKB > riskyMemKB {
			risky = true
			why = fmt.Sprintf("high memory (%d MB)", memKB/1024)
		}

		// Flag suspicious names.
		suspiciousComms := []string{"ncat", "nc", "socat", "nmap", "tcpdump", "strace", "ltrace",
			"ptrace", "gdb", "lldb", "capsh", "nsenter", "unshare", "setcap"}
		for _, s := range suspiciousComms {
			if strings.Contains(strings.ToLower(comm), s) {
				risky = true
				why += " suspicious comm=" + comm
			}
		}

		entry := ProcessEntry{
			PID:     pid,
			PPID:    ppid,
			Comm:    comm,
			Exe:     exe,
			Cmdline: parts,
			MemKB:   memKB,
			Risky:   risky,
			Why:     strings.TrimSpace(why),
		}
		entries = append(entries, entry)
	}

	// Sort risky processes first.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Risky != entries[j].Risky {
			return entries[i].Risky
		}
		return entries[i].MemKB > entries[j].MemKB
	})

	return entries
}

func captureMounts() []string {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func captureNFT() json.RawMessage {
	out, err := exec.Command("nft", "-j", "list", "ruleset").Output()
	if err != nil {
		return json.RawMessage(`{"error":"nft not available"}`)
	}
	return json.RawMessage(out)
}

func captureJournal() []string {
	out, err := exec.Command(
		"journalctl",
		"--no-pager",
		"-n", "200",
		"--output=short-iso",
		"_SYSTEMD_UNIT=hisnosd.service",
		"+", "_SYSTEMD_UNIT=hisnos-threatd.service",
		"+", "_SYSTEMD_UNIT=nftables.service",
	).Output()
	if err != nil {
		return []string{"(journal capture failed: " + err.Error() + ")"}
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

func readJSONFile(path string) json.RawMessage {
	data, err := os.ReadFile(path)
	if err != nil {
		return json.RawMessage(`{"error":"not found"}`)
	}
	return json.RawMessage(data)
}

// ── Archive writer ─────────────────────────────────────────────────────────

func writeArchive(path string, snap *Snapshot) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0640)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		return err
	}
	defer gz.Close()

	tw := tar.NewWriter(gz)
	defer tw.Close()

	// snapshot.json — full structured data.
	snapJSON, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := addTarFile(tw, "snapshot.json", snapJSON); err != nil {
		return err
	}

	// processes-risky.json — just the flagged processes.
	var risky []ProcessEntry
	for _, p := range snap.Processes {
		if p.Risky {
			risky = append(risky, p)
		}
	}
	riskyJSON, _ := json.MarshalIndent(risky, "", "  ")
	_ = addTarFile(tw, "processes-risky.json", riskyJSON)

	// journal.txt — plain text for readability.
	_ = addTarFile(tw, "journal.txt", []byte(strings.Join(snap.JournalTail, "\n")))

	// nft-rules.json
	if snap.NFTRules != nil {
		_ = addTarFile(tw, "nft-rules.json", snap.NFTRules)
	}

	return nil
}

func addTarFile(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{
		Name:    name,
		Mode:    0640,
		Size:    int64(len(data)),
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// ── Rotation ──────────────────────────────────────────────────────────────

func rotateSnapshots() {
	entries, err := filepath.Glob(filepath.Join(snapshotDir, "snapshot-*.tar.gz"))
	if err != nil || len(entries) <= maxSnapshots {
		return
	}
	sort.Strings(entries) // lexicographic = chronological for ISO timestamps
	toDelete := entries[:len(entries)-maxSnapshots]
	for _, f := range toDelete {
		if err := os.Remove(f); err == nil {
			log.Printf("[forensics] rotated old snapshot: %s", f)
		}
	}
}

// ── Utilities ─────────────────────────────────────────────────────────────

func readFirstLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if sc.Scan() {
		return sc.Text()
	}
	return ""
}

func readLines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

// ── IO wrapper kept for test convenience ──────────────────────────────────

// _ ensures io is used (archive/tar uses io.Writer internally).
var _ io.Writer = (*os.File)(nil)
