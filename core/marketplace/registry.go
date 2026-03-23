// core/marketplace/registry.go — Signed plugin marketplace registry.
//
// Manages a catalogue of available HisnOS extension modules (plugins). Each
// plugin entry carries:
//   - Name, version, description, author
//   - SHA-256 content hash of the plugin binary/archive
//   - GPG signature over the JSON manifest (detached, armoured)
//   - Minimum HisnOS version and architecture constraints
//   - Sandbox profile: "strict" | "network" | "privileged"
//
// Verification flow:
//  1. Fetch manifest from registry URL (HTTPS only)
//  2. Verify GPG signature against the trusted keyring
//     (/etc/hisnos/marketplace-keyring.gpg)
//  3. Verify SHA-256 of downloaded archive
//  4. Compat check: architecture + hisnos version
//  5. Install to /var/lib/hisnos/marketplace/<name>/<version>/
//  6. Register in local index (marketplace-index.json)
//
// Sandbox execution via bwrap (bubblewrap):
//   strict:     read-only /usr, no network, no new privs, tmpfs /tmp
//   network:    + network namespace with egress allowed
//   privileged: direct exec (operator must explicitly approve)
//
// All install/uninstall operations are logged as journald events.
// The registry never auto-installs; it only provides catalogue + verification.
package marketplace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	marketplaceBase   = "/var/lib/hisnos/marketplace"
	indexPath         = "/var/lib/hisnos/marketplace-index.json"
	keyringPath       = "/etc/hisnos/marketplace-keyring.gpg"
	defaultRegistryURL = "https://marketplace.hisnos.dev/catalogue.json"
	httpTimeout        = 30 * time.Second
	hisnOSVersion      = "1.0.0"
)

// PluginManifest is the signed catalogue entry for one plugin.
type PluginManifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Author      string `json:"author"`
	ArchiveURL  string `json:"archive_url"`
	SHA256      string `json:"sha256"`
	Signature   string `json:"signature"`   // GPG armoured detached sig over JSON sans Signature field
	MinVersion  string `json:"min_version"` // minimum hisnos version
	Arch        string `json:"arch"`        // "amd64" | "arm64" | "any"
	Sandbox     string `json:"sandbox"`     // "strict" | "network" | "privileged"
}

// InstalledPlugin is the local index record for an installed plugin.
type InstalledPlugin struct {
	Name       string    `json:"name"`
	Version    string    `json:"version"`
	InstallDir string    `json:"install_dir"`
	InstalledAt time.Time `json:"installed_at"`
	Sandbox    string    `json:"sandbox"`
	Enabled    bool      `json:"enabled"`
}

// marketplaceIndex is persisted to indexPath.
type marketplaceIndex struct {
	Installed map[string]*InstalledPlugin `json:"installed"` // keyed by name
}

// Registry manages plugin discovery, verification, and installation.
type Registry struct {
	mu          sync.Mutex
	index       marketplaceIndex
	registryURL string
	client      *http.Client

	emit func(category, event string, data map[string]any)
}

// NewRegistry creates a Registry with the given catalogue URL.
// Pass "" to use the default URL.
func NewRegistry(registryURL string, emit func(string, string, map[string]any)) *Registry {
	if registryURL == "" {
		registryURL = defaultRegistryURL
	}
	if emit == nil {
		emit = func(_, _ string, _ map[string]any) {}
	}
	r := &Registry{
		registryURL: registryURL,
		client:      &http.Client{Timeout: httpTimeout},
		emit:        emit,
		index:       marketplaceIndex{Installed: make(map[string]*InstalledPlugin)},
	}
	r.loadIndex()
	return r
}

// FetchCatalogue retrieves and returns the current plugin catalogue.
func (r *Registry) FetchCatalogue() ([]PluginManifest, error) {
	resp, err := r.client.Get(r.registryURL)
	if err != nil {
		return nil, fmt.Errorf("fetch catalogue: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalogue HTTP %d", resp.StatusCode)
	}
	var manifests []PluginManifest
	if err := json.NewDecoder(resp.Body).Decode(&manifests); err != nil {
		return nil, fmt.Errorf("decode catalogue: %w", err)
	}
	return manifests, nil
}

// Install downloads, verifies, and installs the named plugin.
// Returns an error if GPG verification or hash check fails.
func (r *Registry) Install(manifest PluginManifest) error {
	// 1. Compatibility check.
	if err := r.checkCompat(manifest); err != nil {
		return fmt.Errorf("compat: %w", err)
	}

	// 2. Download archive to a temp file.
	tmpFile, err := r.download(manifest.ArchiveURL)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer os.Remove(tmpFile)

	// 3. Verify SHA-256.
	if err := r.verifyHash(tmpFile, manifest.SHA256); err != nil {
		return fmt.Errorf("hash: %w", err)
	}

	// 4. Verify GPG signature.
	if err := r.verifyGPG(manifest); err != nil {
		return fmt.Errorf("gpg: %w", err)
	}

	// 5. Sandbox approval check.
	if manifest.Sandbox == "privileged" {
		return fmt.Errorf("privileged sandbox requires explicit operator approval via IPC")
	}

	// 6. Extract to install directory.
	installDir := filepath.Join(marketplaceBase, manifest.Name, manifest.Version)
	if err := os.MkdirAll(installDir, 0750); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := extractArchive(tmpFile, installDir); err != nil {
		_ = os.RemoveAll(installDir)
		return fmt.Errorf("extract: %w", err)
	}

	// 7. Register in index.
	r.mu.Lock()
	r.index.Installed[manifest.Name] = &InstalledPlugin{
		Name:        manifest.Name,
		Version:     manifest.Version,
		InstallDir:  installDir,
		InstalledAt: time.Now(),
		Sandbox:     manifest.Sandbox,
		Enabled:     false, // operator must explicitly enable
	}
	r.mu.Unlock()
	r.saveIndex()

	log.Printf("[marketplace] installed %s@%s sandbox=%s", manifest.Name, manifest.Version, manifest.Sandbox)
	r.emit("marketplace", "plugin_installed", map[string]any{
		"name": manifest.Name, "version": manifest.Version, "sandbox": manifest.Sandbox,
	})
	return nil
}

// Enable enables an installed plugin (allows execution).
func (r *Registry) Enable(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.index.Installed[name]
	if !ok {
		return fmt.Errorf("plugin %q not installed", name)
	}
	p.Enabled = true
	r.saveIndexLocked()
	log.Printf("[marketplace] enabled %s", name)
	r.emit("marketplace", "plugin_enabled", map[string]any{"name": name})
	return nil
}

// Disable disables a plugin without removing it.
func (r *Registry) Disable(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.index.Installed[name]
	if !ok {
		return fmt.Errorf("plugin %q not installed", name)
	}
	p.Enabled = false
	r.saveIndexLocked()
	log.Printf("[marketplace] disabled %s", name)
	r.emit("marketplace", "plugin_disabled", map[string]any{"name": name})
	return nil
}

// Uninstall removes an installed plugin.
func (r *Registry) Uninstall(name string) error {
	r.mu.Lock()
	p, ok := r.index.Installed[name]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("plugin %q not installed", name)
	}
	installDir := p.InstallDir
	delete(r.index.Installed, name)
	r.saveIndexLocked()
	r.mu.Unlock()

	if err := os.RemoveAll(installDir); err != nil {
		log.Printf("[marketplace] WARN: remove %s: %v", installDir, err)
	}
	log.Printf("[marketplace] uninstalled %s", name)
	r.emit("marketplace", "plugin_uninstalled", map[string]any{"name": name})
	return nil
}

// Run executes an installed, enabled plugin via bwrap sandbox.
func (r *Registry) Run(name string, args []string) error {
	r.mu.Lock()
	p, ok := r.index.Installed[name]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("plugin %q not installed", name)
	}
	if !p.Enabled {
		r.mu.Unlock()
		return fmt.Errorf("plugin %q is disabled", name)
	}
	sandbox := p.Sandbox
	installDir := p.InstallDir
	r.mu.Unlock()

	entrypoint := filepath.Join(installDir, "run")
	if _, err := os.Stat(entrypoint); err != nil {
		return fmt.Errorf("entrypoint %s not found", entrypoint)
	}

	cmd, err := buildBwrapCmd(entrypoint, installDir, sandbox, args)
	if err != nil {
		return err
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// InstalledList returns all installed plugins.
func (r *Registry) InstalledList() []*InstalledPlugin {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*InstalledPlugin, 0, len(r.index.Installed))
	for _, p := range r.index.Installed {
		out = append(out, p)
	}
	return out
}

// ─── internal ───────────────────────────────────────────────────────────────

func (r *Registry) checkCompat(m PluginManifest) error {
	if m.Arch != "any" && m.Arch != currentArch() {
		return fmt.Errorf("arch mismatch: plugin=%s system=%s", m.Arch, currentArch())
	}
	if m.MinVersion != "" && !versionGE(hisnOSVersion, m.MinVersion) {
		return fmt.Errorf("requires HisnOS ≥ %s (have %s)", m.MinVersion, hisnOSVersion)
	}
	return nil
}

func (r *Registry) download(url string) (string, error) {
	if !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("archive URL must use HTTPS")
	}
	resp, err := r.client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp("", "hisnos-plugin-*.tar.gz")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

func (r *Registry) verifyHash(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != strings.ToLower(expected) {
		return fmt.Errorf("SHA-256 mismatch: got=%s want=%s", got, expected)
	}
	return nil
}

// verifyGPG verifies the manifest signature using gpg --verify.
// The signature is the armoured detached sig over the canonical JSON of the
// manifest with the Signature field set to "".
func (r *Registry) verifyGPG(m PluginManifest) error {
	if _, err := os.Stat(keyringPath); err != nil {
		return fmt.Errorf("marketplace keyring not found at %s", keyringPath)
	}
	// Build canonical JSON for verification.
	sigless := m
	sigless.Signature = ""
	payload, err := json.Marshal(sigless)
	if err != nil {
		return err
	}
	// Write payload and sig to temp files.
	payloadFile, err := os.CreateTemp("", "hisnos-manifest-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(payloadFile.Name())
	payloadFile.Write(payload)
	payloadFile.Close()

	sigFile, err := os.CreateTemp("", "hisnos-sig-*.asc")
	if err != nil {
		return err
	}
	defer os.Remove(sigFile.Name())
	sigFile.WriteString(m.Signature)
	sigFile.Close()

	out, err := exec.Command("gpg", "--no-default-keyring",
		"--keyring", keyringPath,
		"--verify", sigFile.Name(), payloadFile.Name()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("GPG verification failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (r *Registry) loadIndex() {
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return
	}
	var idx marketplaceIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		log.Printf("[marketplace] WARN: corrupt index: %v", err)
		return
	}
	if idx.Installed == nil {
		idx.Installed = make(map[string]*InstalledPlugin)
	}
	r.index = idx
}

func (r *Registry) saveIndex() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saveIndexLocked()
}

func (r *Registry) saveIndexLocked() {
	data, err := json.Marshal(r.index)
	if err != nil {
		return
	}
	_ = writeMarketplaceAtomic(indexPath, data)
}

func writeMarketplaceAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".mkt-tmp-")
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

// buildBwrapCmd constructs a bubblewrap command for the given sandbox profile.
func buildBwrapCmd(entrypoint, installDir, sandbox string, args []string) (*exec.Cmd, error) {
	base := []string{
		"bwrap",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind", "/lib64", "/lib64",
		"--ro-bind", installDir, "/plugin",
		"--tmpfs", "/tmp",
		"--proc", "/proc",
		"--dev", "/dev",
		"--die-with-parent",
		"--new-session",
	}

	switch sandbox {
	case "strict":
		base = append(base, "--unshare-all")
	case "network":
		base = append(base, "--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts")
	case "privileged":
		return nil, fmt.Errorf("privileged plugins cannot be run via bwrap")
	default:
		base = append(base, "--unshare-all")
	}

	base = append(base, "/plugin/run")
	base = append(base, args...)
	cmd := exec.Command(base[0], base[1:]...)
	return cmd, nil
}

// extractArchive extracts a .tar.gz archive to destDir using the system tar.
func extractArchive(archivePath, destDir string) error {
	out, err := exec.Command("tar", "-xzf", archivePath, "-C", destDir, "--strip-components=1").CombinedOutput()
	if err != nil {
		return fmt.Errorf("tar: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func currentArch() string {
	out, err := exec.Command("uname", "-m").Output()
	if err != nil {
		return "amd64"
	}
	arch := strings.TrimSpace(string(out))
	if arch == "x86_64" {
		return "amd64"
	}
	if arch == "aarch64" {
		return "arm64"
	}
	return arch
}

// versionGE returns true if a ≥ b (simple dot-version compare, up to 3 parts).
func versionGE(a, b string) bool {
	pa := splitVersion(a)
	pb := splitVersion(b)
	for i := 0; i < 3; i++ {
		if pa[i] > pb[i] {
			return true
		}
		if pa[i] < pb[i] {
			return false
		}
	}
	return true
}

func splitVersion(v string) [3]int {
	var out [3]int
	parts := strings.SplitN(v, ".", 3)
	for i, p := range parts {
		if i >= 3 {
			break
		}
		n := 0
		for _, c := range p {
			if c >= '0' && c <= '9' {
				n = n*10 + int(c-'0')
			}
		}
		out[i] = n
	}
	return out
}
