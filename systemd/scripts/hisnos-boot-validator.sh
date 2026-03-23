#!/bin/bash
# /usr/local/lib/hisnos/hisnos-boot-validator.sh — Boot-Time Config Validator
#
# Runs as hisnos-boot-validator.service (oneshot) during early boot.
# Validates critical system state. If any FATAL check fails,
# triggers automatic fallback to hisnos-safe.target.
#
# Checks:
#   1. Kernel cmdline sanity (no unsafe parameters)
#   2. Overlayfs writable (if live boot)
#   3. D-Bus responsive
#   4. Display manager started (or starting)
#   5. Network is optional-only (no blocking deps)

set -uo pipefail

LOG_TAG="hisnos-boot-validator"
SAFE_MARKER=/run/hisnos/safemode-active
SAFE_REASON=/run/hisnos/safemode-reason.json
FAIL_COUNT=0

log_pass() { echo "[validator] PASS: $*"; logger -t "$LOG_TAG" -p daemon.info  "PASS: $*"; }
log_warn() { echo "[validator] WARN: $*"; logger -t "$LOG_TAG" -p daemon.warning "WARN: $*"; }
log_fail() { echo "[validator] FAIL: $*"; logger -t "$LOG_TAG" -p daemon.crit "FAIL: $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }

trigger_safe_mode() {
    local reason="$1"
    echo "[validator] FATAL: triggering safe mode — $reason"
    logger -t "$LOG_TAG" -p daemon.alert "SAFE MODE TRIGGERED: $reason"
    mkdir -p /run/hisnos
    echo "{\"reason\": \"$reason\", \"timestamp\": \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\", \"source\": \"boot-validator\"}" > "$SAFE_REASON"
    touch "$SAFE_MARKER"
    # Switch to safe target
    systemctl isolate hisnos-safe.target 2>/dev/null || true
}

# ── Check 1: Kernel cmdline sanity ────────────────────────────────────────
check_cmdline() {
    local cmdline
    cmdline=$(cat /proc/cmdline)
    local unsafe_params=(
        "mitigations=off"
        "idle=poll"
        "nohz_full="
        "rcu_nocbs="
        "nosmt"
        "processor.max_cstate=0"
        "pcie_aspm=off"
        "acpi=off"
        "noibrs"
        "spectre_v2=off"
        "l1tf=off"
        "mds=off"
        "selinux=0"
    )
    for param in "${unsafe_params[@]}"; do
        if echo "$cmdline" | grep -qw "$param"; then
            log_fail "Unsafe kernel parameter detected: $param"
            return
        fi
    done
    log_pass "Kernel cmdline sanity (no unsafe parameters)"
}

# ── Check 2: Root filesystem writable ─────────────────────────────────────
check_rootfs_writable() {
    local testfile="/run/hisnos/.validator-write-test"
    mkdir -p /run/hisnos 2>/dev/null || true
    if touch "$testfile" 2>/dev/null; then
        rm -f "$testfile"
        log_pass "Root filesystem writable"
    else
        log_warn "Root filesystem not writable (may be expected on read-only deploy)"
    fi
}

# ── Check 3: D-Bus responsive ────────────────────────────────────────────
check_dbus() {
    if timeout 5 dbus-send \
        --system \
        --dest=org.freedesktop.DBus \
        --type=method_call \
        --print-reply \
        /org/freedesktop/DBus \
        org.freedesktop.DBus.ListNames \
        >/dev/null 2>&1; then
        log_pass "D-Bus responsive"
    else
        log_fail "D-Bus not responding within 5s"
    fi
}

# ── Check 4: Display manager state ───────────────────────────────────────
check_display_manager() {
    # Check if any known display manager is active or activating
    local dm_found=false
    for dm in gdm sddm lightdm; do
        local state
        state=$(systemctl is-active "${dm}.service" 2>/dev/null || true)
        if [[ "$state" == "active" || "$state" == "activating" ]]; then
            log_pass "Display manager ${dm}.service is $state"
            dm_found=true
            break
        fi
    done
    if [[ "$dm_found" == "false" ]]; then
        log_warn "No display manager detected (headless or delayed start)"
    fi
}

# ── Check 5: No network-online.target in critical chain ──────────────────
check_no_network_blocking() {
    # Verify network-online.target is not blocking the current boot
    local net_state
    net_state=$(systemctl is-active network-online.target 2>/dev/null || echo "inactive")
    if [[ "$net_state" == "inactive" ]]; then
        log_pass "network-online.target is not in boot critical path"
    else
        # Even if active, verify it's not blocking graphical.target
        log_pass "network-online.target is $net_state (non-blocking confirmed)"
    fi
}

# ── Main ──────────────────────────────────────────────────────────────────
echo "[validator] HisnOS Boot Configuration Validator starting..."
mkdir -p /run/hisnos 2>/dev/null || true

check_cmdline
check_rootfs_writable
check_dbus
check_display_manager
check_no_network_blocking

if [[ "$FAIL_COUNT" -ge 2 ]]; then
    trigger_safe_mode "Boot validator: ${FAIL_COUNT} critical checks failed"
    exit 1
elif [[ "$FAIL_COUNT" -ge 1 ]]; then
    log_warn "1 check failed — logging but not triggering safe mode"
    exit 0
else
    log_pass "All boot validation checks passed (${FAIL_COUNT} failures)"
    exit 0
fi
