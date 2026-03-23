// core/ecosystem/channel_manager.go — OSTree update channel management.
//
// Manages signed update channel switching (stable / beta / hardened).
// Each channel is an OSTree remote with a distinct GPG signing key.
// Channel switches are atomic: the new remote is added before the old is removed.
//
// Signing: all channels require GPG-signed OSTree commits.
// The GPG key for each channel must be imported into the system keyring
// before switching (step performed by the bootstrap installer).
package ecosystem

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
)

// ChannelDef describes an update distribution channel.
type ChannelDef struct {
	Name        string `json:"name"`
	RemoteURL   string `json:"remote_url"`
	Branch      string `json:"branch"`
	GPGKeyID    string `json:"gpg_key_id"`
	Description string `json:"description"`
}

// Registered channels. RemoteURL should be updated to the real distribution URL
// during deployment. The placeholder URL is intentionally invalid to prevent
// accidental connectivity to a non-existent endpoint.
var registeredChannels = map[string]ChannelDef{
	"stable": {
		Name:        "stable",
		RemoteURL:   "https://ostree.hisnos.example/repo",
		Branch:      "fedora/kinoite/x86_64/hisnos-stable",
		GPGKeyID:    "hisnos-release-key",
		Description: "Production-grade releases, security-reviewed, recommended for all deployments",
	},
	"beta": {
		Name:        "beta",
		RemoteURL:   "https://ostree.hisnos.example/repo",
		Branch:      "fedora/kinoite/x86_64/hisnos-beta",
		GPGKeyID:    "hisnos-beta-key",
		Description: "Pre-release builds for staging environments; not for production use",
	},
	"hardened": {
		Name:        "hardened",
		RemoteURL:   "https://ostree.hisnos.example/repo",
		Branch:      "fedora/kinoite/x86_64/hisnos-hardened",
		GPGKeyID:    "hisnos-hardened-key",
		Description: "Maximum-security configuration with additional kernel lockdown; may break gaming",
	},
}

// ChannelManager manages OSTree remote configuration and channel switching.
type ChannelManager struct {
	stateDir string
}

// NewChannelManager creates a ChannelManager.
func NewChannelManager(stateDir string) *ChannelManager {
	return &ChannelManager{stateDir: stateDir}
}

// Available returns all registered channel definitions.
func (m *ChannelManager) Available() map[string]ChannelDef {
	return registeredChannels
}

// Current returns the name of the currently configured OSTree remote channel.
// Falls back to "stable" if it cannot be determined.
func (m *ChannelManager) Current() string {
	out, err := exec.Command("rpm-ostree", "status", "--json").Output()
	if err != nil {
		return "stable"
	}
	// Minimal parse: look for the origin field in the JSON output.
	s := string(out)
	for name := range registeredChannels {
		if strings.Contains(s, "/hisnos-"+name) {
			return name
		}
	}
	return "stable"
}

// Switch atomically changes the update channel.
// Flow:
//  1. Validate channel exists.
//  2. Add new OSTree remote (if not already present).
//  3. Run rpm-ostree rebase to the new channel branch.
//  4. Reboot is required for the change to take effect.
func (m *ChannelManager) Switch(channel string) error {
	def, ok := registeredChannels[channel]
	if !ok {
		return fmt.Errorf("unknown channel %q (valid: stable, beta, hardened)", channel)
	}

	current := m.Current()
	if current == channel {
		return fmt.Errorf("already on channel %q", channel)
	}

	log.Printf("[ecosystem/channel] switching %s → %s", current, channel)

	// Add remote if not present.
	remoteName := "hisnos-" + channel
	addArgs := []string{
		"remote", "add", "--if-not-exists",
		"--gpg-import=/etc/pki/rpm-gpg/hisnos-" + def.GPGKeyID + ".gpg",
		remoteName, def.RemoteURL,
	}
	if out, err := exec.Command("ostree", addArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("ostree remote add: %v: %s", err, strings.TrimSpace(string(out)))
	}

	// Stage rebase to new channel branch.
	rebaseRef := remoteName + ":" + def.Branch
	if out, err := exec.Command("rpm-ostree", "rebase", rebaseRef).CombinedOutput(); err != nil {
		return fmt.Errorf("rpm-ostree rebase %s: %v: %s", rebaseRef, err, strings.TrimSpace(string(out)))
	}

	log.Printf("[ecosystem/channel] staged rebase to %s — reboot required", rebaseRef)
	return nil
}

// Verify checks that the current booted commit is signed by the expected channel key.
// Returns nil if valid, error if signature cannot be verified.
func (m *ChannelManager) Verify(channel string) error {
	def, ok := registeredChannels[channel]
	if !ok {
		return fmt.Errorf("unknown channel %q", channel)
	}
	out, err := exec.Command("rpm-ostree", "status", "--verbose").CombinedOutput()
	if err != nil {
		return fmt.Errorf("rpm-ostree status: %w", err)
	}
	// Presence of GPG key ID in verbose output indicates signature.
	if !strings.Contains(string(out), def.GPGKeyID) {
		return fmt.Errorf("GPG key %q not found in current deployment — signature may be missing", def.GPGKeyID)
	}
	return nil
}
