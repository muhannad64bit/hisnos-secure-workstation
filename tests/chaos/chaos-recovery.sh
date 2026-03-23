#!/bin/bash
# HisnOS Chaos Test — Group 5: Recovery Validation (source-validation mode)
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
SVC="$PROJECT_ROOT/systemd"
SCRIPTS="$PROJECT_ROOT/systemd/scripts"
GRUB="$PROJECT_ROOT/recovery/grub.d"

LOG="/tmp/chaos-recovery.log"
RESULTS_DIR="/tmp/chaos-results"
PASS=0; FAIL=0; SKIP=0
mkdir -p "$RESULTS_DIR" 2>/dev/null || true

log()    { echo "[$(date -u +%T)] $*" | tee -a "$LOG"; }
pass()   { log "  ✅ PASS: $*"; PASS=$((PASS+1)); }
fail()   { log "  ❌ FAIL: $*"; FAIL=$((FAIL+1)); }
skip()   { log "  ⏭️  SKIP: $*"; SKIP=$((SKIP+1)); }
header() { log ""; log "═══ TEST: $* ═══"; }

# ── 5.1: Safe-mode escalation correctness ─────────────────────────────────
test_safe_mode() {
    header "5.1 — Safe-mode target: Conflicts coverage"
    local target="$SVC/hisnos-safe.target"
    [[ -f "$target" ]] || { fail "Missing hisnos-safe.target"; return; }

    local required=(gaming automation performance-guard threat-engine fleet-sync irq-balancer thermal rt-guard)
    for svc in "${required[@]}"; do
        grep -q "$svc" "$target" && pass "Conflicts=$svc" || fail "Missing Conflicts=$svc"
    done

    # Verify safe-mode.service masks services
    local sms="$SVC/hisnos-safe-mode.service"
    [[ -f "$sms" ]] || { fail "Missing safe-mode.service"; return; }
    grep -q 'mask --runtime' "$sms" && pass "Safe-mode masks at runtime" || fail "No runtime masking"
    grep -q 'threat-engine' "$sms" && pass "Safe-mode masks threat-engine" || fail "Missing threat-engine"
    grep -q 'automation' "$sms" && pass "Safe-mode masks automation" || fail "Missing automation"
}

# ── 5.2: Watchdog decision correctness ─────────────────────────────────────
test_watchdog() {
    header "5.2 — Watchdog: all detection mechanisms"
    local ws="$SCRIPTS/watchdog.sh"
    [[ -f "$ws" ]] || { fail "Missing watchdog.sh"; return; }
    bash -n "$ws" && pass "Watchdog syntax OK" || fail "Syntax error"

    grep -q 'RESTART_LOOP_THRESH' "$ws" && pass "Restart-loop detection" || fail "Missing restart-loop"
    grep -q 'RESTART_LOOP_WINDOW' "$ws" && pass "Restart window defined" || fail "Missing window"
    grep -q 'CPU_LOCK_SEC' "$ws" && pass "CPU lock detection" || fail "Missing CPU lock"
    grep -q 'MEM_PRESSURE_PCT' "$ws" && pass "Memory pressure detection" || fail "Missing memory"
    grep -q 'SAFE_MODE_FAIL_THRESH' "$ws" && pass "Safe-mode threshold" || fail "Missing threshold"
    grep -q 'hisnos-safe.target' "$ws" && pass "Escalates to safe target" || fail "Wrong target"
    grep -q 'SIGTERM\|SIGKILL' "$ws" && pass "Has kill escalation" || fail "No kill escalation"
    grep -q 'service_disable' "$ws" && pass "Has service disable" || fail "No disable function"

    # Verify service scheduling
    local wsvc="$SVC/hisnos-watchdog.service"
    [[ -f "$wsvc" ]] || { fail "Missing watchdog.service"; return; }
    grep -q 'CPUSchedulingPolicy=batch' "$wsvc" && pass "Batch scheduling" || fail "Wrong scheduling"
    grep -q 'CPUWeight=10' "$wsvc" && pass "CPUWeight=10" || fail "Missing CPUWeight"
}

# ── 5.3: Rollback correctness ─────────────────────────────────────────────
test_rollback() {
    header "5.3 — Panic rollback: full correctness"
    local rs="$SCRIPTS/hisnos-panic-rollback.sh"
    [[ -f "$rs" ]] || { fail "Missing panic-rollback.sh"; return; }
    bash -n "$rs" && pass "Syntax OK" || fail "Syntax error"

    grep -q 'PANIC_THRESHOLD=3' "$rs" && pass "3-strike threshold" || fail "Wrong threshold"
    grep -q 'PANIC_COUNT_FILE' "$rs" && pass "Counter tracked" || fail "No counter"
    grep -q 'echo "0"' "$rs" && pass "Counter resets on clean" || fail "No reset"
    grep -q 'grub2-editenv\|hisnos_bad_deploy' "$rs" && pass "GRUB env marking" || fail "No GRUB mark"
    grep -q 'rpm-ostree rollback' "$rs" && pass "rpm-ostree rollback" || fail "No rollback"

    local svc="$SVC/hisnos-panic-rollback.service"
    [[ -f "$svc" ]] || { fail "Missing service"; return; }
    grep -q 'ExecStop.*last-boot-clean' "$svc" && pass "ExecStop marker" || fail "No stop marker"
    grep -q 'Before=multi-user.target' "$svc" && pass "Runs early in boot" || fail "Late ordering"
}

# ── 5.4: GRUB fallback correctness ────────────────────────────────────────
test_grub_fallback() {
    header "5.4 — GRUB fallback entries"
    # Recovery
    [[ -x "$GRUB/41_hisnos-recovery" ]] && pass "Recovery GRUB entry (+x)" || fail "Missing recovery"
    # Safe Mode
    local gs="$GRUB/42_hisnos-safe-mode"
    [[ -x "$gs" ]] && pass "Safe Mode GRUB (+x)" || fail "Missing safe mode"
    if [[ -f "$gs" ]]; then
        bash -n "$gs" && pass "Safe Mode syntax OK" || fail "Syntax error"
        grep -q 'hisnos.safe=1' "$gs" && pass "Sets hisnos.safe=1" || fail "Missing safe flag"
        grep -q 'multi-user.target' "$gs" && pass "Boots to multi-user" || fail "Wrong target"
    fi
    # Safe Hardware
    local gh="$GRUB/43_hisnos-safe-hardware"
    [[ -x "$gh" ]] && pass "Safe Hardware GRUB (+x)" || fail "Missing safe hw"
    if [[ -f "$gh" ]]; then
        bash -n "$gh" && pass "Safe HW syntax OK" || fail "Syntax error"
        grep -q 'nomodeset' "$gh" && pass "nomodeset" || fail "Missing nomodeset"
        grep -q 'intel_pstate=passive' "$gh" && pass "passive pstate" || fail "Missing pstate"
    fi
}

# ── 5.5: Boot validator correctness ────────────────────────────────────────
test_boot_validator() {
    header "5.5 — Boot validator: all 5 checks"
    local vs="$SCRIPTS/hisnos-boot-validator.sh"
    [[ -f "$vs" ]] || { fail "Missing boot-validator.sh"; return; }
    bash -n "$vs" && pass "Syntax OK" || fail "Syntax error"

    grep -q 'check_cmdline' "$vs" && pass "Check 1: cmdline" || fail "No cmdline check"
    grep -q 'check_rootfs' "$vs" && pass "Check 2: rootfs" || fail "No rootfs check"
    grep -q 'check_dbus' "$vs" && pass "Check 3: dbus" || fail "No dbus check"
    grep -q 'check_display_manager' "$vs" && pass "Check 4: display mgr" || fail "No DM check"
    grep -q 'check_no_network' "$vs" && pass "Check 5: network" || fail "No network check"

    local svc="$SVC/hisnos-boot-validator.service"
    [[ -f "$svc" ]] || { fail "Missing service"; return; }
    grep -q 'TimeoutStartSec=15' "$svc" && pass "15s timeout" || fail "Wrong timeout"
    grep -q 'Before=graphical.target' "$svc" && pass "Before graphical" || fail "Wrong ordering"
}

case "${1:-all}" in
    safe-mode) test_safe_mode ;; watchdog) test_watchdog ;; rollback) test_rollback ;;
    grub) test_grub_fallback ;; validator) test_boot_validator ;;
    all) test_safe_mode; test_watchdog; test_rollback; test_grub_fallback; test_boot_validator ;;
    *) echo "Usage: $0 {all|safe-mode|watchdog|rollback|grub|validator}"; exit 1 ;;
esac

log ""; log "  RECOVERY: ✅ $PASS passed, ❌ $FAIL failed, ⏭️ $SKIP skipped"
cat > "$RESULTS_DIR/recovery-chaos.json" <<EOF
{"group":"recovery","timestamp":"$(date -u +%Y-%m-%dT%H:%M:%SZ)","pass":$PASS,"fail":$FAIL,"skip":$SKIP}
EOF
exit $FAIL
