# HisnOS Recovery Guide

**Use this guide when something is broken or stuck.**
All recovery operations are idempotent — safe to run multiple times.

---

## Quick Reference

```bash
# Diagnose: print all subsystem states
hisnos-recover.sh status

# Get targeted recovery steps for current state
hisnos-recover.sh rollback-guide
```

---

## 1. Vault Recovery

### Vault Won't Unlock (Passphrase Forgotten)

HisnOS cannot recover a forgotten passphrase. The vault cipher is AES-256-GCM with no backdoor.

**Options:**
- Restore vault directory from a backup made before the passphrase was lost.
- Accept data loss and re-initialize: `"${VAULT_SH}" init` (creates a new empty vault).

### Vault Stuck Mounted (Cannot Lock)

```bash
# Option 1: Via recovery CLI (recommended)
hisnos-recover.sh vault-force-lock

# Option 2: Manual fusermount3
VAULT_MOUNT="${HOME}/.local/share/hisnos/vault/mount"
fusermount3 -uz "${VAULT_MOUNT}"

# Option 3: Kill watcher then unmount
systemctl --user stop hisnos-vault-watcher.service
systemctl --user stop hisnos-vault-idle.timer
fusermount3 -uz "${HOME}/.local/share/hisnos/vault/mount"
```

### Vault Watcher Not Starting

```bash
# Check why it failed
systemctl --user status hisnos-vault-watcher.service
journalctl --user -u hisnos-vault-watcher.service -n 50

# Restart
systemctl --user restart hisnos-vault-watcher.service

# Verify gdbus is available
which gdbus
```

### Vault Idle Timer Not Working

```bash
# Check timer state
systemctl --user list-timers hisnos-vault-idle.timer

# Reload and restart
systemctl --user daemon-reload
systemctl --user restart hisnos-vault-idle.timer
```

---

## 2. Firewall Recovery

### All Egress Blocked (Connectivity Lost)

This means the nftables default-deny is in effect with no allow rules loaded.

```bash
# Check current ruleset
sudo nft list ruleset

# Reload HisnOS rules from disk
sudo systemctl restart nftables.service

# If that fails, emergency flush (removes ALL rules — allows all traffic)
hisnos-recover.sh firewall-flush --confirm
# Then restore:
sudo systemctl restart nftables.service
```

### nftables Service Failed

```bash
# View the error
sudo systemctl status nftables.service
sudo journalctl -u nftables.service -n 50

# Check for syntax errors in rule files
sudo nft -c -f /etc/nftables.conf

# Common issue: gaming chain left in bad state
sudo nft flush chain inet hisnos gaming_output 2>/dev/null || true
sudo nft flush chain inet hisnos gaming_input 2>/dev/null || true
sudo systemctl restart nftables.service
```

### Gaming Chain Stuck Loaded

```bash
# Reset gaming mode (flushes chain, restores CPU/IRQ, re-enables vault timer)
hisnos-recover.sh gaming-reset

# Manual fallback
sudo nft flush chain inet hisnos gaming_output 2>/dev/null || true
sudo nft flush chain inet hisnos gaming_input 2>/dev/null || true
```

---

## 3. Lab Recovery

### Lab Session Won't Stop

```bash
# Graceful stop attempt
"${HOME}/.local/share/hisnos/lab/runtime/hisnos-lab-runtime.sh" stop

# Emergency stop (kills bwrap processes, cleans veth pairs)
hisnos-recover.sh lab-emergency-stop

# Manual veth cleanup (if orphaned interfaces remain)
ip link show | grep -E "vlh-|vlc-"
# For each orphaned interface:
sudo ip link delete vlh-XXXX 2>/dev/null || true
sudo ip link delete vlc-XXXX 2>/dev/null || true
```

### Lab Networking Broken (veth Orphans)

```bash
# List orphaned veth pairs
ip link show | grep -E "vlh-|vlc-"

# Remove all HisnOS lab veth pairs
for iface in $(ip link show | grep -oP '(?<=\d: )vlh-\S+|vlc-\S+'); do
    sudo ip link delete "${iface%:}" 2>/dev/null && echo "Removed ${iface%:}"
done

# Restart lab-netd socket
sudo systemctl restart hisnos-lab-netd.socket
```

### hisnos-lab-netd Socket Broken

```bash
sudo systemctl status hisnos-lab-netd.socket
sudo systemctl status hisnos-lab-netd@*.service

# Reset
sudo systemctl stop hisnos-lab-netd@*.service 2>/dev/null || true
sudo systemctl restart hisnos-lab-netd.socket
```

---

## 4. Gaming Mode Recovery

### Gaming Stuck (CPU/IRQ Not Restored)

```bash
# Recovery CLI (recommended)
hisnos-recover.sh gaming-reset

# Manual CPU governor restore
for cpu in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do
    echo "powersave" | sudo tee "${cpu}"
done

# Remove stale lock file
rm -f "${XDG_RUNTIME_DIR}/hisnos-gaming.lock"

# Re-enable vault idle timer
systemctl --user start hisnos-vault-idle.timer
```

### Gaming Tuning Service Failed to Stop

```bash
# Check the service
sudo systemctl status hisnos-gaming-tuned-start.service

# Force-stop tuning (this triggers ExecStopPost= rollback)
sudo systemctl stop hisnos-gaming-tuned-start.service

# If ExecStopPost= also failed, run stop manually
sudo /etc/hisnos/gaming/hisnos-gaming-tuned.sh stop
```

---

## 5. Dashboard Recovery

### Dashboard Not Responding

```bash
# Check socket + service
systemctl --user status hisnos-dashboard.socket
systemctl --user status hisnos-dashboard.service

# Restart
systemctl --user restart hisnos-dashboard.socket

# Check logs
journalctl --user -u hisnos-dashboard.service -n 50
```

### Dashboard in Bad State (Safe Mode)

```bash
hisnos-recover.sh dashboard-safe-mode
```

This writes a drop-in config that disables non-essential endpoints and restarts the service.

### Dashboard Confirm Token Lost

The confirm token is generated fresh on each daemon start. If you lose it:

```bash
# Restart dashboard (generates new token)
systemctl --user restart hisnos-dashboard.service

# Fetch new token
curl -s http://localhost:9443/api/confirm/token
```

---

## 6. Audit Pipeline Recovery

### hisnos-logd Not Writing to Audit Log

```bash
# Check service
systemctl --user status hisnos-logd.service
journalctl --user -u hisnos-logd.service -n 50

# Verify audit directory exists and is writable
ls -la /var/lib/hisnos/audit/
stat /var/lib/hisnos/audit/current.jsonl

# Restart logd
systemctl --user restart hisnos-logd.service
```

### Audit Log Rotation Stuck (File >50MB)

```bash
# Check size
du -sh /var/lib/hisnos/audit/current.jsonl

# Manually trigger rotation (rename file; logd creates new current.jsonl)
TS=$(date +%Y%m%d-%H%M%S)
sudo mv /var/lib/hisnos/audit/current.jsonl \
    "/var/lib/hisnos/audit/hisnos-audit-${TS}.jsonl"
gzip "/var/lib/hisnos/audit/hisnos-audit-${TS}.jsonl" &

# Restart logd to open new file
systemctl --user restart hisnos-logd.service
```

### Threat Daemon (threatd) Not Updating

```bash
# Check service
systemctl --user status hisnos-threatd.service
journalctl --user -u hisnos-threatd.service -n 50

# Verify threat state file
cat /var/lib/hisnos/threat-state.json | python3 -m json.tool

# Restart
systemctl --user restart hisnos-threatd.service
```

---

## 7. System Update Recovery

### Update Applied but System Won't Boot

At GRUB prompt (hold Shift at boot), select the previous deployment entry.

After booting into the old deployment:

```bash
# Pin the working deployment to prevent garbage collection
sudo ostree admin pin 0

# Remove the bad deployment
sudo rpm-ostree cleanup -p
```

### Rollback Failed

```bash
# List available deployments
rpm-ostree status

# Pin current working deployment
sudo ostree admin pin 0

# Manual rollback to specific checksum
sudo rpm-ostree deploy <checksum>
systemctl reboot
```

### Preflight Blocking Update (False Positive)

```bash
# Run preflight with verbose output to see which check is failing
"${HOME}/.local/share/hisnos/update/hisnos-update-preflight.sh" 2>&1

# Common causes:
# - Vault is mounted → lock vault first: "${VAULT_SH}" lock
# - Active lab session → stop lab: hisnos-lab-runtime.sh stop
# - Low disk space → clean up: rpm-ostree cleanup -b
```

---

## 8. Full System Recovery Sequence

Use this if multiple subsystems are broken simultaneously:

```bash
# Step 1: Get status overview
hisnos-recover.sh status

# Step 2: Stop lab (safest first)
hisnos-recover.sh lab-emergency-stop 2>/dev/null || true

# Step 3: Reset gaming
hisnos-recover.sh gaming-reset 2>/dev/null || true

# Step 4: Force-lock vault
hisnos-recover.sh vault-force-lock 2>/dev/null || true

# Step 5: Restore firewall
sudo systemctl restart nftables.service

# Step 6: Restart audit pipeline
systemctl --user restart hisnos-logd.service
systemctl --user restart hisnos-threatd.service

# Step 7: Restart dashboard
systemctl --user restart hisnos-dashboard.socket

# Step 8: Verify
hisnos-recover.sh status
```

---

## 9. Useful Diagnostic Commands

```bash
# All HisnOS user services
systemctl --user list-units 'hisnos-*' --all

# All HisnOS system services
systemctl list-units 'hisnos-*' --all

# Recent audit events from journal
journalctl -t hisnos-vault -t hisnos-logd -t hisnos-lab-runtime -n 100

# nftables current state
sudo nft list ruleset

# Active cgroup limits for hisnos services
systemd-cgls | grep hisnos

# Threat state snapshot
cat /var/lib/hisnos/threat-state.json

# Orphaned bwrap processes
pgrep -a bwrap

# Orphaned lab veth interfaces
ip link show | grep -E "vlh-|vlc-"
```
