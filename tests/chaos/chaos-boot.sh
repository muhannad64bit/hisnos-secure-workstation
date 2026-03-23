#!/bin/bash
# HisnOS Chaos Test — Group 1: Boot Chaos (source-validation mode)
set -uo pipefail

# ── Auto-detect project root ─────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
SVC="$PROJECT_ROOT/systemd"
PERF_SVC="$PROJECT_ROOT/core/performance/systemd"
SCRIPTS="$PROJECT_ROOT/systemd/scripts"
CONFIG="$PROJECT_ROOT/config"
GRUB="$PROJECT_ROOT/recovery/grub.d"

LOG="/tmp/chaos-boot.log"
RESULTS_DIR="/tmp/chaos-results"
PASS=0; FAIL=0; SKIP=0
mkdir -p "$RESULTS_DIR" 2>/dev/null || true

log()    { echo "[$(date -u +%T)] $*" | tee -a "$LOG"; }
pass()   { log "  ✅ PASS: $*"; PASS=$((PASS+1)); }
fail()   { log "  ❌ FAIL: $*"; FAIL=$((FAIL+1)); }
skip()   { log "  ⏭️  SKIP: $*"; SKIP=$((SKIP+1)); }
header() { log ""; log "═══ TEST: $* ═══"; }

# ── 1.1: Corrupted ISO — emergency TUI exists ────────────────────────────
test_corrupted_iso() {
    header "1.1 — Emergency TUI for corrupted boot"
    local esh="$PROJECT_ROOT/dracut/modules.d/90hisnos-live/hisnos-emergency.sh"
    [[ -f "$esh" ]] && pass "Emergency TUI script exists" || fail "Missing hisnos-emergency.sh"
    if [[ -f "$esh" ]]; then
        grep -q 'Retry\|retry' "$esh" && pass "Emergency TUI has retry option" || fail "No retry option"
        grep -q 'shell\|Shell' "$esh" && pass "Emergency TUI has shell option" || fail "No shell option"
        grep -q 'reboot\|Reboot' "$esh" && pass "Emergency TUI has reboot option" || fail "No reboot option"
        bash -n "$esh" && pass "Emergency TUI script syntax OK" || fail "Syntax error in emergency script"
    fi
}

# ── 1.2: Slow USB — dracut config excludes network blockers ──────────────
test_slow_usb() {
    header "1.2 — Dracut config optimized for slow media"
    local dracut_conf="$CONFIG/dracut/90-hisnos-stable.conf"
    if [[ -f "$dracut_conf" ]]; then
        grep -q 'omit_dracutmodules' "$dracut_conf" && pass "Dracut omits unnecessary modules" || fail "No omit_dracutmodules"
        for mod in iscsi nfs cifs fcoe; do
            grep -q "$mod" "$dracut_conf" && pass "Dracut omits $mod (no network boot)" || skip "$mod not in omit list"
        done
    else
        skip "Dracut config not found at $dracut_conf"
    fi
}

# ── 1.3: Missing GPU — no boot service depends on GPU ────────────────────
test_missing_gpu() {
    header "1.3 — No boot service depends on GPU"
    local gpu_deps=0
    for svc in "$SVC"/hisnos-*.service "$PERF_SVC"/hisnos-*.service; do
        [[ -f "$svc" ]] || continue
        if grep -qi 'gpu\|drm\|render\|nvidia\|amdgpu' "$svc" 2>/dev/null; then
            if grep -q 'WantedBy=multi-user.target\|WantedBy=sysinit.target' "$svc"; then
                fail "Boot service $(basename "$svc") has GPU dependency"
                gpu_deps=$((gpu_deps+1))
            fi
        fi
    done
    [[ $gpu_deps -eq 0 ]] && pass "No boot-critical service depends on GPU"

    # Verify thermal/irq have ConditionPathExists guards
    [[ -f "$PERF_SVC/hisnos-thermal.service" ]] && \
        grep -q 'ConditionPathExists=/sys/class/hwmon' "$PERF_SVC/hisnos-thermal.service" && \
        pass "Thermal has hwmon guard" || fail "Thermal missing hwmon guard"
    [[ -f "$PERF_SVC/hisnos-irq-balancer.service" ]] && \
        grep -q 'ConditionPathExists=/proc/interrupts' "$PERF_SVC/hisnos-irq-balancer.service" && \
        pass "IRQ balancer has /proc/interrupts guard" || fail "IRQ missing guard"
}

# ── 1.4: Bad cmdline — boot validator catches unsafe params ──────────────
test_bad_cmdline() {
    header "1.4 — Boot validator catches unsafe kernel params"
    local validator="$SCRIPTS/hisnos-boot-validator.sh"
    [[ -f "$validator" ]] || { fail "Missing hisnos-boot-validator.sh"; return; }

    pass "Boot validator script exists"
    bash -n "$validator" && pass "Boot validator syntax OK" || fail "Syntax error"

    for param in "mitigations=off" "idle=poll" "nohz_full" "nosmt" "spectre_v2=off"; do
        grep -q "$param" "$validator" && pass "Validator checks '$param'" || fail "Validator missing '$param'"
    done

    # Verify service has tight timeout
    local svc="$SVC/hisnos-boot-validator.service"
    [[ -f "$svc" ]] || { fail "Missing boot-validator.service"; return; }
    grep -q 'TimeoutStartSec=15' "$svc" && pass "Validator has 15s timeout" || fail "Missing/wrong timeout"
    grep -q 'Before=graphical.target' "$svc" && pass "Validator runs before graphical" || fail "Wrong ordering"
}

# ── 1.5: Broken ACPI — safe hardware GRUB entry ─────────────────────────
test_broken_acpi() {
    header "1.5 — Safe Hardware GRUB entry for broken ACPI"
    local grub_hw="$GRUB/43_hisnos-safe-hardware"
    [[ -f "$grub_hw" ]] || { fail "Missing 43_hisnos-safe-hardware"; return; }
    [[ -x "$grub_hw" ]] && pass "Safe HW GRUB script is executable" || fail "Not executable"
    bash -n "$grub_hw" && pass "Safe HW GRUB syntax OK" || fail "Syntax error"
    grep -q 'nomodeset' "$grub_hw" && pass "Sets nomodeset" || fail "Missing nomodeset"
    grep -q 'nohz=off' "$grub_hw" && pass "Sets nohz=off" || fail "Missing nohz=off"
    grep -q 'intel_pstate=passive' "$grub_hw" && pass "Sets passive pstate" || fail "Missing passive pstate"
}

# ── Runner ────────────────────────────────────────────────────────────────
case "${1:-all}" in
    corrupted-iso) test_corrupted_iso ;; slow-usb) test_slow_usb ;;
    missing-gpu) test_missing_gpu ;; bad-cmdline) test_bad_cmdline ;;
    broken-acpi) test_broken_acpi ;;
    all) test_corrupted_iso; test_slow_usb; test_missing_gpu; test_bad_cmdline; test_broken_acpi ;;
    *) echo "Usage: $0 {all|corrupted-iso|slow-usb|missing-gpu|bad-cmdline|broken-acpi}"; exit 1 ;;
esac

log ""; log "  BOOT CHAOS: ✅ $PASS passed, ❌ $FAIL failed, ⏭️ $SKIP skipped"
cat > "$RESULTS_DIR/boot-chaos.json" <<EOF
{"group":"boot","timestamp":"$(date -u +%Y-%m-%dT%H:%M:%SZ)","pass":$PASS,"fail":$FAIL,"skip":$SKIP}
EOF
exit $FAIL
