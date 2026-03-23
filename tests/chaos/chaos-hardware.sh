#!/bin/bash
# HisnOS Chaos Test — Group 4: Hardware Chaos (source-validation mode)
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
SVC="$PROJECT_ROOT/systemd"
PERF_SVC="$PROJECT_ROOT/core/performance/systemd"
GRUB="$PROJECT_ROOT/recovery/grub.d"
SCRIPTS="$PROJECT_ROOT/systemd/scripts"

LOG="/tmp/chaos-hardware.log"
RESULTS_DIR="/tmp/chaos-results"
PASS=0; FAIL=0; SKIP=0
mkdir -p "$RESULTS_DIR" 2>/dev/null || true

log()    { echo "[$(date -u +%T)] $*" | tee -a "$LOG"; }
pass()   { log "  ✅ PASS: $*"; PASS=$((PASS+1)); }
fail()   { log "  ❌ FAIL: $*"; FAIL=$((FAIL+1)); }
skip()   { log "  ⏭️  SKIP: $*"; SKIP=$((SKIP+1)); }
header() { log ""; log "═══ TEST: $* ═══"; }

# ── 4.1: Suspend/resume — no service depends on resume ────────────────────
test_suspend_spam() {
    header "4.1 — No service depends on suspend/resume events"
    local dep_count=0
    for f in "$SVC"/hisnos-*.service "$PERF_SVC"/hisnos-*.service; do
        [[ -f "$f" ]] || continue
        if grep -qi 'sleep.target\|suspend.target\|hibernate' "$f" 2>/dev/null; then
            fail "$(basename "$f"): depends on sleep/suspend"
            dep_count=$((dep_count+1))
        fi
    done
    [[ $dep_count -eq 0 ]] && pass "No service depends on suspend/resume"

    # Verify watchdog uses RestartSec (survives resume)
    [[ -f "$SVC/hisnos-watchdog.service" ]] && \
        grep -q 'Restart=' "$SVC/hisnos-watchdog.service" && \
        pass "Watchdog has Restart= (survives resume)" || fail "Watchdog missing Restart="
}

# ── 4.2: HDMI hotplug — no boot service depends on display output ─────────
test_hdmi_hotplug() {
    header "4.2 — No boot service depends on display output"
    for f in "$SVC"/hisnos-*.service "$PERF_SVC"/hisnos-*.service; do
        [[ -f "$f" ]] || continue
        if grep -qi 'drm\|display.*output\|monitor.*count\|xrandr' "$f" 2>/dev/null; then
            fail "$(basename "$f"): has display output dependency"
            return
        fi
    done
    pass "No boot service depends on display output"
}

# ── 4.3: Wifi storm — fleet-sync tolerates network loss ───────────────────
test_wifi_storm() {
    header "4.3 — Fleet-sync tolerates network loss"
    local fleet_svc="$PROJECT_ROOT/core/fleet/systemd/hisnos-fleet-sync.service"
    [[ -f "$fleet_svc" ]] || { skip "Fleet-sync service not found"; return; }

    grep -q 'Restart=on-failure' "$fleet_svc" && pass "Fleet-sync restarts on failure" || fail "No restart"
    grep -q 'IOSchedulingClass=idle' "$fleet_svc" && pass "Fleet-sync uses idle IO" || fail "Missing idle IO"

    # Verify no boot service depends on network-online
    local net_deps=0
    for f in "$SVC"/hisnos-boot-complete.service "$SVC"/hisnos-live-health.service; do
        [[ -f "$f" ]] || continue
        if grep -q 'network-online.target' "$f"; then
            fail "$(basename "$f"): depends on network-online.target"
            net_deps=$((net_deps+1))
        fi
    done
    [[ $net_deps -eq 0 ]] && pass "Boot services independent of network"
}

# ── 4.4: NVMe/hardware reset — safe hardware GRUB entry ──────────────────
test_nvme_reset() {
    header "4.4 — Hardware fallback: GRUB entries + ConditionPath guards"
    local grub_hw="$GRUB/43_hisnos-safe-hardware"
    [[ -f "$grub_hw" ]] && pass "Safe Hardware GRUB entry exists" || fail "Missing GRUB entry"

    # Verify ConditionPathExists guards on hardware services
    local guarded=0
    for f in "$PERF_SVC"/hisnos-*.service; do
        [[ -f "$f" ]] || continue
        if grep -q 'ConditionPathExists=' "$f"; then
            guarded=$((guarded+1))
        fi
    done
    [[ $guarded -ge 2 ]] && pass "$guarded services have ConditionPathExists guards" || fail "Too few guards ($guarded)"

    # Verify all performance services deferred to graphical
    for f in "$PERF_SVC"/hisnos-*.service; do
        [[ -f "$f" ]] || continue
        grep -q 'WantedBy=graphical.target' "$f" && \
            pass "$(basename "$f"): WantedBy=graphical.target" || \
            fail "$(basename "$f"): not deferred to graphical"
    done
}

case "${1:-all}" in
    suspend-spam) test_suspend_spam ;; hdmi-hotplug) test_hdmi_hotplug ;;
    wifi-storm) test_wifi_storm ;; nvme-reset) test_nvme_reset ;;
    all) test_suspend_spam; test_hdmi_hotplug; test_wifi_storm; test_nvme_reset ;;
    *) echo "Usage: $0 {all|suspend-spam|hdmi-hotplug|wifi-storm|nvme-reset}"; exit 1 ;;
esac

log ""; log "  HARDWARE CHAOS: ✅ $PASS passed, ❌ $FAIL failed, ⏭️ $SKIP skipped"
cat > "$RESULTS_DIR/hardware-chaos.json" <<EOF
{"group":"hardware","timestamp":"$(date -u +%Y-%m-%dT%H:%M:%SZ)","pass":$PASS,"fail":$FAIL,"skip":$SKIP}
EOF
exit $FAIL
