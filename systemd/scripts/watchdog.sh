#!/bin/bash
# /usr/local/lib/hisnos/watchdog.sh — HisnOS Unified Subsystem Watchdog
#
# Monitors for hung/runaway processes and kills them.
# Runs as hisnos-watchdog.service (Type=simple, CPUSchedulingPolicy=batch).
#
# Capabilities:
#   1. Kill hung services >60s CPU lock
#   2. Detect restart loops (>3 restarts in 2 minutes)
#   3. Detect memory leak growth rate (>90% cgroup limit)
#   4. Disable failing subsystem automatically
#   5. Trigger safe-mode escalation after 3 total subsystem failures
#
# Additional monitors:
#   - Hung gaming mode (>2h → kill)
#   - Stuck update transactions (>30min → kill)
#   - Stuck installer (>2h → kill)
#   - Runaway AI/automation CPU (>80% for 5min → kill)
#
# All actions are logged to journal + /var/log/hisnos/watchdog.log.

set -uo pipefail
shopt -s nullglob

# ── Configuration ─────────────────────────────────────────────────────────
GAMING_MAX_SEC=$((2 * 3600))        # 2 hours
UPDATE_MAX_SEC=$((30 * 60))         # 30 minutes
INSTALLER_MAX_SEC=$((2 * 3600))     # 2 hours
CPU_THRESHOLD=80                    # percent
CPU_VIOLATION_MAX=60                # 60 × 5s = 5 minutes sustained
CPU_LOCK_SEC=60                     # kill service stuck >60s at 100% CPU
MEM_PRESSURE_PCT=90                 # percent of MemoryMax
RESTART_LOOP_WINDOW=120             # 2 minutes
RESTART_LOOP_THRESH=3               # 3 restarts in window = loop
SAFE_MODE_FAIL_THRESH=3             # 3 subsystem failures → safe mode
POLL_INTERVAL=5                     # seconds
HISNOS_RUN=/run/hisnos
LOG_TAG="hisnos-watchdog"

# ── State ─────────────────────────────────────────────────────────────────
declare -A cpu_violations           # pid → consecutive violation count
declare -A cpu_lock_start           # service → first 100% CPU timestamp
declare -A restart_history          # service → comma-separated timestamps
TOTAL_FAILURES=0                    # cumulative subsystem failure count
SAFE_MODE_TRIGGERED=false

# ── Monitored services ───────────────────────────────────────────────────
MONITORED_SERVICES=(
    "hisnos-automation.service"
    "hisnos-threat-engine.service"
    "hisnos-performance-guard.service"
    "hisnos-hispowerd.service"
    "hisnos-irq-balancer.service"
    "hisnos-thermal.service"
    "hisnos-rt-guard.service"
)

# ── Helpers ───────────────────────────────────────────────────────────────
log_info()  { echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] INFO:  $*"; logger -t "$LOG_TAG" -p daemon.info  "$*"; }
log_warn()  { echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] WARN:  $*"; logger -t "$LOG_TAG" -p daemon.warning "$*"; }
log_crit()  { echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] CRIT:  $*"; logger -t "$LOG_TAG" -p daemon.crit "$*"; }

safe_kill() {
    local pid=$1 name=$2 reason=$3
    if kill -0 "$pid" 2>/dev/null; then
        log_crit "Killing $name (PID $pid): $reason"
        kill -TERM "$pid" 2>/dev/null || true
        sleep 5
        if kill -0 "$pid" 2>/dev/null; then
            log_crit "SIGTERM failed for $name (PID $pid), sending SIGKILL"
            kill -KILL "$pid" 2>/dev/null || true
        fi
    fi
}

service_stop() {
    local svc=$1 reason=$2
    log_crit "Stopping service $svc: $reason"
    systemctl stop "$svc" 2>/dev/null || true
}

service_disable() {
    local svc=$1 reason=$2
    log_crit "DISABLING service $svc: $reason"
    systemctl stop "$svc" 2>/dev/null || true
    systemctl mask --runtime "$svc" 2>/dev/null || true
    TOTAL_FAILURES=$((TOTAL_FAILURES + 1))
    log_warn "Total subsystem failures: $TOTAL_FAILURES / $SAFE_MODE_FAIL_THRESH"
}

trigger_safe_mode() {
    local reason=$1
    if [[ "$SAFE_MODE_TRIGGERED" == "true" ]]; then
        return
    fi
    SAFE_MODE_TRIGGERED=true
    log_crit "=== SAFE MODE ESCALATION ==="
    log_crit "Reason: $reason"
    mkdir -p "$HISNOS_RUN"
    echo "{\"reason\": \"$reason\", \"timestamp\": \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\", \"source\": \"watchdog\", \"failures\": $TOTAL_FAILURES}" \
        > "$HISNOS_RUN/safemode-reason.json"
    touch "$HISNOS_RUN/safemode-active"
    systemctl isolate hisnos-safe.target 2>/dev/null || true
}

# ── Service runtime check ─────────────────────────────────────────────────
service_elapsed_sec() {
    local svc=$1
    local ts
    ts=$(systemctl show -p ActiveEnterTimestampMonotonic --value "$svc" 2>/dev/null) || echo "-1"
    if [[ -z "$ts" || "$ts" == "0" ]]; then
        echo "-1"
        return
    fi
    local now_ts
    now_ts=$(awk '{printf "%d", $1 * 1000000}' /proc/uptime)
    echo $(( (now_ts - ts) / 1000000 ))
}

# ── Check: Restart loop detection ─────────────────────────────────────────
check_restart_loops() {
    local now
    now=$(date +%s)
    for svc in "${MONITORED_SERVICES[@]}"; do
        local n_restarts
        n_restarts=$(systemctl show -p NRestarts --value "$svc" 2>/dev/null || echo "0")
        [[ -z "$n_restarts" ]] && n_restarts=0

        # Track restart events
        local key="$svc"
        local history="${restart_history[$key]:-}"
        local prev_count
        prev_count=$(echo "$history" | awk -F: '{print $1}')
        [[ -z "$prev_count" ]] && prev_count=0

        if [[ "$n_restarts" -gt "$prev_count" ]]; then
            # New restart detected — record timestamp
            local timestamps
            timestamps=$(echo "$history" | cut -d: -f2-)
            timestamps="${timestamps:+$timestamps,}$now"
            restart_history[$key]="$n_restarts:$timestamps"

            # Count restarts within window
            local count=0
            IFS=',' read -ra ts_arr <<< "$timestamps"
            for ts in "${ts_arr[@]}"; do
                [[ -z "$ts" ]] && continue
                if [[ $((now - ts)) -le $RESTART_LOOP_WINDOW ]]; then
                    count=$((count + 1))
                fi
            done

            if [[ "$count" -ge "$RESTART_LOOP_THRESH" ]]; then
                service_disable "$svc" \
                    "Restart loop detected: $count restarts in ${RESTART_LOOP_WINDOW}s"
            fi
        else
            restart_history[$key]="$n_restarts:"
        fi
    done

    # Check if total failures warrant safe mode
    if [[ "$TOTAL_FAILURES" -ge "$SAFE_MODE_FAIL_THRESH" ]]; then
        trigger_safe_mode "Watchdog: $TOTAL_FAILURES subsystem failures exceeded threshold"
    fi
}

# ── Check: Hung services (CPU lock >60s) ──────────────────────────────────
check_cpu_lock() {
    local now
    now=$(date +%s)
    for svc in "${MONITORED_SERVICES[@]}"; do
        if ! systemctl is-active --quiet "$svc" 2>/dev/null; then
            unset "cpu_lock_start[$svc]" 2>/dev/null || true
            continue
        fi

        local pid
        pid=$(systemctl show -p MainPID --value "$svc" 2>/dev/null || echo "0")
        [[ "$pid" == "0" || -z "$pid" ]] && continue

        local cpu
        cpu=$(ps -o %cpu= -p "$pid" 2>/dev/null | tr -d ' ' | cut -d. -f1) || continue
        [[ -z "$cpu" ]] && continue

        if [[ "$cpu" -ge 95 ]]; then
            if [[ -z "${cpu_lock_start[$svc]:-}" ]]; then
                cpu_lock_start[$svc]=$now
            elif [[ $((now - ${cpu_lock_start[$svc]})) -ge $CPU_LOCK_SEC ]]; then
                service_disable "$svc" \
                    "CPU locked at ${cpu}% for >${CPU_LOCK_SEC}s"
                unset "cpu_lock_start[$svc]"
            fi
        else
            unset "cpu_lock_start[$svc]" 2>/dev/null || true
        fi
    done
}

# ── Check: Hung gaming mode ───────────────────────────────────────────────
check_gaming() {
    if ! systemctl is-active --quiet hisnos-gaming.service 2>/dev/null; then
        return
    fi
    local elapsed
    elapsed=$(service_elapsed_sec "hisnos-gaming.service")
    if [[ "$elapsed" -gt "$GAMING_MAX_SEC" ]]; then
        service_stop "hisnos-gaming.service" \
            "Gaming mode running for ${elapsed}s (limit ${GAMING_MAX_SEC}s)"
    fi
}

# ── Check: Stuck update transaction ───────────────────────────────────────
check_update() {
    local rpm_pid
    rpm_pid=$(pgrep -x rpm-ostree 2>/dev/null || true)
    if [[ -n "$rpm_pid" ]]; then
        local elapsed
        elapsed=$(ps -o etimes= -p "$rpm_pid" 2>/dev/null | tr -d ' ') || return
        if [[ -n "$elapsed" && "$elapsed" -gt "$UPDATE_MAX_SEC" ]]; then
            safe_kill "$rpm_pid" "rpm-ostree" \
                "Update transaction stuck for ${elapsed}s (limit ${UPDATE_MAX_SEC}s)"
        fi
    fi
}

# ── Check: Stuck installer ───────────────────────────────────────────────
check_installer() {
    if ! systemctl is-active --quiet hisnos-installer.service 2>/dev/null; then
        return
    fi
    local elapsed
    elapsed=$(service_elapsed_sec "hisnos-installer.service")
    if [[ "$elapsed" -gt "$INSTALLER_MAX_SEC" ]]; then
        service_stop "hisnos-installer.service" \
            "Installer running for ${elapsed}s (limit ${INSTALLER_MAX_SEC}s)"
    fi
}

# ── Check: Runaway AI/automation CPU ──────────────────────────────────────
check_cpu_runaway() {
    local targets=("hisnos-automation" "hisnosd --module automation" "hisnosd --module threat")
    for pattern in "${targets[@]}"; do
        local pid
        pid=$(pgrep -f "$pattern" 2>/dev/null | head -1) || continue
        [[ -z "$pid" ]] && continue

        local cpu
        cpu=$(ps -o %cpu= -p "$pid" 2>/dev/null | tr -d ' ' | cut -d. -f1) || continue
        [[ -z "$cpu" ]] && continue

        if [[ "$cpu" -ge "$CPU_THRESHOLD" ]]; then
            cpu_violations[$pid]=$(( ${cpu_violations[$pid]:-0} + 1 ))
            if [[ "${cpu_violations[$pid]}" -ge "$CPU_VIOLATION_MAX" ]]; then
                safe_kill "$pid" "$pattern" \
                    "CPU at ${cpu}% for $((CPU_VIOLATION_MAX * POLL_INTERVAL))s (threshold ${CPU_THRESHOLD}%)"
                unset "cpu_violations[$pid]"
            fi
        else
            unset "cpu_violations[$pid]" 2>/dev/null || true
        fi
    done
}

# ── Check: Memory pressure ───────────────────────────────────────────────
check_memory_pressure() {
    local cgroup_base="/sys/fs/cgroup/system.slice"
    for svc in "${MONITORED_SERVICES[@]}"; do
        local cg="$cgroup_base/$svc"
        [[ -d "$cg" ]] || continue

        local current max
        current=$(cat "$cg/memory.current" 2>/dev/null) || continue
        max=$(cat "$cg/memory.max" 2>/dev/null) || continue
        [[ "$max" == "max" ]] && continue

        local pct=$(( current * 100 / max ))
        if [[ "$pct" -ge "$MEM_PRESSURE_PCT" ]]; then
            log_warn "Memory pressure on $svc: ${pct}% of limit (${current}/${max})"
            service_disable "$svc" "Memory at ${pct}% of MemoryMax limit — likely leak"
        fi
    done
}

# ── Main loop ─────────────────────────────────────────────────────────────
log_info "Watchdog started (poll=${POLL_INTERVAL}s, safe_mode_thresh=${SAFE_MODE_FAIL_THRESH})"
mkdir -p "$HISNOS_RUN" 2>/dev/null || true

while true; do
    check_restart_loops
    check_cpu_lock
    check_gaming
    check_update
    check_installer
    check_cpu_runaway
    check_memory_pressure
    sleep "$POLL_INTERVAL"
done
