# HisnOS Build Pipeline Reference

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                     HisnOS Build Pipeline (v1.0)                           │
│                                                                             │
│  ┌─────────────┐     ┌──────────────┐     ┌──────────────┐                │
│  │  Fedora 43  │────▶│ rpm-ostree   │────▶│  OSTree Repo │                │
│  │  Repos      │     │  compose     │     │  (archive)   │                │
│  └─────────────┘     └──────────────┘     └──────┬───────┘                │
│                                                   │                         │
│  ┌─────────────┐     ┌──────────────┐            │                         │
│  │  treefile   │────▶│ postprocess  │            │                         │
│  │  .json      │     │  .sh (hook)  │            │                         │
│  └─────────────┘     └──────────────┘            │                         │
│                                                   ▼                         │
│  ┌─────────────┐     ┌──────────────┐     ┌──────────────┐                │
│  │  Lorax      │────▶│ livemedia-   │────▶│ Live Root    │                │
│  │  templates  │     │ creator      │     │ (squashfs)   │                │
│  └─────────────┘     └──────────────┘     └──────┬───────┘                │
│                                                   │                         │
│  ┌─────────────┐     ┌──────────────┐            │                         │
│  │  Calamares  │────▶│  Stage HisnOS│◀───────────┘                         │
│  │  kickstart  │     │  components  │                                       │
│  └─────────────┘     └──────┬───────┘                                       │
│                             │                                               │
│  ┌─────────────┐            ▼                                               │
│  │  Plymouth   │     ┌──────────────┐     ┌──────────────┐                │
│  │  theme      │────▶│   xorriso    │────▶│  HisnOS ISO  │                │
│  └─────────────┘     │  (hybrid)    │     │  (.iso)      │                │
│                       └──────────────┘     └──────┬───────┘                │
│  ┌─────────────┐                                  │                         │
│  │  Recovery   │                                  ▼                         │
│  │  GRUB entry │────────────────────▶  SHA256 + Manifest                   │
│  └─────────────┘                                  │                         │
│                                                   ▼                         │
│                                          ┌──────────────┐                  │
│                                          │  QEMU test   │                  │
│                                          │  + sign      │                  │
│                                          └──────────────┘                  │
└─────────────────────────────────────────────────────────────────────────────┘
```

## Runtime Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    HisnOS Runtime Architecture                              │
│                                                                             │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │                    systemd (PID 1)                                    │  │
│  │  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐           │  │
│  │  │ hisnosd  │  │ threatd  │  │hisnos-   │  │hispowerd │           │  │
│  │  │ (core)   │  │          │  │automation│  │(gaming)  │           │  │
│  │  └────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬─────┘           │  │
│  │       │              │              │              │                  │  │
│  │  ┌────▼──────────────▼──────────────▼──────────────▼─────────────┐  │  │
│  │  │              hisnosd IPC Bus (Unix socket JSON-RPC)            │  │  │
│  │  └────┬─────────────────────────────────────────────┬────────────┘  │  │
│  │       │                                             │               │  │
│  │  ┌────▼─────┐  ┌──────────┐  ┌──────────┐  ┌─────▼──────┐       │  │
│  │  │dashboard │  │ vault    │  │ egress   │  │performance  │       │  │
│  │  │:7374     │  │(gocrypt) │  │(nftables)│  │(cpu,irq,   │       │  │
│  │  └──────────┘  └──────────┘  └──────────┘  │thermal,    │       │  │
│  │                                              │numa,rt)    │       │  │
│  │  ┌──────────┐  ┌──────────┐  ┌──────────┐  └────────────┘       │  │
│  │  │ auditd   │  │ KVM/QEMU │  │ distrobox│                        │  │
│  │  │ hisnos-  │  │ (lab)    │  │ (lab)    │                        │  │
│  │  │ logd     │  └──────────┘  └──────────┘                        │  │
│  │  └──────────┘                                                     │  │
│  └──────────────────────────────────────────────────────────────────┘  │
│                                                                             │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │                    OSTree / rpm-ostree base                           │  │
│  │  Immutable /usr  │  Mutable /etc /var  │  Overlay packages           │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────────┘
```

## Boot Sequence

```
Power ON
    │
    ▼
UEFI/BIOS firmware
    │ POST + hardware init
    ▼
GRUB 2 (EFI: /boot/efi/EFI/fedora/grubx64.efi)
    │ Load kernel + initramfs
    ▼
Linux kernel
    │ Hardware detection, device nodes
    ▼
dracut initramfs
    │
    ├─ pre-udev/05: hisnos-cmdline-check.sh
    │    • Validate kernel args against policy
    │    • Set /run/hisnos/safemode if violations
    │    • Set /run/hisnos/recovery if requested
    │
    ├─ pre-pivot/40: hisnos-boot.sh
    │    • Read boot health score from installed system
    │    • If score < 40 → force safe-mode
    │    • Filesystem integrity check (ext4 fsck -n)
    │    • Write boot summary to /run/hisnos/
    │
    ├─ pre-pivot/50: hisnos-recovery-menu.sh
    │    (only if hisnos.recovery=1)
    │    • Interactive recovery menu
    │    • Rescue shell / chroot / journal / firewall reset
    │
    └─ pivot_root → /sysroot
         │
         ▼
systemd (PID 1)
    │
    ├─ local-fs.target
    │    └─ hisnos-safe-mode.service (if /run/hisnos/safemode-active)
    │
    ├─ sysinit.target
    │    ├─ auditd.service
    │    └─ nftables.service (load hisnos_filter + hisnos_egress)
    │
    ├─ network.target
    │    └─ NetworkManager.service
    │
    ├─ multi-user.target
    │    ├─ hisnosd.service           (core control runtime)
    │    ├─ hisnos-threat-engine      (threat detection)
    │    ├─ hisnos-automation         (AI decision engine)
    │    ├─ hisnos-performance-guard  (RT + IRQ + thermal)
    │    ├─ hisnos-dashboard.service  (governance UI :7374)
    │    ├─ hisnos-boot-complete      (record boot health score)
    │    └─ hisnos-update-check.timer (weekly update check)
    │
    └─ graphical.target
         └─ sddm.service → KDE Plasma 6
```

## Safe-Mode Escalation

```
Trigger conditions → Safe-mode activation:

 (A) Boot health score < 40 (3+ consecutive bad boots)
     dracut pre-pivot/40 → write /run/hisnos/safemode-active
     systemd boots to rescue.target

 (B) Kernel cmdline: hisnos.safemode=1
     cmdline-check.sh sets flag → same path as (A)

 (C) Cmdline policy violation + hisnos.strict=1
     cmdline-check.sh: panic_banner() → halt

 (D) hisnosd threat score ≥ 95 (TierCritical)
     hisnosd safe-mode IPC → IPC blocks mutating commands
     Non-essential services masked (gaming, hispowerd)
     Containment rules applied

 (E) Self-healer: ≥3 distinct services fail within 2 min
     onCorrelatedFailure() → hisnosd safe-mode entry
     Event emitted: health/safe_mode_triggered

Safe-mode exit:
   operator confirms: hisnosd IPC "acknowledge_safe_mode"
   (requires confirm:true + operator ID)
   OR reboot without trigger conditions
```

## Threat Signal Flow

```
Kernel / audit events
        │
        ├─ auditd → hisnos-logd (JSONL) → /var/log/hisnos/security.jsonl
        │
        ├─ procfs polling (namespace_census, rt_guard)
        │
        ├─ nftables log → systemd journal → threat engine
        │
        └─ gocryptfs mount events → vault_exposure signal
                │
                ▼
         Threat Engine (core/threat/engine/)
                │
         ┌──────┴───────────────────────┐
         │  Signal scorer (0-100 each)  │
         │  ┌─────────────────────────┐ │
         │  │ privilege_escalation    │ │
         │  │ namespace_abuse         │ │
         │  │ firewall_anomaly        │ │
         │  │ vault_exposure          │ │
         │  │ persistence_signal      │ │
         │  │ kernel_integrity        │ │
         │  │ rt_escalation (rt_guard)│ │
         │  └─────────────────────────┘ │
         └──────┬───────────────────────┘
                │
         Composite Score (weighted average)
                │
         ┌──────▼────────────────────────┐
         │  Automation Engine           │
         │  ┌────────────────────────┐  │
         │  │ Holt's EMA predictor   │  │
         │  │ Temporal cluster       │  │
         │  │ Baseline z-score       │  │
         │  │ Confidence model       │  │
         │  │ Response orchestrator  │  │
         │  └────────────────────────┘  │
         └──────┬────────────────────────┘
                │
         Response Matrix
                │
         ┌──────▼────────────────────────┐
         │ alert → emit → dashboard     │
         │ contain → nftables/cgroup    │
         │ safemode → hisnosd IPC       │
         │ operator prompt → pending Q  │
         └───────────────────────────────┘
```

## Update Pipeline

```
hisnos-update-check.timer (weekly, ±4h jitter)
        │
        ▼
hisnos-update-check.service
        │
        ▼
update/hisnos-update-preflight.sh
   • Check disk space (≥5GB free)
   • Verify network
   • Check battery/power (laptops)
   • Verify no gaming session active
        │
        ▼ (preflight OK)
rpm-ostree upgrade --check
        │
        ├─ No update available → log + exit 0
        │
        └─ Update available →
              rpm-ostree upgrade (stage only)
                    │
                    ▼
              Rollback confidence scoring
              • Age of current deployment
              • Service health check
              • Boot score
                    │
                    ├─ Score ≥ 70 → Automatic staged upgrade
                    │              (apply on next reboot)
                    │
                    └─ Score < 70 → Operator approval required
                                   (IPC pending confirmation queue)

Rollback:
   rpm-ostree rollback  OR  hisnos IPC "trigger_rollback"
   → GlobalRollback.RollbackToLatest()
   → All subsystem snapshots restored in reverse order
```

## Directory Structure

```
HisnOS-Secure-Workstation/
├── build/
│   ├── iso/
│   │   ├── treefile.json           OSTree compose definition
│   │   ├── treefile-postprocess.sh Post-compose rootfs hook
│   │   ├── build-hisnos-iso.sh     Full ISO build pipeline
│   │   ├── repos/                  Fedora .repo files
│   │   ├── test-iso-qemu.sh        QEMU boot test
│   │   └── sign-iso.sh             GPG + SHA256 signing
│   └── ostree/
│       └── compose.sh              OSTree compose orchestrator
├── kickstart/
│   └── hisnos-install.ks           Anaconda/Calamares KS
├── lorax/
│   └── tmpl.d/hisnos.tmpl          Lorax ISO template
├── dracut/
│   ├── 95hisnos/                   Custom dracut module
│   │   ├── module-setup.sh
│   │   ├── hisnos-lib.sh           Shared helpers
│   │   ├── hisnos-cmdline-check.sh Pre-udev policy check
│   │   ├── hisnos-boot.sh          Pre-pivot health check
│   │   ├── hisnos-recovery-menu.sh Interactive recovery
│   │   └── hisnos-vault-unlock.sh  TPM2 vault pre-unlock
│   └── install-dracut-module.sh    Install + rebuild initramfs
├── systemd/
│   ├── hisnos-boot-complete.service
│   ├── hisnos-safe-mode.service
│   ├── hisnos-threat-engine.service
│   ├── hisnos-automation.service
│   └── hisnos-performance-guard.service
├── security/
│   └── nftables-base.nft           Base firewall policy
├── core/                           hisnosd control runtime
├── recovery/
│   ├── dracut/95hisnos-recovery/   Legacy dracut module
│   └── grub.d/41_hisnos-recovery   GRUB recovery entries
├── bootstrap/
│   └── bootstrap-installer.sh      15-step installer
└── docs/
    ├── AI-REPORT.md
    ├── ARCHITECTURE-CORE.md
    └── BUILD-PIPELINE.md           ← This file
```

## Build Instructions

### Prerequisites

```bash
# On Fedora build host:
sudo dnf install -y \
    rpm-ostree ostree \
    lorax lorax-lmc-novirt \
    xorriso syslinux \
    python3 jq \
    gpg gpgme \
    rsync curl \
    qemu-kvm-core   # for test
```

### Step 1: OSTree Compose

```bash
cd build/ostree
sudo bash compose.sh \
    --treefile ../iso/treefile.json \
    --repo ./ostree-repo \
    --force
```

### Step 2: Build ISO

```bash
cd build/iso
sudo bash build-hisnos-iso.sh
# Output: build/iso/output/HisnOS-1.0-x86_64.iso
```

### Step 3: Test (QEMU)

```bash
bash build/iso/test-iso-qemu.sh build/iso/output/HisnOS-1.0-x86_64.iso
```

### Step 4: Sign

```bash
bash build/iso/sign-iso.sh build/iso/output/HisnOS-1.0-x86_64.iso \
    --key-id your-gpg-key-id
```

### Step 5: Install dracut module (on target system)

```bash
sudo bash dracut/install-dracut-module.sh
```

## Operator Commands Reference

| Command | IPC Method | Description |
|---------|-----------|-------------|
| Status | `get_status` | Full system status |
| Vault unlock | `vault_unlock` | Unlock encrypted vault |
| Vault lock | `vault_lock` | Lock vault immediately |
| Gaming ON | `gaming_start` | Apply performance profile |
| Gaming OFF | `gaming_stop` | Restore default profile |
| Threat status | `get_threat_status` | Current threat score |
| Update check | `check_updates` | Check for OS updates |
| Update apply | `apply_update` | Stage + apply update |
| Rollback | `trigger_rollback` | OSTree rollback |
| Safe mode | `acknowledge_safe_mode` | Exit safe mode |
| Profile | `apply_profile` | Set performance profile |
| Automation | `get_automation_status` | AI engine status |
| Fleet | `get_fleet_status` | Fleet sync status |

### IPC Usage

```bash
# Via socat (CLI)
echo '{"jsonrpc":"2.0","id":1,"method":"get_status","params":{}}' \
  | socat - UNIX-CONNECT:/run/user/$(id -u)/hisnosd.sock

# Via dashboard (browser)
open http://localhost:7374

# Via hisnos-pkg (marketplace)
hisnos-pkg list
hisnos-pkg install <name>
```

## Failure Recovery Guide

### Boot failure (score < 40)
```
1. System boots to rescue.target automatically
2. Add hisnos.recovery=1 to GRUB cmdline
3. In recovery menu: check journal (option 5)
4. Rollback if needed (option 4)
5. Or chroot + fix (option 2 → 3)
```

### Firewall locked out
```
# In recovery menu option 7 (firewall reset)
# Or from chroot:
nft flush ruleset
systemctl restart nftables
```

### Vault corrupt
```
# From recovery shell:
gocryptfs -debug /var/lib/hisnos/vault-cipher /tmp/vault-check
# Or check integrity:
gocryptfs -fsck /var/lib/hisnos/vault-cipher
```

### OSTree rollback
```bash
# From live system:
rpm-ostree rollback
reboot

# From recovery menu (option 4):
# Automatic rpm-ostree rollback --sysroot=/sysroot
```

## Threat Model Summary

| Attack vector | Mitigation |
|--------------|-----------|
| Root escalation | rt_guard + namespace_census + audit |
| Network exfiltration | nftables default-deny egress |
| Persistence | persistence_signal + immutable /usr |
| Vault exposure | gocryptfs AES-256 + auto-lock |
| Supply chain | OSTree GPG + rpm GPG verify |
| Kernel tampering | kernel_integrity signal + lockdown |
| Live memory | No swap encryption (TODO Phase 2) |
| Physical access | Full disk encryption (LUKS, optional) |

## Release Engineering Flow

```
1. git tag v1.0.x
2. CI: compose.sh → verify commit hash
3. CI: build-hisnos-iso.sh
4. CI: test-iso-qemu.sh (must pass)
5. CI: sign-iso.sh --key-id releases@hisnos.dev
6. CI: generate-manifest.sh
7. Upload to:
   - CDN: cdn.hisnos.dev/releases/v1.0.x/
   - OSTree remote: ostree.hisnos.dev
8. Update channel pointer (stable/beta/hardened)
9. Fleet sync distributes policy bundle update
```
