# HisnOS Architecture Diagram

**Version:** Phase 8 / Production
**Base OS:** Fedora Kinoite (KDE Plasma 6, immutable/rpm-ostree)

---

## System Overview

```
╔══════════════════════════════════════════════════════════════════════════════╗
║                    HisnOS Secure Workstation                                ║
║                    (Fedora Kinoite — immutable)                             ║
╠══════════════════════════════════════════════════════════════════════════════╣
║                                                                              ║
║  ┌─────────────────────────────────────────────────────────────────────┐    ║
║  │  KDE Plasma 6 Desktop (Work Zone)                                   │    ║
║  │                                                                     │    ║
║  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────────────────┐  │    ║
║  │  │ Browser      │  │ IDE / Tools  │  │ OpenSnitch (per-app GUI) │  │    ║
║  │  └──────┬───────┘  └──────┬───────┘  └──────────────────────────┘  │    ║
║  │         │                 │                                         │    ║
║  └─────────┼─────────────────┼─────────────────────────────────────────┘   ║
║            │                 │                                              ║
║  ┌─────────┼─────────────────┼─────────────────────────────────────────┐   ║
║  │  Vault Zone (gocryptfs AES-256-GCM)                                 │   ║
║  │                                                                     │   ║
║  │  ~/.local/share/hisnos/vault/                                       │   ║
║  │  ┌──────────────────┐  ┌──────────────┐  ┌─────────────────────┐  │   ║
║  │  │ hisnos-vault.sh  │  │ vault-watcher│  │ vault-idle.timer    │  │   ║
║  │  │ init/mount/lock  │  │ (D-Bus KSCL) │  │ → idle.service lock │  │   ║
║  │  └──────────────────┘  └──────────────┘  └─────────────────────┘  │   ║
║  └─────────────────────────────────────────────────────────────────────┘   ║
║                                                                              ║
║  ┌──────────────────────────────────────────────────────────────────────┐   ║
║  │  Lab Zone                                                            │   ║
║  │                                                                      │   ║
║  │  ┌─────────────────────────┐   ┌────────────────────────────────┐   │   ║
║  │  │  Light Lab              │   │  Full Lab                      │   │   ║
║  │  │  Distrobox (Kali)       │   │  KVM/QEMU + libvirt            │   │   ║
║  │  │  bwrap + user NS        │   │  Hardware VM isolation         │   │   ║
║  │  │  ↕ veth pair (netd)    │   │  ↕ virbr0 / isolated net       │   │   ║
║  │  └─────────────────────────┘   └────────────────────────────────┘   │   ║
║  │                                                                      │   ║
║  │  ┌──────────────────────────────────────────────────────────────┐   │   ║
║  │  │  hisnos-lab-netd.sh (privileged, systemd socket-activated)   │   │   ║
║  │  │  veth create/delete  ·  nftables lab rules  ·  DNS relay     │   │   ║
║  │  └──────────────────────────────────────────────────────────────┘   │   ║
║  └──────────────────────────────────────────────────────────────────────┘   ║
║                                                                              ║
╠══════════════════════════════════════════════════════════════════════════════╣
║                        Governance & Monitoring                              ║
╠══════════════════════════════════════════════════════════════════════════════╣
║                                                                              ║
║  ┌──────────────────────────────────────────────────────────────────────┐   ║
║  │  Governance Dashboard (localhost:9443)                               │   ║
║  │  systemd socket-activated · Go net/http · SvelteKit frontend        │   ║
║  │                                                                      │   ║
║  │  GET  /api/health          POST /api/vault/lock                      │   ║
║  │  GET  /api/vault/status    POST /api/vault/mount                     │   ║
║  │  GET  /api/firewall/status POST /api/firewall/reload                 │   ║
║  │  GET  /api/lab/status      POST /api/lab/start                       │   ║
║  │  GET  /api/threat/status   POST /api/gaming/start                    │   ║
║  │  GET  /api/audit/summary   POST /api/update/apply                    │   ║
║  │  GET  /api/journal/stream  (SSE)                                     │   ║
║  └──────────────────────────────────────────────────────────────────────┘   ║
║                                                                              ║
║  ┌─────────────────────────────┐  ┌──────────────────────────────────────┐  ║
║  │  Threat Intelligence        │  │  Audit Pipeline                      │  ║
║  │  hisnos-threatd             │  │  hisnos-logd                         │  ║
║  │                             │  │                                      │  ║
║  │  Tails: current.jsonl       │  │  Subscribes to:                      │  ║
║  │  Window: 1h rolling         │  │  · journalctl HISNOS_* tags          │  ║
║  │  Burst window: 5min         │  │  · journalctl _TRANSPORT=audit       │  ║
║  │  Eval: every 30s            │  │                                      │  ║
║  │                             │  │  Writes: /var/lib/hisnos/audit/      │  ║
║  │  Signals (6):               │  │  current.jsonl (50MB, rotate+gzip)   │  ║
║  │  · lab_session_active +15   │  │  Retention: 14 days                  │  ║
║  │  · ns_burst          +20   │  │                                      │  ║
║  │  · fw_block_rate     +15   │  └──────────────────────────────────────┘  ║
║  │  · nft_modified      +25   │                                            ║
║  │  · vault_exposure    +15   │  ┌──────────────────────────────────────┐  ║
║  │  · priv_exec_burst   +10   │  │  auditd                              │  ║
║  │                             │  │  /etc/audit/rules.d/hisnos.rules     │  ║
║  │  Output:                    │  │                                      │  ║
║  │  /var/lib/hisnos/           │  │  Keys: hisnos_ns, hisnos_ns_clone    │  ║
║  │    threat-state.json        │  │  hisnos_mount, hisnos_lab_exec       │  ║
║  │    threat-timeline.jsonl    │  │  hisnos_nft, hisnos_ostree           │  ║
║  └─────────────────────────────┘  │  hisnos_priv, hisnos_vault           │  ║
║                                   │  hisnos_audit_tamper                 │  ║
║                                   │  hisnos_config_tamper                │  ║
║                                   └──────────────────────────────────────┘  ║
║                                                                              ║
╠══════════════════════════════════════════════════════════════════════════════╣
║                          Kernel / System Layer                              ║
╠══════════════════════════════════════════════════════════════════════════════╣
║                                                                              ║
║  ┌──────────────────────────────────────────────────────────────────────┐   ║
║  │  nftables (egress firewall)                                          │   ║
║  │                                                                      │   ║
║  │  inet hisnos table:                                                  │   ║
║  │  · input chain     (policy drop — external interfaces)              │   ║
║  │  · output chain    (policy drop — default deny egress)              │   ║
║  │  · forward chain   (lab traffic isolation)                          │   ║
║  │  · gaming_output   (gaming-optimized rules — loaded on demand)      │   ║
║  │  · gaming_input    (gaming-optimized rules — loaded on demand)      │   ║
║  │  · lab_output      (per-session lab egress rules)                   │   ║
║  │                                                                      │   ║
║  │  Allowlists: /etc/nftables/hisnos-*.nft                             │   ║
║  └──────────────────────────────────────────────────────────────────────┘   ║
║                                                                              ║
║  ┌────────────────────┐  ┌──────────────────┐  ┌──────────────────────┐    ║
║  │  Gaming Mode       │  │  cgroups v2       │  │  OpenSnitch          │    ║
║  │                    │  │                  │  │                      │    ║
║  │  CPU: performance  │  │  Per-service     │  │  Per-application     │    ║
║  │  IRQ: affinity     │  │  MemoryMax/      │  │  prompt-based        │    ║
║  │  nft: gaming chain │  │  CPUQuota limits │  │  egress rules        │    ║
║  │  Vault: no-lock    │  │  on all hisnos   │  │  (GUI tray icon)     │    ║
║  │                    │  │  daemons         │  │                      │    ║
║  │  polkit: hisnos-   │  └──────────────────┘  └──────────────────────┘    ║
║  │  gaming group      │                                                     ║
║  └────────────────────┘                                                     ║
║                                                                              ║
║  ┌──────────────────────────────────────────────────────────────────────┐   ║
║  │  rpm-ostree (Fedora Kinoite immutable base)                          │   ║
║  │  · /usr read-only (overlays via rpm-ostree)                         │   ║
║  │  · Deployment rollback available                                    │   ║
║  │  · GPG-verified packages                                            │   ║
║  │  · UEFI Secure Boot                                                 │   ║
║  └──────────────────────────────────────────────────────────────────────┘   ║
║                                                                              ║
╚══════════════════════════════════════════════════════════════════════════════╝
```

---

## Data Flow

```
  Journal (systemd)
       │
       ├─► hisnos-logd ──────────────────────► /var/lib/hisnos/audit/
       │   (HISNOS_* + audit transport)         current.jsonl (50MB/segment)
       │
       ├─► hisnos-threatd ◄──────────────────── /var/lib/hisnos/audit/current.jsonl
       │   (tails JSONL, 1h window)             (inode-aware tailer)
       │         │
       │         ▼
       │   /var/lib/hisnos/threat-state.json  (atomic write, 30s cadence)
       │   /var/lib/hisnos/threat-timeline.jsonl  (appended, 48h retained)
       │
  auditd
       │
       └─► journal (_TRANSPORT=audit) ──────────► hisnos-logd (filtered by hisnos_ key)
```

---

## Service Dependency Graph

```
  systemd (system)
  ├── nftables.service             ← always-on, boot
  ├── auditd.service               ← always-on, boot
  ├── hisnos-lab-netd.socket       ← socket-activated (on lab start)
  │   └── hisnos-lab-netd@.service ← per-connection (privileged helper)
  ├── hisnos-gaming-tuned-start.service  ← oneshot (polkit, on gaming start)
  └── hisnos-gaming-tuned-stop.service   ← oneshot (polkit, on gaming stop)

  systemd --user
  ├── hisnos-dashboard.socket      ← socket-activated (on first connection)
  │   └── hisnos-dashboard.service ← Go binary, port 9443
  ├── hisnos-logd.service          ← always-on, follows journal
  ├── hisnos-threatd.service       ← always-on, 30s eval cycle
  ├── hisnos-vault-watcher.service ← D-Bus subscriber (KDE screen lock)
  ├── hisnos-vault-idle.timer      ← idle-lock timer (suppressed in gaming)
  │   └── hisnos-vault-idle.service ← oneshot: calls hisnos-vault.sh lock
  └── hisnos-gaming.service        ← oneshot: gaming orchestrator
```

---

## File System Layout

```
/
├── etc/
│   ├── nftables/
│   │   ├── hisnos-egress.nft     ← egress allowlist
│   │   ├── hisnos-gaming.nft     ← gaming chain rules (loaded on demand)
│   │   └── hisnos-lab.nft        ← lab isolation rules
│   ├── audit/rules.d/
│   │   └── hisnos.rules          ← auditd policy (hisnos_* keys)
│   ├── hisnos/
│   │   ├── lab/netd/             ← lab-netd privileged scripts
│   │   └── gaming/               ← gaming tuning scripts + polkit rule
│   └── polkit-1/rules.d/
│       └── 10-hisnos-gaming.rules ← polkit for gaming group
│
├── var/lib/hisnos/
│   ├── audit/
│   │   ├── current.jsonl         ← active audit log
│   │   └── hisnos-audit-*.jsonl.gz ← compressed segments (14d)
│   ├── threat-state.json         ← current risk score + signals
│   └── threat-timeline.jsonl     ← score history (48h)
│
└── home/<user>/
    └── .local/share/hisnos/
        ├── bin/                  ← compiled daemons (logd, threatd, dashboard)
        ├── vault/
        │   ├── ciphertext/       ← gocryptfs encrypted store
        │   ├── mount/            ← plaintext mountpoint
        │   └── hisnos-vault.sh
        ├── update/
        │   ├── hisnos-update.sh
        │   └── hisnos-update-preflight.sh
        ├── lab/
        │   └── runtime/
        │       └── hisnos-lab-runtime.sh
        └── gaming/
            └── hisnos-gaming.sh
```
