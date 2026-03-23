// core/performance/cmdline_profile.go — Kernel command-line performance profiles.
//
// REBOOT REQUIRED: All kernel cmdline changes only take effect on next boot.
// This module stages the desired profile via rpm-ostree kargs (Kinoite) or
// /etc/default/grub (mutable systems) and reports the pending state.
//
// Profiles:
//   balanced    — default CFS, no isolation (removes perf kargs)
//   performance — RCU offload to non-boot CPUs, reduced kernel noise
//   ultra       — full CPU isolation + NOHZ_FULL (requires ≥3 CPUs)
package performance

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
)

// CmdlineProfile manages kernel boot parameter profiles.
// It does NOT apply changes to the running kernel — only stages for next reboot.
type CmdlineProfile struct{}

// cmdlineDef specifies kernel args to add/remove for a named profile.
type cmdlineDef struct {
	name        string
	description string
	add         []string // args to append (supports "N" placeholder for last core index)
	remove      []string // arg prefixes to delete
}

var cmdlineDefs = map[string]cmdlineDef{
	"balanced": {
		name:        "balanced",
		description: "Default CFS, no kernel isolation — safe for all hardware",
		remove:      []string{"isolcpus", "nohz_full", "rcu_nocbs", "rcu_nocb_poll"},
	},
	"performance": {
		name:        "performance",
		description: "RCU offload to non-boot CPUs; reduces kernel noise without isolation",
		add:         []string{"rcu_nocbs=1-N", "rcu_nocb_poll"},
		remove:      []string{"isolcpus", "nohz_full"},
	},
	"ultra": {
		name:        "ultra",
		description: "Full CPU isolation + NOHZ_FULL (requires ≥3 CPUs, dedicated game cores)",
		add:         []string{"isolcpus=2-N", "nohz_full=2-N", "rcu_nocbs=2-N", "rcu_nocb_poll"},
		remove:      []string{},
	},
}

// ActiveProfile reads /proc/cmdline and infers the current active profile.
func (c *CmdlineProfile) ActiveProfile() string {
	cmdline, err := readFile("/proc/cmdline")
	if err != nil {
		return "unknown"
	}
	switch {
	case strings.Contains(cmdline, "nohz_full") && strings.Contains(cmdline, "isolcpus"):
		return "ultra"
	case strings.Contains(cmdline, "rcu_nocbs"):
		return "performance"
	default:
		return "balanced"
	}
}

// QueueProfile stages kernel arguments for the named profile on next reboot.
// cpuCount is the number of online CPUs (used to compute the isolation range).
// Returns a human-readable message and nil on success; the message always notes reboot is required.
func (c *CmdlineProfile) QueueProfile(profile string, cpuCount int) (string, error) {
	def, ok := cmdlineDefs[profile]
	if !ok {
		return "", fmt.Errorf("unknown profile %q (valid: balanced, performance, ultra)", profile)
	}
	if cpuCount < 3 && (profile == "performance" || profile == "ultra") {
		return "", fmt.Errorf("profile %q requires ≥3 CPUs; found %d", profile, cpuCount)
	}

	lastCore := cpuCount - 1

	// Remove stale args first (best-effort; failure is non-fatal).
	for _, arg := range def.remove {
		if err := c.deleteKArg(arg); err != nil {
			log.Printf("[perf/cmdline] WARN: delete karg %q: %v", arg, err)
		}
	}

	// Append new args with CPU index substitution.
	for _, karg := range def.add {
		karg = strings.ReplaceAll(karg, "1-N", fmt.Sprintf("1-%d", lastCore))
		karg = strings.ReplaceAll(karg, "2-N", fmt.Sprintf("2-%d", lastCore))
		if err := c.appendKArg(karg); err != nil {
			return "", fmt.Errorf("append karg %q: %w", karg, err)
		}
	}

	msg := fmt.Sprintf("Profile %q staged (CPUs 0-1 reserved for system, 2-%d for workloads). Reboot required.", profile, lastCore)
	log.Printf("[perf/cmdline] %s", msg)
	return msg, nil
}

// appendKArg adds a kernel argument.
// Tries rpm-ostree first (Kinoite); falls back to /etc/default/grub.
func (c *CmdlineProfile) appendKArg(karg string) error {
	if err := c.rpmOstreeKArg("--append-if-missing=" + karg); err == nil {
		return nil
	}
	return c.grubAppend(karg)
}

// deleteKArg removes a kernel argument prefix.
func (c *CmdlineProfile) deleteKArg(prefix string) error {
	// rpm-ostree: delete by prefix (fuzzy match).
	if err := c.rpmOstreeKArg("--delete=" + prefix); err == nil {
		return nil
	}
	return c.grubDelete(prefix)
}

// rpmOstreeKArg runs rpm-ostree kargs with the given argument flag.
func (c *CmdlineProfile) rpmOstreeKArg(flag string) error {
	out, err := exec.Command("rpm-ostree", "kargs", flag).CombinedOutput()
	if err != nil {
		return fmt.Errorf("rpm-ostree kargs %s: %v: %s", flag, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// grubAppend appends karg to GRUB_CMDLINE_LINUX in /etc/default/grub.
func (c *CmdlineProfile) grubAppend(karg string) error {
	content, err := readFile("/etc/default/grub")
	if err != nil {
		return fmt.Errorf("grub not found (not a mutable system?): %w", err)
	}
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, `GRUB_CMDLINE_LINUX="`) {
			inner := strings.TrimSuffix(strings.TrimPrefix(line, `GRUB_CMDLINE_LINUX="`), `"`)
			if !strings.Contains(inner, karg) {
				lines[i] = fmt.Sprintf(`GRUB_CMDLINE_LINUX="%s %s"`, inner, karg)
			}
			break
		}
	}
	if err := writeFileAtomic("/etc/default/grub", strings.Join(lines, "\n")); err != nil {
		return err
	}
	_, err = exec.Command("grub2-mkconfig", "-o", "/boot/grub2/grub.cfg").CombinedOutput()
	return err
}

// grubDelete removes all args matching prefix from GRUB_CMDLINE_LINUX.
func (c *CmdlineProfile) grubDelete(prefix string) error {
	content, err := readFile("/etc/default/grub")
	if err != nil {
		return nil // not a GRUB system — silently skip
	}
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, `GRUB_CMDLINE_LINUX="`) {
			inner := strings.TrimSuffix(strings.TrimPrefix(line, `GRUB_CMDLINE_LINUX="`), `"`)
			var kept []string
			for _, f := range strings.Fields(inner) {
				if !strings.HasPrefix(f, prefix) {
					kept = append(kept, f)
				}
			}
			lines[i] = fmt.Sprintf(`GRUB_CMDLINE_LINUX="%s"`, strings.Join(kept, " "))
			break
		}
	}
	return writeFileAtomic("/etc/default/grub", strings.Join(lines, "\n"))
}
