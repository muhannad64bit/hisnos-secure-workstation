# HisnOS Operator Guide

**Audience:** Primary operator (single-user workstation)
**OS:** Fedora Kinoite (immutable, rpm-ostree)
**Version:** Phase 8 / Production

---

## Table of Contents

1. [Daily Operations](#1-daily-operations)
2. [Vault Management](#2-vault-management)
3. [Lab Mode](#3-lab-mode)
4. [Firewall & Egress Control](#4-firewall--egress-control)
5. [Gaming Mode](#5-gaming-mode)
6. [System Updates](#6-system-updates)
7. [Dashboard](#7-dashboard)
8. [Threat Monitoring](#8-threat-monitoring)
9. [Audit Logs](#9-audit-logs)
10. [Recovery Procedures](#10-recovery-procedures)
11. [Stress Testing](#11-stress-testing)

---

## 1. Daily Operations

### Morning Checklist

```bash
# 1. Check overall system risk score
cat /var/lib/hisnos/threat-state.json | python3 -c \
  "import json,sys; d=json.load(sys.stdin); print(f'Risk: {d[\"risk_score\"]} ({d[\"risk_level\"]})')"

# 2. Verify firewall is active
systemctl is-active nftables.service

# 3. Verify audit pipeline is running
systemctl --user is-active hisnos-logd.service

# 4. Check dashboard health
curl -s http://localhost:9443/api/health
```

### Service Status Overview

| Service | Scope | Check Command |
|---|---|---|
| nftables | system | `systemctl is-active nftables.service` |
| auditd | system | `systemctl is-active auditd.service` |
| hisnos-logd | user | `systemctl --user is-active hisnos-logd.service` |
| hisnos-threatd | user | `systemctl --user is-active hisnos-threatd.service` |
| hisnos-dashboard | user | `systemctl --user is-active hisnos-dashboard.service` |
| hisnos-vault-watcher | user | `systemctl --user is-active hisnos-vault-watcher.service` |
| hisnos-vault-idle.timer | user | `systemctl --user is-active hisnos-vault-idle.timer` |

---

## 2. Vault Management

### Vault CLI

```bash
VAULT_SH="${HOME}/.local/share/hisnos/vault/hisnos-vault.sh"

# Initialize (first time only — prompts for passphrase)
"${VAULT_SH}" init

# Mount vault
"${VAULT_SH}" mount

# Check status
"${VAULT_SH}" check

# Manually lock vault
"${VAULT_SH}" lock

# Rotate encryption key (requires vault to be mounted)
"${VAULT_SH}" rotate-key
```

### Auto-lock Behavior

The vault auto-locks via two independent mechanisms:

1. **Idle timer** (`hisnos-vault-idle.timer`): locks after configurable idle period.
2. **Screen lock watcher** (`hisnos-vault-watcher.service`): locks immediately when KDE screen lock activates.

To temporarily suppress auto-lock (e.g., during a long process):

```bash
# Suppress idle timer (vault-watcher still active)
systemctl --user stop hisnos-vault-idle.timer

# Re-enable after work
systemctl --user start hisnos-vault-idle.timer
```

### Vault and Threat Score

The threat engine fires `vault_exposure` (+15 risk points) when:
- The vault has been continuously mounted for > 30 minutes.

This is informational — it does NOT auto-lock the vault.

---

## 3. Lab Mode

### Starting a Lab Session

```bash
# Light lab (Distrobox/Kali) — user-mode networking
"${HOME}/.local/share/hisnos/lab/runtime/hisnos-lab-runtime.sh" start kali

# Full lab (isolated veth networking via netd)
"${HOME}/.local/share/hisnos/lab/runtime/hisnos-lab-runtime.sh" start kali --network isolated

# Or via dashboard
curl -s -X POST http://localhost:9443/api/lab/start
```

### Network Profiles

| Profile | Isolation Level | Use Case |
|---|---|---|
| `none` | Host network | Low-risk tools |
| `isolated` | veth + nftables | Malware analysis, untrusted code |

```bash
# Get current profile
curl -s http://localhost:9443/api/lab/network-profile

# Set profile (requires confirm token)
TOKEN=$(curl -s http://localhost:9443/api/confirm/token | python3 -c "import json,sys; print(json.load(sys.stdin)['token'])")
curl -s -X POST -H "X-HisnOS-Confirm: ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"profile":"isolated"}' \
  http://localhost:9443/api/lab/network-profile
```

### Stopping Lab

```bash
"${HOME}/.local/share/hisnos/lab/runtime/hisnos-lab-runtime.sh" stop

# Emergency stop (kills all bwrap, cleans veth)
hisnos-recover.sh lab-emergency-stop
```

---

## 4. Firewall & Egress Control

### Architecture

- **Default deny** egress: all outbound blocked unless explicitly allowed.
- Allowlists in `/etc/nftables/hisnos-*.nft`.
- OpenSnitch provides per-application rules (GUI in system tray).

### Reloading Rules

```bash
# Via systemctl (requires sudo)
sudo systemctl reload nftables.service

# Via dashboard (requires confirm token)
TOKEN=$(curl -s http://localhost:9443/api/confirm/token | python3 -c "import json,sys; print(json.load(sys.stdin)['token'])")
curl -s -X POST -H "X-HisnOS-Confirm: ${TOKEN}" http://localhost:9443/api/firewall/reload
```

### Viewing Current Rules

```bash
sudo nft list ruleset

# Check what is allowed for a specific IP
sudo nft list table inet hisnos
```

### Emergency Firewall Flush

**Use only in a connectivity emergency.** Removes all HisnOS firewall rules.

```bash
hisnos-recover.sh firewall-flush --confirm
```

After flushing, restore rules:

```bash
sudo systemctl restart nftables.service
```

---

## 5. Gaming Mode

### Starting Gaming Mode

```bash
# Via gaming CLI
"${HOME}/.local/share/hisnos/gaming/hisnos-gaming.sh" start

# Or via dashboard
TOKEN=$(curl -s http://localhost:9443/api/confirm/token | python3 -c "import json,sys; print(json.load(sys.stdin)['token'])")
curl -s -X POST -H "X-HisnOS-Confirm: ${TOKEN}" http://localhost:9443/api/gaming/start
```

**Effects of gaming mode start:**
- CPU governor set to `performance` on all cores.
- IRQ affinity tuned to non-gaming cores (cores 2–7 by default).
- nftables gaming chain loaded (gaming-optimized egress rules).
- Vault idle timer suppressed (vault stays mounted during session).
- System mode transitioned to `gaming` in dashboard.

### Stopping Gaming Mode

```bash
"${HOME}/.local/share/hisnos/gaming/hisnos-gaming.sh" stop
```

All tuning is rolled back: CPU governors restored, IRQ affinity restored, gaming nft chain flushed, vault idle timer re-enabled.

### GameMode Integration

Games launched via `gamemoderun` or the Steam GameMode launch option automatically call the HisnOS gaming script. See `gaming/config/gamemode.ini`.

### Checking Gaming Status

```bash
"${HOME}/.local/share/hisnos/gaming/hisnos-gaming.sh" status

# Via dashboard
curl -s http://localhost:9443/api/gaming/status
```

### Emergency Gaming Reset

If gaming mode is stuck (e.g., script crashed):

```bash
hisnos-recover.sh gaming-reset
```

---

## 6. System Updates

### Pre-flight Check

Always run before applying an update:

```bash
"${HOME}/.local/share/hisnos/update/hisnos-update-preflight.sh"
```

Checks: disk space, vault state, active lab sessions, pending deployments.

### Applying an Update

```bash
# Check for available updates (non-destructive)
TOKEN=$(curl -s http://localhost:9443/api/confirm/token | python3 -c "import json,sys; print(json.load(sys.stdin)['token'])")
curl -s -X POST -H "X-HisnOS-Confirm: ${TOKEN}" http://localhost:9443/api/update/check

# Apply (streams progress via SSE)
curl -sN -X POST -H "X-HisnOS-Confirm: ${TOKEN}" http://localhost:9443/api/update/prepare
curl -s -X POST -H "X-HisnOS-Confirm: ${TOKEN}" http://localhost:9443/api/update/apply
```

After applying, **reboot** to boot into the new deployment:

```bash
systemctl reboot
```

### Rolling Back

If the new deployment is broken:

```bash
# At runtime (before reboot)
rpm-ostree rollback

# Via dashboard
curl -s -X POST -H "X-HisnOS-Confirm: ${TOKEN}" http://localhost:9443/api/update/rollback

# At boot: in GRUB, select the previous entry
```

---

## 7. Dashboard

### Access

The dashboard runs on `http://localhost:9443` (socket-activated, starts on first connection).

```bash
# Check backend health
curl -s http://localhost:9443/api/health

# Force-start (normally not needed)
systemctl --user start hisnos-dashboard.socket
```

### Confirm Token Pattern

Destructive actions require the session token:

```bash
TOKEN=$(curl -s http://localhost:9443/api/confirm/token | \
  python3 -c "import json,sys; print(json.load(sys.stdin)['token'])")
```

The token is valid for the lifetime of the current dashboard process (refreshes on restart).

### Safe Mode

If the dashboard is misbehaving:

```bash
hisnos-recover.sh dashboard-safe-mode
```

This restarts the dashboard with a minimal configuration override.

---

## 8. Threat Monitoring

### Reading Threat State

```bash
# Full state
cat /var/lib/hisnos/threat-state.json | python3 -m json.tool

# Risk score only
cat /var/lib/hisnos/threat-state.json | python3 -c \
  "import json,sys; d=json.load(sys.stdin); print(d['risk_score'], d['risk_level'])"

# Active signals
cat /var/lib/hisnos/threat-state.json | python3 -c \
  "import json,sys; d=json.load(sys.stdin); \
   [print(k,v) for k,v in d.get('signals',{}).items() if v]"
```

### Risk Levels

| Score | Level | Action |
|---|---|---|
| 0–20 | low | Normal operations |
| 21–50 | medium | Review active signals, investigate if unexpected |
| 51–80 | high | Investigate immediately; consider locking vault |
| 81–100 | critical | Lock vault, stop lab, review audit logs |

### Signal Reference

| Signal | Points | Trigger |
|---|---|---|
| `lab_session_active` | +15 | Lab session is currently running |
| `ns_burst` | +20 | >3 namespace operations in 5 minutes |
| `fw_block_rate` | +15 | >10 firewall blocks in 5 minutes |
| `nft_modified` | +25 | nftables ruleset change detected in audit log |
| `vault_exposure` | +15 | Vault mounted continuously >30 minutes |
| `priv_exec_burst` | +10 | >3 privilege escalations in 5 minutes |

### Timeline

```bash
# Last 10 threat timeline entries
tail -n 10 /var/lib/hisnos/threat-timeline.jsonl | python3 -c \
  "import json,sys; [print(json.dumps(json.loads(l), indent=2)) for l in sys.stdin if l.strip()]"
```

---

## 9. Audit Logs

### Location

| File | Contents |
|---|---|
| `/var/lib/hisnos/audit/current.jsonl` | Active log (rolling, max 50MB) |
| `/var/lib/hisnos/audit/hisnos-audit-*.jsonl.gz` | Compressed segments (14-day retention) |

### Querying

```bash
# Recent events
tail -n 100 /var/lib/hisnos/audit/current.jsonl | python3 -m json.tool

# Firewall events
grep '"subsystem":"firewall"' /var/lib/hisnos/audit/current.jsonl | tail -20

# Vault events
grep '"subsystem":"vault"' /var/lib/hisnos/audit/current.jsonl | tail -20

# Lab events
grep '"subsystem":"lab"' /var/lib/hisnos/audit/current.jsonl | tail -20
```

### Via Dashboard API

```bash
curl -s http://localhost:9443/api/audit/summary
curl -s http://localhost:9443/api/audit/sessions
curl -s http://localhost:9443/api/audit/firewall-events
```

---

## 10. Recovery Procedures

### Recovery CLI

```bash
# Overview of all subsystem states
hisnos-recover.sh status

# Targeted rollback guidance based on current state
hisnos-recover.sh rollback-guide

# Force-lock vault (if watcher is stuck)
hisnos-recover.sh vault-force-lock

# Emergency firewall flush (destructive — requires --confirm)
hisnos-recover.sh firewall-flush --confirm

# Reset stuck gaming mode
hisnos-recover.sh gaming-reset

# Kill lab + clean veth pairs
hisnos-recover.sh lab-emergency-stop

# Put dashboard in safe mode
hisnos-recover.sh dashboard-safe-mode
```

See `docs/RECOVERY-GUIDE.md` for detailed procedures.

---

## 11. Stress Testing

### Running All Tests

```bash
cd tests/stress
./run-all.sh 2>&1 | tee /tmp/hisnos-stress-results.json
```

### Running a Single Suite

```bash
./run-all.sh --suite firewall
./run-all.sh --suite vault
./run-all.sh --suite audit
./run-all.sh --suite lab-ns
./run-all.sh --suite update
./run-all.sh --suite suspend
```

### JSON-Only Output (for CI)

```bash
./run-all.sh --json-only > /tmp/results.json
echo "Exit code: $?"
```

### Test Suites

| Suite | Script | Tests |
|---|---|---|
| firewall | stress-firewall.sh | blocked egress, rule reload latency, gaming chain toggle, conntrack stability |
| lab-ns | stress-lab-ns.sh | namespace creation rate, bwrap launch cycle, ns threat signal, veth cleanup |
| vault | stress-vault.sh | vault status, exposure signal, lock latency, idle timer, watcher |
| audit | stress-audit.sh | logd running, write rate flood, rotation state, auditd rules, JSON integrity |
| update | stress-update.sh | preflight check, update status API, ostree state, rollback availability, script lint |
| suspend | stress-suspend.sh | pre-suspend state, inhibitors, post-resume recovery, vault watcher, network state |
