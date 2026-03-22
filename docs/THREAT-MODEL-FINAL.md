# HisnOS Threat Model (Final)

**System:** HisnOS Secure Workstation Layer on Fedora Kinoite
**Scope:** Single-user, daily-driver workstation with security-sensitive work zones
**Version:** Phase 8 / Production
**Date:** 2026-03-22

---

## 1. System Overview

HisnOS is a hardened configuration and tooling layer on top of Fedora Kinoite (immutable, rpm-ostree). It is NOT a new OS. The security model assumes:

- Physical security of the workstation is maintained by the operator.
- The operator is the sole user of the system.
- The primary threat surface is software-based: malicious code executing in user or system space.

### Trust Zones

```
┌─────────────────────────────────────────────────────────────────┐
│ Work Zone (Normal Desktop)                                      │
│  - KDE Plasma 6, standard user applications                     │
│  - Egress: nftables default-deny + OpenSnitch per-app rules     │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ Vault Zone (gocryptfs AES-256-GCM)                        │  │
│  │  - Sensitive documents, credentials, keys                 │  │
│  │  - Auto-lock on idle + screen lock                        │  │
│  │  - Threat signal: vault_exposure after 30min              │  │
│  └───────────────────────────────────────────────────────────┘  │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ Lab Zone (Distrobox/KVM — untrusted code execution)       │  │
│  │  - Isolated veth networking (no host routing by default)  │  │
│  │  - bwrap sandboxing for light lab sessions                │  │
│  │  - KVM/QEMU full isolation for high-risk analysis         │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
│ Governance Layer (localhost only)                               │
│  Dashboard (Go + SvelteKit, socket-activated, port 9443)       │
│  Threat Engine (threatd, 30s eval cycle)                       │
│  Audit Pipeline (logd + auditd → /var/lib/hisnos/audit/)       │
└─────────────────────────────────────────────────────────────────┘
```

---

## 2. Threat Actors

### TA1: Malicious Downloaded Code
**Likelihood:** High | **Impact:** High

Code executed in the work zone (browser downloads, scripts, packages) that attempts to:
- Exfiltrate data via network
- Access vault contents
- Escalate privileges
- Persist across reboots

**Mitigations:**
- nftables default-deny egress blocks unexpected outbound connections.
- OpenSnitch prompts for per-application allow decisions.
- Vault is encrypted at rest; lock on idle prevents access without passphrase.
- auditd + threatd detects privilege escalation patterns (priv_exec_burst signal).
- rpm-ostree immutability prevents persistent modification of /usr.

### TA2: Lab Escape Attempt
**Likelihood:** Medium | **Impact:** High

Malicious code running inside a Distrobox or KVM VM that attempts to break out to the host:
- Namespace escape via kernel vulnerability
- Network bridge exploitation to reach host-only services
- Shared filesystem traversal

**Mitigations:**
- Distrobox shares the host kernel — NOT a security boundary against kernel exploits.
- KVM/QEMU provides stronger isolation (hardware virtualization boundary).
- veth-based networking with nftables rules restricts lab-to-host routing.
- hisnos-lab-netd.sh manages veth lifecycle with cleanup on session end.
- Threat engine monitors ns_burst signal (>3 namespace operations in 5 minutes).

**Explicit limitation:** Distrobox is a convenience boundary only, not a security boundary.

### TA3: Credential/Key Theft
**Likelihood:** Medium | **Impact:** Critical

Code accessing sensitive files, API keys, SSH keys, passwords stored in the vault or in memory:
- Reading vault contents while mounted
- Memory scraping for gocryptfs key material
- Accessing `~/.ssh`, `~/.gnupg` outside vault

**Mitigations:**
- Vault auto-locks on idle and screen lock — minimize exposure window.
- vault_exposure threat signal alerts when vault has been mounted >30 minutes.
- Files outside vault are not protected by encryption (operator responsibility).

**Explicit limitation:** gocryptfs key material resides in memory while vault is mounted. Cold boot attack or memory dump can extract it.

### TA4: Supply Chain Compromise
**Likelihood:** Low | **Impact:** Critical

Malicious update packages delivered via rpm-ostree update mechanism:
- Tampered RPM packages
- Compromised Fedora/RPM Fusion repositories

**Mitigations:**
- rpm-ostree verifies GPG signatures on all packages.
- hisnos-update-preflight.sh validates system state before applying updates.
- Rollback deployment retained for immediate recovery.
- Immutable OS means no silent persistence without an update cycle.

**Explicit limitation:** HisnOS does not add additional binary transparency or attestation beyond what Fedora provides.

### TA5: Physical Access (Limited Scope)
**Likelihood:** Low | **Impact:** High (assumed physical security maintained)

Attacker with brief physical access (evil maid):
- Boot from external media
- Access unencrypted disk

**Mitigations:**
- Full disk encryption (LUKS) at OS layer (Fedora Kinoite default on install).
- UEFI Secure Boot (Fedora default).
- Vault adds a second encryption layer on top of LUKS.

**Explicit limitation:** Physical security is operator responsibility. Extended physical access (disk removal) defeats LUKS without passphrase.

---

## 3. Attack Surface

### Network

| Surface | Exposure | Mitigation |
|---|---|---|
| Egress — all protocols | Default-deny | nftables + OpenSnitch allowlist |
| Ingress — all ports | Blocked by default | No listening services on external interfaces |
| Lab veth bridge | Isolated per-session | netd manages lifecycle; nftables blocks lab→host |
| Dashboard (9443) | localhost only | Socket-activated; no TLS (loopback only) |
| D-Bus | User session bus | Vault watcher uses D-Bus for screen-lock signal only |

### Filesystem

| Path | Sensitivity | Protection |
|---|---|---|
| `/var/lib/hisnos/` | Threat state, audit logs | Root-owned; operator reads; logd/threatd write |
| `/etc/hisnos/` | Configuration, scripts | Root-owned; immutable after bootstrap |
| `~/.local/share/hisnos/` | Binaries, runtime state | User-owned; checked by dashboard |
| Vault mountpoint | Encrypted sensitive data | gocryptfs AES-256-GCM; auto-lock |
| `/etc/nftables/` | Firewall rules | Root-owned; tamper detected by auditd (hisnos_nft key) |

### Privileged Operations

| Operation | Mechanism | Authorization |
|---|---|---|
| Firewall reload | `sudo systemctl reload nftables.service` | sudoers or polkit |
| Gaming CPU/IRQ tuning | system oneshot service via polkit | `hisnos-gaming` group + polkit rule |
| Lab veth management | `hisnos-lab-netd.sh` via systemd socket | `hisnos-lab` group + systemd socket activation |
| Audit rule load | `sudo augenrules --load` | bootstrap only (one-time) |

---

## 4. Non-Goals (Explicit Limitations)

These threats are **out of scope** for HisnOS MVP:

| Threat | Why Out of Scope |
|---|---|
| Root compromise | Root bypasses all userspace controls |
| Kernel exploits | No custom kernel module protection in MVP |
| DLP (clipboard/screen exfiltration) | No screenshot/clipboard interception |
| Anti-forensics | No log tampering protection beyond auditd |
| Network traffic analysis | No IDS/IPS; OpenSnitch is allow/deny only |
| Process injection into allowlisted apps | OpenSnitch cannot detect injected traffic |
| Deep SELinux policy authoring | Default Fedora SELinux policy in MVP |
| Memory dump protection | Cold boot attack extracts vault key if mounted |
| Distrobox kernel escape | Distrobox shares host kernel — not a security boundary |

---

## 5. Threat Detection Coverage

### Real-time Signals (threatd, 30s eval cycle)

| Signal | What It Detects | Score Impact |
|---|---|---|
| `lab_session_active` | Active bwrap/lab session | +15 (baseline) |
| `ns_burst` | ≥3 namespace ops in 5 min | +20 (lateral movement indicator) |
| `fw_block_rate` | ≥10 blocked connections in 5 min | +15 (exfiltration attempt) |
| `nft_modified` | nftables ruleset change in audit log | +25 (firewall bypass attempt) |
| `vault_exposure` | Vault mounted >30 min | +15 (exposure window) |
| `priv_exec_burst` | ≥3 privilege escalations in 5 min | +10 (escalation attempt) |

### Audit Coverage (auditd rules)

| Activity | Audit Key | Rule |
|---|---|---|
| Namespace creation (unshare/clone) | `hisnos_ns`, `hisnos_ns_clone` | syscall unshare, clone |
| Mount operations | `hisnos_mount` | syscall mount, umount2 |
| Lab binary execution | `hisnos_lab_exec` | execve from bwrap |
| nftables modification | `hisnos_nft` | execve of /usr/sbin/nft |
| rpm-ostree execution | `hisnos_ostree` | execve of /usr/bin/rpm-ostree |
| Privilege escalation | `hisnos_priv` | execve of sudo/su/pkexec |
| Vault access | `hisnos_vault` | execve of gocryptfs |
| Audit tampering | `hisnos_audit_tamper`, `hisnos_audit_config` | write to /var/log/audit/, /etc/audit/ |
| Config tampering | `hisnos_config_tamper` | write to /etc/hisnos/, /etc/nftables/ |

---

## 6. Security Controls Summary

| Control | Implementation | Coverage |
|---|---|---|
| Egress firewall | nftables default-deny | Network exfiltration |
| Per-app network rules | OpenSnitch | Fine-grained egress |
| Vault encryption | gocryptfs AES-256-GCM | Data at rest |
| Vault auto-lock | idle timer + screen-lock watcher | Exposure window |
| Kernel audit | auditd + hisnos.rules | Privilege/namespace/config activity |
| Audit pipeline | hisnos-logd (JSONL, 14-day) | Forensic evidence |
| Threat scoring | hisnos-threatd (6 signals, 30s) | Real-time risk awareness |
| Lab isolation | bwrap + veth + nftables | Untrusted code execution |
| Immutable OS | rpm-ostree | Persistent malware resistance |
| Rollback | rpm-ostree deployment history | Update safety |
| Recovery CLI | hisnos-recover.sh | Operator emergency response |

---

## 7. Residual Risk Acceptance

The following residual risks are explicitly accepted for MVP:

1. **Memory key exposure:** Vault key in RAM while mounted. Acceptable for workstation use; operator locks vault when away.
2. **Root omnipotence:** Root access defeats all controls. Acceptable; root is not routinely used.
3. **Distrobox kernel sharing:** Not a security boundary. Operator uses KVM for high-risk analysis.
4. **OpenSnitch process injection bypass:** Low likelihood; covered by threat monitoring of ns_burst/priv_exec_burst signals.
5. **No DLP:** Clipboard/screen exfiltration not blocked. Operator responsibility for sensitive data handling.
