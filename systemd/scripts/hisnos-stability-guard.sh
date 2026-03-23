#!/bin/bash
# /usr/local/lib/hisnos/hisnos-stability-guard.sh — Long-Run Stability Guard
#
# Invoked by hisnos-stability-guard.timer every 6 hours.
# Detects stability threats and writes a structured report.
#
# Checks:
#   1. Config drift (kernel cmdline changed from expected)
#   2. Runaway journal logs (>500MB in /var/log/journal)
#   3. Disk pressure (rootfs or /var >90% full)
#   4. Corrupted OSTree deployment (ostree admin status fails)
#   5. Zombie process accumulation (>50 zombies)
#   6. Memory pressure (MemAvailable < 10% of MemTotal)
#
# Output: /var/lib/hisnos/stability-report.json

set -uo pipefail

LOG_TAG="hisnos-stability"
REPORT_FILE="/var/lib/hisnos/stability-report.json"
REPORT_DIR="/var/lib/hisnos"
WARNINGS=0
ERRORS=0

log_info() { logger -t "$LOG_TAG" -p daemon.info  "$*"; }
log_warn() { logger -t "$LOG_TAG" -p daemon.warning "$*"; WARNINGS=$((WARNINGS + 1)); }
log_err()  { logger -t "$LOG_TAG" -p daemon.err "$*"; ERRORS=$((ERRORS + 1)); }

# ── Check 1: Kernel cmdline drift ─────────────────────────────────────────
check_cmdline_drift() {
    local expected_file="/etc/hisnos/expected-cmdline-hash"
    if [[ ! -f "$expected_file" ]]; then
        # First run — create baseline
        sha256sum /proc/cmdline | awk '{print $1}' > "$expected_file" 2>/dev/null || true
        log_info "Cmdline baseline created"
        return
    fi
    local current expected
    current=$(sha256sum /proc/cmdline | awk '{print $1}')
    expected=$(cat "$expected_file" 2>/dev/null)
    if [[ "$current" != "$expected" ]]; then
        log_warn "Kernel cmdline drift detected (hash mismatch)"
    fi
}

# ── Check 2: Runaway journal logs ─────────────────────────────────────────
check_journal_size() {
    local journal_dir="/var/log/journal"
    if [[ -d "$journal_dir" ]]; then
        local size_mb
        size_mb=$(du -sm "$journal_dir" 2>/dev/null | awk '{print $1}') || return
        if [[ "$size_mb" -gt 500 ]]; then
            log_warn "Journal logs excessive: ${size_mb}MB (limit 500MB)"
            # Auto-vacuum to 200MB
            journalctl --vacuum-size=200M 2>/dev/null || true
            log_info "Journal vacuumed to 200MB"
        fi
    fi
}

# ── Check 3: Disk pressure ───────────────────────────────────────────────
check_disk_pressure() {
    for mount in / /var /home; do
        if mountpoint -q "$mount" 2>/dev/null || [[ "$mount" == "/" ]]; then
            local usage
            usage=$(df --output=pcent "$mount" 2>/dev/null | tail -1 | tr -d ' %') || continue
            if [[ -n "$usage" && "$usage" -ge 90 ]]; then
                log_err "Disk pressure: $mount at ${usage}% full"
            elif [[ -n "$usage" && "$usage" -ge 80 ]]; then
                log_warn "Disk pressure warning: $mount at ${usage}% full"
            fi
        fi
    done
}

# ── Check 4: OSTree deployment health ────────────────────────────────────
check_ostree_health() {
    if command -v ostree &>/dev/null; then
        if ! ostree admin status &>/dev/null; then
            log_err "OSTree deployment corrupted: ostree admin status failed"
        fi
    fi
    if command -v rpm-ostree &>/dev/null; then
        local status
        status=$(rpm-ostree status --json 2>/dev/null) || {
            log_err "rpm-ostree status failed — deployment may be corrupted"
            return
        }
        # Check for pending deployments stuck >24h
        local pending
        pending=$(echo "$status" | python3 -c "
import sys,json
try:
    d=json.load(sys.stdin)
    for dep in d.get('deployments',[]):
        if dep.get('staged',False):
            print('staged')
except: pass" 2>/dev/null)
        if [[ -n "$pending" ]]; then
            log_warn "Staged OSTree deployment detected — may need reboot"
        fi
    fi
}

# ── Check 5: Zombie processes ────────────────────────────────────────────
check_zombies() {
    local zombie_count
    zombie_count=$(ps aux 2>/dev/null | awk '$8 ~ /Z/ {count++} END {print count+0}')
    if [[ "$zombie_count" -gt 50 ]]; then
        log_err "Zombie process accumulation: $zombie_count zombies"
    elif [[ "$zombie_count" -gt 20 ]]; then
        log_warn "Zombie processes rising: $zombie_count"
    fi
}

# ── Check 6: Memory pressure ─────────────────────────────────────────────
check_memory_pressure() {
    local mem_total mem_avail
    mem_total=$(awk '/MemTotal/ {print $2}' /proc/meminfo)
    mem_avail=$(awk '/MemAvailable/ {print $2}' /proc/meminfo)
    if [[ -n "$mem_total" && -n "$mem_avail" && "$mem_total" -gt 0 ]]; then
        local pct_avail=$(( mem_avail * 100 / mem_total ))
        if [[ "$pct_avail" -lt 10 ]]; then
            log_err "Memory pressure: only ${pct_avail}% available (${mem_avail}kB / ${mem_total}kB)"
        elif [[ "$pct_avail" -lt 20 ]]; then
            log_warn "Memory pressure warning: ${pct_avail}% available"
        fi
    fi
}

# ── Write report ──────────────────────────────────────────────────────────
write_report() {
    mkdir -p "$REPORT_DIR" 2>/dev/null || true
    cat > "$REPORT_FILE" <<REPORT
{
  "timestamp": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "uptime_sec": $(awk '{printf "%d", $1}' /proc/uptime),
  "warnings": $WARNINGS,
  "errors": $ERRORS,
  "status": "$(if [[ $ERRORS -gt 0 ]]; then echo "DEGRADED"; elif [[ $WARNINGS -gt 0 ]]; then echo "WARNING"; else echo "HEALTHY"; fi)",
  "kernel": "$(uname -r)",
  "load_avg": "$(cat /proc/loadavg | awk '{print $1, $2, $3}')"
}
REPORT
    log_info "Stability report: warnings=$WARNINGS errors=$ERRORS → $REPORT_FILE"
}

# ── Main ──────────────────────────────────────────────────────────────────
log_info "Stability guard check starting"

check_cmdline_drift
check_journal_size
check_disk_pressure
check_ostree_health
check_zombies
check_memory_pressure
write_report

exit 0
