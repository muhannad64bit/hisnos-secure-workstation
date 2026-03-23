# HisnOS Core Architecture — Phase 14 Reference

> **Audience:** Core contributors, security auditors, on-call operators.
> **Scope:** `core/` — the `hisnosd` daemon and all subsystems wired into it.
> This document is generated at the end of Phase 14 (Core Security Hardening).
> Last updated: 2026-03-22

---

## 1. Directory Tree

```
core/
├── go.mod                              # module hisnos.local/hisnosd
├── main.go                             # 15-step startup sequence
│
├── state/                              # SystemState + atomic persistence
│   ├── state.go                        # SystemState struct + Mode type
│   └── manager.go                      # atomic JSON write, RWMutex
│
├── eventbus/                           # In-process typed event bus
│   └── bus.go
│
├── policy/                             # Static policy rules (no side-effects)
│   └── engine.go                       # CanStartLab, CanStartGaming, etc.
│
├── runtime/                            # Control-plane primitives (Phase 14)
│   ├── transaction_manager.go          # WAL journal, schema migration, replay
│   ├── leader_guard.go                 # flock-based single-instance guard
│   ├── watchdog.go                     # per-subsystem heartbeat + escalation
│   ├── policy_enforcer.go              # priority-queue action dispatcher
│   └── safemode.go                     # safe-mode lifecycle enforcer
│
├── telemetry/                          # Observability (Phase 14)
│   └── security_events_stream.go       # 10 000-entry ring buffer, pub/sub, journal
│
├── forensics/                          # Forensic capture (Phase 14)
│   └── snapshot.go                     # tar.gz bundle of system state
│
├── threat/
│   └── engine/                         # Threat scoring engine (Phase 14)
│       ├── signal.go                   # Signal interface + decay math
│       ├── engine.go                   # scoring orchestrator
│       ├── namespace_abuse.go          # signal: unexpected namespaces
│       ├── privilege_escalation.go     # signal: dangerous caps / SUID
│       ├── firewall_anomaly.go         # signal: nftables tampering
│       ├── vault_exposure.go           # signal: vault overexposure
│       ├── persistence_signal.go       # signal: persistence mechanism changes
│       ├── kernel_integrity_signal.go  # signal: dmesg / sysctl / modules
│       ├── risk_projection.go          # linear regression on score history
│       └── response_matrix.go          # edge-triggered action dispatch
│
├── security/
│   ├── integrity/
│   │   └── verifier.go                 # SHA-256 baseline + violation scoring
│   ├── isolation/
│   │   └── namespace_census.go         # /proc namespace census, orphan detection
│   └── containment/
│       └── containment.go              # emergency network + cgroup isolation
│
├── orchestrator/                       # Domain orchestrators (Phases 1-12)
│   ├── vault.go
│   ├── firewall.go
│   ├── lab.go
│   ├── gaming.go
│   └── update.go
│
├── supervisor/                         # Subsystem lifecycle
│   └── supervisor.go
│
└── ipc/
    └── server.go                       # Unix socket JSON-RPC + safe-mode gate
```

---

## 2. Go Modules Layout

```
module hisnos.local/hisnosd            (core/go.mod)

Internal packages (no external deps):
  hisnos.local/hisnosd/state
  hisnos.local/hisnosd/eventbus
  hisnos.local/hisnosd/policy
  hisnos.local/hisnosd/runtime
  hisnos.local/hisnosd/telemetry
  hisnos.local/hisnosd/forensics
  hisnos.local/hisnosd/threat/engine
  hisnos.local/hisnosd/security/integrity
  hisnos.local/hisnosd/security/isolation
  hisnos.local/hisnosd/security/containment
  hisnos.local/hisnosd/orchestrator
  hisnos.local/hisnosd/supervisor
  hisnos.local/hisnosd/ipc

Stdlib packages used (selection):
  container/heap     — PolicyEnforcer priority queue
  crypto/sha256      — Integrity baseline + journal checksums
  crypto/rand        — Transaction IDs
  encoding/json      — All persistence
  net                — IPC Unix socket + journal UDP
  os/exec            — nft, auditctl, systemctl, notify-send
  sync               — RWMutex throughout
  syscall            — flock, Kill(pid,0), MS_REMOUNT|MS_RDONLY

NO external dependencies.
```

---

## 3. Service Dependency Graph

```
systemd
  └─ hisnosd.service (socket-activated via hisnosd.socket)
       │
       ├─[init]─ LeaderGuard ──────────────── /run/user/<uid>/hisnosd.lock
       │
       ├─[init]─ state.Manager ─────────────► /var/lib/hisnos/core-state.json
       │
       ├─[init]─ TransactionManager ────────► /var/lib/hisnos/state.journal
       │              │
       │              └─ ReplayJournal() ──► SafeModeEnforcer.Enter() [if corrupted]
       │
       ├─[init]─ SecurityEventStream ───────► /var/log/hisnos/security-events.jsonl
       │                                    ► /run/systemd/journal/socket (native)
       │
       ├─[init]─ SafeModeEnforcer ──────────► /var/lib/hisnos/safe-mode-state.json
       │              │                     ► nft (strict ruleset)
       │              │                     ► systemctl stop hispowerd
       │              │                     ► auditctl -f 2
       │              │                     ► notify-send (critical)
       │              └─ IsBlocked(cmd) ───► ipc.Server.safeModeGate
       │
       ├─[init]─ Watchdog ─────────────────► per-subsystem goroutines
       │              │
       │              └─ escalation ladder:
       │                   WARN → RESTART → CIRCUIT_OPEN → SAFE_MODE → OPERATOR_ALERT
       │                                                    ▲
       │                                          SafeModeEnforcer.Enter()
       │
       ├─[init]─ PolicyEnforcer ────────────► priority queue (container/heap)
       │              │                     ► dead-letter: /var/lib/hisnos/policy-dead-letter.json
       │              └─ ActionFn ─────────► orchestrators (vault, firewall, gaming, lab)
       │
       ├─[init]─ ThreatEngine ─────────────► 6 signal goroutines (10s cadence)
       │              │                     ► /var/lib/hisnos/threat-state.json
       │              │
       │              ├─ RiskProjection ───► linear regression on 30 samples
       │              │
       │              └─ ResponseMatrix ──► edge-triggered band actions
       │                                    ► PolicyEnforcer.Submit()
       │                                    ► SafeModeEnforcer.Enter() [CRITICAL band]
       │
       ├─[loop]─ IntegrityVerifier ─────────► every 5 min
       │              │                     ► /var/lib/hisnos/integrity-baseline.json
       │              └─ Violation ────────► SecurityEventStream.Emit()
       │
       ├─[loop]─ NamespaceCensus ───────────► every 2 min
       │              │                     ► /var/lib/hisnos/namespace-census.json
       │              └─ onOrphan ─────────► SecurityEventStream.Emit()
       │
       └─[serve]─ ipc.Server ───────────────► /run/user/<uid>/hisnosd.sock
                       │                    ► eventbus.Bus (mode changes, etc.)
                       ├─ safeModeGate ─────► SafeModeEnforcer.IsBlocked()
                       └─ onAcknowledgeSafeMode → SafeModeEnforcer.Exit()
```

---

## 4. State Transaction Flow

```
Operator/trigger calls: txMgr.Apply("op_name", mutationFn)
                                │
              ┌─────────────────▼──────────────────────┐
              │  1. Read current state snapshot         │
              │  2. Apply mutationFn to copy → "after"  │
              │  3. Write JOURNAL INTENT entry           │
              │     { id, op, timestamp,                 │
              │       checksum(after), committed=false } │
              │  4. state.Manager.Update(mutationFn)    │  ← atomic JSON rename
              │  5. Write JOURNAL COMMIT entry           │
              │     { id, committed=true }              │
              └────────────────────────────────────────┘

On startup — ReplayJournal():
  FOR each entry in journal (JSONL, oldest first):
    IF committed=true  → verify checksum → OK
    IF committed=false → CORRUPTION DETECTED
      → SafeModeEnforcer.Enter("state_corruption_on_startup")
      → return corrupted=true

Journal rotation: every 500 entries → rename to .journal.old, open fresh

Schema migration (MigrateSchema()):
  Read state.Version
  IF version < current → apply incremental upgrade functions
  → persist migrated state
```

---

## 5. Threat Scoring Algorithm

```
INPUT: N signals, each with { raw_i, weight_i, halfLife_i, sampledAt_i }

STEP 1 — Decay each raw score:
  age_i       = now - sampledAt_i
  decayed_i   = raw_i × exp(−ln(2) / halfLife_i × age_i)
  (exp computed via Taylor series — stdlib-free)

STEP 2 — Weighted average:
  base_score  = Σ(decayed_i × weight_i) / Σ(weight_i)

STEP 3 — Burst bonus  [spikes in last 30s]:
  spikeCount  = count(decayed_i > 50.0)
  IF spikeCount ≥ 3:
    burst_bonus = min(25, 10 + (spikeCount - 3) × 5)
  ELSE burst_bonus = 0

STEP 4 — Cluster bonus  [multiple signals within 5s]:
  clusterCount = count(sampledAt_i within 5s of any other sample)
  IF clusterCount ≥ 2:
    cluster_bonus = 5
  ELSE cluster_bonus = 0

STEP 5 — Final score:
  final = min(100, base_score + burst_bonus + cluster_bonus)

Signal weights (normalised to 1.0):
  privilege_escalation  0.25   halfLife  5 min
  firewall_anomaly      0.20   halfLife  2 min
  namespace_abuse       0.15   halfLife 10 min
  vault_exposure        0.15   halfLife  8 min
  persistence_signal    0.15   halfLife 15 min
  kernel_integrity      0.10   halfLife 20 min

Risk bands:
  MINIMAL  [ 0, 20)
  LOW      [20, 40)
  MEDIUM   [40, 60)
  HIGH     [60, 80)
  CRITICAL [80,100]

Risk projection (RiskProjection):
  Maintain circular buffer of 30 timestamped samples.
  slope = (N·Σxy − Σx·Σy) / (N·Σx² − (Σx)²)   [linear regression]
  IF slope >  +1/min → RISING
  IF slope <  -1/min → FALLING
  IF stdDev(last 10) > 10 → VOLATILE
  ELSE → STABLE
  timeToCritical = (80 − projectedNow) / slope   [if slope > 0, score < 80]
```

---

## 6. Safe-Mode Escalation Flow

```
Trigger (any of):
  A. Watchdog circuit breaker OPEN on critical subsystem
  B. TransactionManager.ReplayJournal() → corrupted=true
  C. ThreatEngine score ≥ 80 AND ResponseMatrix fires "safe_mode_candidate"
  D. Operator IPC: {"command":"set_mode","params":{"mode":"safe-mode"}}

                        │
                        ▼
            SafeModeEnforcer.Enter(reason)
                        │
          ┌─────────────┴──────────────────────┐
          │  Mandatory actions (applyActions)   │
          │  1. nft -f /etc/nftables/hisnos-strict.nft
          │  2. systemctl --user stop hisnos-hispowerd
          │  3. auditctl -f 2                   │
          │  4. notify-send --urgency=critical  │
          │  5. journald structured log         │
          │  6. write safe-mode-state.json      │
          └─────────────────────────────────────┘
                        │
          ┌─────────────▼──────────────────────┐
          │  IPC gate active                    │
          │  BLOCKED: start_lab, start_gaming,  │
          │           prepare_update,           │
          │           unlock_vault              │
          │  ALLOWED: get_state, health,        │
          │           lock_vault, stop_*,       │
          │           acknowledge_safe_mode     │
          └─────────────────────────────────────┘

EXIT (all conditions must hold):
  1. Watchdog reports no circuit-open (watchdogOK=true)
  2. Risk score < 40.0 (DefaultExitThreshold)
  3. Operator sends:
     {"command":"acknowledge_safe_mode","params":{"confirm":true,"operator":"<id>"}}

                        │
            SafeModeEnforcer.Exit(operatorID, score, watchdogOK)
                        │
          ┌─────────────┴──────────────────────┐
          │  Restore actions (restoreActions)   │
          │  1. systemctl reload-or-restart nftables
          │  2. auditctl -f 1                   │
          │  3. notify-send --urgency=normal    │
          │  4. write safe-mode-state.json      │
          └─────────────────────────────────────┘

DRY-RUN mode (HISNOS_DRY_RUN=1):
  All exec calls replaced with log.Printf — no system changes.
  Safe for CI and operator simulation.
```

---

## 7. Rollback Flow (State Corruption / Crash)

```
Normal operation:
  state.json ──► WAL journal ──► committed entry ──► OK

Crash mid-transaction (e.g. power loss after INTENT, before COMMIT):
  startup:
    ReplayJournal() finds committed=false entry
    → corrupted = true
    → SafeModeEnforcer.Enter("state_corruption_on_startup")
    → operator must acknowledge after manual inspection

Corrupt state.json (checksum mismatch):
  CorruptionDetected() = true
    → SafeModeEnforcer.Enter("state_file_corrupt")
    → Last known good snapshot: state.journal (replay from last committed entry)

Containment emergency rollback (crash/SIGTERM handler):
  containment.Manager.EmergencyRestore()
    → RestoreFilesystem()   (MS_REMOUNT read-write)
    → ReleaseQuarantine()   (move PIDs back to root cgroup)
    → RestoreNetwork()      (delete hisnos_containment nft table)

IPC server crash:
  LeaderGuard detects stale socket via validateSocket()
  → removes stale Unix socket
  → supervisor restarts hisnosd
  → LeaderGuard.AcquireLeadership() removes stale lock file (Kill(pid,0) check)
```

---

## 8. Operator API Endpoints

All commands use the Unix socket at `$XDG_RUNTIME_DIR/hisnosd.sock`.

### Protocol
```
Request:   {"id":"<uuid>","command":"<name>","params":{...}}\n
Response:  {"id":"<uuid>","ok":true,"data":{...}}\n
           {"id":"<uuid>","ok":false,"error":"<message>"}\n
```

### Command Reference

| Command | Params | Safe-mode | Description |
|---|---|---|---|
| `get_state` | — | ✅ allowed | Full SystemState snapshot |
| `health` | — | ✅ allowed | Mode, risk score, vault/lab status |
| `set_mode` | `mode: string` | ⚠️ only `safe-mode` | Transition operational mode |
| `lock_vault` | — | ✅ allowed | Force vault unmount |
| `unlock_vault` | — | 🚫 blocked | Unlock gocryptfs vault |
| `start_lab` | `profile: string` | 🚫 blocked | Start lab session |
| `stop_lab` | — | ✅ allowed | Stop active lab session |
| `reload_firewall` | — | ⚠️ strict-only | Reload nftables ruleset |
| `prepare_update` | — | 🚫 blocked | Run rpm-ostree preflight |
| `start_gaming` | — | 🚫 blocked | Enable gaming mode (GameMode + nft fastpath) |
| `stop_gaming` | — | ✅ allowed | Stop gaming mode |
| `acknowledge_alert` | — | ✅ allowed | No-op placeholder |
| `acknowledge_safe_mode` | `confirm: true`, `operator: string` | ✅ allowed | Exit safe-mode (subject to exit conditions) |

### Example: Exit safe-mode
```bash
echo '{"id":"1","command":"acknowledge_safe_mode","params":{"confirm":true,"operator":"admin"}}' \
  | socat - UNIX-CONNECT:$XDG_RUNTIME_DIR/hisnosd.sock
# → {"id":"1","ok":true,"data":{"safe_mode_active":false,"operator":"admin"}}
```

### Example: Get current state
```bash
echo '{"id":"2","command":"get_state","params":{}}' \
  | socat - UNIX-CONNECT:$XDG_RUNTIME_DIR/hisnosd.sock
```

### Example: Lock vault
```bash
echo '{"id":"3","command":"lock_vault","params":{}}' \
  | socat - UNIX-CONNECT:$XDG_RUNTIME_DIR/hisnosd.sock
```

---

## 9. Systemd Units

### hisnosd.socket
```ini
[Unit]
Description=HisnOS Control Daemon Socket
PartOf=graphical-session.target

[Socket]
ListenStream=%t/hisnosd.sock
SocketMode=0600
DirectoryMode=0700

[Install]
WantedBy=sockets.target
```

### hisnosd.service
```ini
[Unit]
Description=HisnOS Control Daemon
Requires=hisnosd.socket
After=hisnosd.socket network.target auditd.service nftables.service
PartOf=graphical-session.target

[Service]
Type=notify
ExecStart=/usr/local/bin/hisnosd
Restart=on-failure
RestartSec=5s
TimeoutStartSec=30s

# Cgroup limits
MemoryMax=128M
CPUQuota=20%
TasksMax=64

# Hardening
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=yes
ReadWritePaths=/var/lib/hisnos /var/log/hisnos /run/user/%U /tmp

# DRY_RUN for CI:
# Environment=HISNOS_DRY_RUN=1

[Install]
WantedBy=graphical-session.target
```

### hisnos-integrity-verifier.service
```ini
[Unit]
Description=HisnOS Integrity Verifier
After=hisnosd.service
BindsTo=hisnosd.service

[Service]
Type=oneshot
# Integrated into hisnosd — this unit is a documentation placeholder.
# The verifier runs as a goroutine inside hisnosd on a 5-minute ticker.
ExecStart=/bin/true
RemainAfterExit=yes
MemoryMax=32M
CPUQuota=5%
NoNewPrivileges=yes
```

### hisnos-boot-health.service  (Phase 13)
```ini
[Unit]
Description=HisnOS Boot Health Logger
After=multi-user.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/hisnos-boot-health
MemoryMax=32M
CPUQuota=10%
NoNewPrivileges=yes
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
```

---

## 10. Wiring Checklist (hisnosd startup sequence)

```
Step 1   LeaderGuard.AcquireLeadership(uid, ipcSocket)
Step 2   state.NewManager() + runtime.NewTransactionManager(stateMgr)
Step 3   txMgr.ReplayJournal()           → corrupted bool
Step 4   txMgr.MigrateSchema()
Step 5   telemetry.NewSecurityEventStream("/var/log/hisnos/security-events.jsonl")
Step 6   runtime.NewSafeModeEnforcer(dryRun, onEventFn)
         IF corrupted → safeModeEnforcer.Enter("state_corruption_on_startup")
Step 7   runStartupValidation()           (nftables, hisnos_egress, auditd, cmdline)
Step 8   runtime.NewWatchdog(onSafeModeCandidate)
Step 9   runtime.NewPolicyEnforcer(ctx, deadLetterFn, dryRun)
Step 10  eventbus.New() + all orchestrators
Step 11  ipc.New(...)
         ipcServer.SetSafeModeGate(safeModeEnforcer gate fn)
         ipcServer.SetAcknowledgeSafeModeHandler(exit fn)
Step 12  supervisor.New() + watchdog.Register("nftables", SubsystemSpec{...})
Step 13  ThreatEngine with ResponseMatrix wired to PolicyEnforcer.Submit()
Step 14  Start goroutines: IPC, supervisor, policyEnforcer.Run(), watchdog,
                           integrityVerifier.RunLoop(), namespaceCensus.RunLoop(),
                           threatEngine ticker
Step 15  Emit "hisnosd_started" security event; wait for SIGTERM/SIGINT
```

---

## 11. Key File Locations

| File | Purpose |
|---|---|
| `/var/lib/hisnos/core-state.json` | Authoritative system state (atomic write) |
| `/var/lib/hisnos/state.journal` | Write-ahead log (JSONL) |
| `/var/lib/hisnos/safe-mode-state.json` | Safe-mode persistence (survives restart) |
| `/var/lib/hisnos/integrity-baseline.json` | SHA-256 baselines for unit/nft/ostree checks |
| `/var/lib/hisnos/integrity-report.json` | Latest integrity verification report |
| `/var/lib/hisnos/namespace-census.json` | Latest namespace census snapshot |
| `/var/lib/hisnos/threat-state.json` | Latest threat engine output |
| `/var/lib/hisnos/operator-alert.json` | Watchdog operator alert (critical failures) |
| `/var/lib/hisnos/policy-dead-letter.json` | Failed policy actions |
| `/var/lib/hisnos/forensics/snapshot-<ts>.tar.gz` | Forensic bundles (last 10 kept) |
| `/var/log/hisnos/security-events.jsonl` | Security event stream (JSONL) |
| `/run/user/<uid>/hisnosd.sock` | IPC Unix socket |
| `/run/user/<uid>/hisnosd.lock` | Single-instance flock guard |

---

## 12. Phase 15 — Production Layer Extensions

### 12.1 Directory Tree (Phase 15 additions)

```
core/
├── performance/                         # NEW — Phase 15
│   ├── helpers.go                       # shared sysfs I/O helpers
│   ├── manager.go                       # profile coordinator + IPC handlers
│   ├── cpu_runtime.go                   # governor, turbo boost
│   ├── irq_runtime.go                   # GPU/NIC IRQ affinity
│   ├── io_runtime.go                    # block device IO scheduler
│   ├── memory_runtime.go                # VM parameters, drop_caches, NUMA
│   ├── scheduler_runtime.go             # CFS scheduler sysctl tuning
│   ├── cmdline_profile.go               # kernel boot arg staging (reboot-required)
│   ├── hisnos-perf-apply.sh             # boot-time profile re-apply helper
│   └── systemd/
│       └── hisnos-performance.service   # user unit: re-apply profile on session start
│
├── automation/                          # NEW — Phase 15
│   ├── learning_state.go                # adaptive threshold + incident history
│   ├── risk_predictor.go                # Holt's double EMA, 10-min projection
│   ├── anomaly_cluster.go               # density-based signal clustering
│   ├── response_orchestrator.go         # pre-emptive action dispatch
│   └── decision_engine.go              # main eval loop + IPC handlers
│
└── ecosystem/                           # NEW — Phase 15
    ├── fleet_identity.go                # anonymous fleet ID (SHA-256 of machine-id)
    ├── channel_manager.go               # stable/beta/hardened channel switching
    ├── update_manager.go                # rpm-ostree lifecycle + health scoring
    ├── module_registry.go               # plugin manifest registry
    ├── telemetry_client.go              # optional anonymous telemetry (opt-in)
    ├── manager.go                       # coordinator + IPC handlers
    └── systemd/
        ├── hisnos-update-check.service  # weekly update availability check
        └── hisnos-update-check.timer    # OnCalendar=Mon 02:00, Persistent=true

dashboard/backend/
└── phase15_handlers.go                  # HTTP proxies for automation + update + perf
```

---

### 12.2 Performance Profile Summary

| Profile | Governor | Turbo | IO Sched | Swappiness | NUMA Bal | THP | Cache Drop | IRQ Route |
|---|---|---|---|---|---|---|---|---|
| balanced | schedutil | off | mq-deadline | 60 | yes | madvise | no | no |
| performance | performance | on | none | 10 | no | madvise | no | no |
| ultra | performance | on | none | 5 | no | never | yes | CPUs 0–1 |

**Rollback safety**: Snapshot of all sysfs values is captured before any write.
On failure at any step, all previously written values are restored in reverse order.
`ultra` drop_caches failure is non-fatal (logs warning, continues).

**Cmdline profiles** (rpm-ostree kargs — reboot required):
- `balanced`: removes isolation args  
- `performance`: `rcu_nocbs=1-N rcu_nocb_poll`  
- `ultra`: `isolcpus=2-N nohz_full=2-N rcu_nocbs=2-N rcu_nocb_poll`

---

### 12.3 Automation Decision Logic

```
Every 30s:
  Read /var/lib/hisnos/threat-state.json
  Observe(score) → Holt's EMA update (α=0.3, β=0.2)
  Record(signals) → AnomalyCluster (60s window)
  Predict(threshold) → Prediction{projected, prob, trajectory, timeToCritical}

  IF suppressed OR inSafeMode → skip dispatch

  IF pred.AlertProbability ≥ 0.65 AND hotClusters exist:
    Dispatch(hotClusters) → match rule by pattern + minScore
    IF not on cooldown → submit IPC actions
    RecordIncident → persist to automation-state.json
    Emit "preemptive_action" security event

Adaptive threshold:
  On false positive: threshold += 2.5 (max 85.0)
  On confirmed alert: threshold -= 1.0 (min 50.0)
  Cooldown between adjustments: 2h
```

**Cluster patterns and pre-emptive rules:**

| Pattern | Trigger | Min Score | Actions | Cooldown |
|---|---|---|---|---|
| lateral_movement | namespace_abuse + privilege_escalation | 55 | reload_firewall, stop_lab | 3m |
| kernel_exploit | privilege_escalation + kernel_integrity | 60 | reload_firewall, lock_vault | 2m |
| exfil_prep | vault_exposure + firewall_anomaly | 50 | lock_vault, reload_firewall | 2m |
| persistence_rootkit | persistence_signal + kernel_integrity | 65 | reload_firewall | 5m |
| escalation | high-score + priv/firewall signals | 65 | reload_firewall | 5m |
| generic | any hot cluster | 75 | reload_firewall | 5m |

---

### 12.4 Ecosystem Update Flow

```
GET /api/update/status → get_update_status IPC
  → update_manager.Status()
  → rpm-ostree status --json
  → Returns: channel, current_commit, update_available,
             booted_health_score, rollback_confidence

POST /api/update/channel {"channel":"beta"}
  → set_update_channel IPC
  → channel_manager.Switch("beta")
  → ostree remote add hisnos-beta + rpm-ostree rebase hisnos-beta:branch
  → REBOOT REQUIRED

POST /api/update/rollback {"confirm":true}
  → trigger_rollback IPC
  → update_manager.Rollback(true)
  → rpm-ostree rollback
  → REBOOT REQUIRED

Rollback confidence scoring (0–100):
  +30  previous deployment age > 7 days
  +25  all required services running (hisnosd, nftables, auditd)
  +20  integrity-report.json status=pass
  -20  previous deployment is staged (not applied)
  -10  previous deployment age < 1 day
```

---

### 12.5 Phase 15 IPC Commands

**Performance (all gated by safe-mode except reads):**

| Command | Params | Safe-mode | Notes |
|---|---|---|---|
| `get_performance_profile` | — | ✅ | runtime + cmdline profile |
| `set_performance_profile` | `mode: string` | 🚫 blocked | immediate sysfs apply |
| `queue_cmdline_profile` | `mode: string` | 🚫 blocked | stages kargs for next reboot |

**Automation:**

| Command | Params | Safe-mode | Notes |
|---|---|---|---|
| `get_automation_status` | — | ✅ | full prediction + clusters |
| `override_automation` | `action: suppress\|reset\|mark_false_positive\|mark_confirmed` | ✅ | threshold learning |

**Ecosystem:**

| Command | Params | Safe-mode | Notes |
|---|---|---|---|
| `get_update_status` | — | ✅ | channel, health, deployments |
| `set_update_channel` | `channel: stable\|beta\|hardened` | 🚫 blocked | staged rebase |
| `trigger_rollback` | `confirm: true` | 🚫 blocked | rpm-ostree rollback |
| `get_module_registry` | — | ✅ | plugin listing |
| `register_module` | `id, name, version, sha256, install_path` | 🚫 blocked | register extension |
| `get_fleet_identity` | — | ✅ | anonymous fleet ID |

---

### 12.6 Failure Recovery — Phase 15

**Performance profile failure:**
```
Apply(profile) fails at subsystem N:
  Reverse all subsystems 0..N-1 using captured snapshot
  saveState("balanced") — record rollback in perf-state.json
  Emit "profile_apply_failed" security event
  hisnosd returns error to IPC caller
```

**Automation false positive surge:**
```
Operator sends: override_automation {"action":"suppress","duration_minutes":120}
  → OverrideCooldown set 2h ahead
  → All eval cycles skip dispatch for 2h
  → Threshold adjusted upward (+2.5) via MarkFalsePositive
```

**Update channel switch failure:**
```
If rpm-ostree rebase fails:
  New remote was already added (idempotent: --if-not-exists)
  Existing booted deployment unchanged (rebase only stages)
  Operator must investigate and retry or rollback
  No automatic recovery — operator intervention required
```

**Ecosystem manager init failure:**
```
If LoadFleetIdentity fails:
  main.go logs fatal + exits — fleet identity is required
  Operator must ensure /etc/machine-id exists and is readable
```

---

### 12.7 Architecture Decision Log — Phase 15

| Decision | Rationale |
|---|---|
| RegisterCommand pattern for IPC | Avoids import cycles between ipc and new packages; each subsystem registers itself |
| Holt's double EMA for prediction | Simple 2-variable smoothing — no ML library, deterministic, works with sparse data |
| 30s eval interval for automation | Short enough to react before a 10-min threat window expires; low enough CPU overhead |
| Alert probability linear (not sigmoid) | Avoids exp() dependency; linear is interpretable and easier to audit |
| anomaly cluster = single temporal bucket | Sufficient for correlated threat signals; multi-bucket DBSCAN adds complexity with marginal gain at this scale |
| Channel switch via rpm-ostree rebase | Atomic: new remote added before switch; booted deployment unchanged until reboot |
| Rollback confidence score | Operator needs guidance on rollback safety; deterministic scoring beats gut feel |
| Telemetry opt-in with conf file | Privacy by default; enterprise deployments can enable for fleet visibility |
| FleetID = SHA-256 of salt+machine-id | Non-reversible (no rainbow table attack on 128-bit hash); machine-id never transmitted |
| Performance rollback = sysfs re-write | sysfs writes have no undo log; snapshot-before-write + reverse-on-failure is best-effort but correct for runtime values |
| cmdline changes require reboot | Immutable Kinoite constraint; documented clearly; runtime changes cover 95% of gaming use cases without reboot |
