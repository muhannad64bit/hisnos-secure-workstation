#!/bin/bash
# HisnOS Chaos Test — Group 2: Runtime Chaos (source-validation mode)
# Validates service hardening, resource limits, and watchdog coverage.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
SVC="$PROJECT_ROOT/systemd"
PERF_SVC="$PROJECT_ROOT/core/performance/systemd"
SCRIPTS="$PROJECT_ROOT/systemd/scripts"

LOG="/tmp/chaos-runtime.log"
RESULTS_DIR="/tmp/chaos-results"
PASS=0; FAIL=0; SKIP=0
mkdir -p "$RESULTS_DIR" 2>/dev/null || true

log()    { echo "[$(date -u +%T)] $*" | tee -a "$LOG"; }
pass()   { log "  ✅ PASS: $*"; PASS=$((PASS+1)); }
fail()   { log "  ❌ FAIL: $*"; FAIL=$((FAIL+1)); }
skip()   { log "  ⏭️  SKIP: $*"; SKIP=$((SKIP+1)); }
header() { log ""; log "═══ TEST: $* ═══"; }

# ── 2.1: CPU stress — services have CPUWeight and CPUQuota ────────────────
test_cpu_stress() {
    header "2.1 — CPU isolation: deferred services have low weight"
    local targets=(
        "$SVC/hisnos-automation.service"
        "$SVC/hisnos-performance-guard.service"
        "$SVC/hisnos-threat-engine.service"
        "$PERF_SVC/hisnos-irq-balancer.service"
        "$PERF_SVC/hisnos-thermal.service"
        "$PERF_SVC/hisnos-rt-guard.service"
    )
    for f in "${targets[@]}"; do
        local bn=$(basename "$f")
        [[ -f "$f" ]] || { fail "Missing $bn"; continue; }
        grep -q 'CPUWeight=10' "$f" && pass "$bn: CPUWeight=10" || fail "$bn: missing CPUWeight=10"
        grep -q 'CPUQuota=' "$f" && pass "$bn: has CPUQuota" || fail "$bn: missing CPUQuota"
        grep -q 'Nice=10' "$f" && pass "$bn: Nice=10" || fail "$bn: missing Nice=10"
        grep -q 'Type=idle' "$f" && pass "$bn: Type=idle" || fail "$bn: not Type=idle"
    done
}

# ── 2.2: Memory pressure — services have MemoryMax ────────────────────────
test_mem_pressure() {
    header "2.2 — Memory limits: all services have MemoryMax"
    for f in "$SVC"/hisnos-*.service "$PERF_SVC"/hisnos-*.service; do
        [[ -f "$f" ]] || continue
        local bn=$(basename "$f")
        grep -q 'MemoryMax=' "$f" && pass "$bn: has MemoryMax" || fail "$bn: missing MemoryMax"
    done
    # Verify watchdog monitors memory pressure
    local ws="$SCRIPTS/watchdog.sh"
    [[ -f "$ws" ]] && grep -q 'MEM_PRESSURE_PCT' "$ws" && pass "Watchdog monitors memory pressure" || fail "Watchdog missing memory check"
}

# ── 2.3: Disk full — stability guard detects pressure ─────────────────────
test_disk_full() {
    header "2.3 — Disk pressure: stability guard detects"
    local sg="$SCRIPTS/hisnos-stability-guard.sh"
    [[ -f "$sg" ]] || { fail "Missing stability-guard.sh"; return; }
    bash -n "$sg" && pass "Stability guard syntax OK" || fail "Syntax error"
    grep -q 'check_disk_pressure' "$sg" && pass "Has disk pressure check" || fail "Missing disk check"
    grep -q '90' "$sg" && pass "Has 90% threshold" || fail "Missing threshold"
    # Verify timer exists
    [[ -f "$SVC/hisnos-stability-guard.timer" ]] && pass "Stability guard timer exists" || fail "Missing timer"
    grep -q 'OnUnitActiveSec=6h' "$SVC/hisnos-stability-guard.timer" && pass "Runs every 6h" || fail "Wrong interval"
}

# ── 2.4: Journal flood — stability guard vacuums ──────────────────────────
test_journal_flood() {
    header "2.4 — Journal flood: stability guard auto-vacuums"
    local sg="$SCRIPTS/hisnos-stability-guard.sh"
    [[ -f "$sg" ]] || { fail "Missing stability-guard.sh"; return; }
    grep -q 'check_journal_size' "$sg" && pass "Has journal size check" || fail "Missing journal check"
    grep -q 'vacuum' "$sg" && pass "Has journal vacuum action" || fail "No auto-vacuum"
    grep -q '500' "$sg" && pass "Journal limit 500MB" || fail "Wrong journal limit"
}

# ── 2.5: D-Bus restart — boot validator checks dbus ──────────────────────
test_dbus_restart() {
    header "2.5 — D-Bus resilience: boot validator checks dbus"
    local bv="$SCRIPTS/hisnos-boot-validator.sh"
    [[ -f "$bv" ]] || { fail "Missing boot-validator.sh"; return; }
    grep -q 'check_dbus' "$bv" && pass "Boot validator checks D-Bus" || fail "No dbus check"
    grep -q 'timeout.*dbus-send' "$bv" && pass "D-Bus check has timeout" || fail "No timeout on dbus check"
}

# ── 2.6: NM crash — no boot dependency, NM restarts ──────────────────────
test_nm_crash() {
    header "2.6 — NetworkManager crash: no boot dependency"
    # Verify no boot service depends on network-online.target
    local net_deps=0
    for f in "$SVC"/hisnos-*.service; do
        [[ -f "$f" ]] || continue
        local bn=$(basename "$f")
        # Only check non-timer, non-fleet boot services
        if grep -q 'Wants=network-online.target' "$f" 2>/dev/null; then
            fail "$bn: still has Wants=network-online.target"
            net_deps=$((net_deps+1))
        fi
    done
    [[ $net_deps -eq 0 ]] && pass "No service has Wants=network-online.target"

    # live-health should not require NetworkManager
    local lh="$SVC/hisnos-live-health.service"
    if [[ -f "$lh" ]]; then
        grep -q 'After=.*NetworkManager' "$lh" && fail "live-health still depends on NM" || pass "live-health: no NM dependency"
    fi
}

case "${1:-all}" in
    cpu-stress) test_cpu_stress ;; mem-pressure) test_mem_pressure ;;
    disk-full) test_disk_full ;; journal-flood) test_journal_flood ;;
    dbus-restart) test_dbus_restart ;; nm-crash) test_nm_crash ;;
    all) test_cpu_stress; test_mem_pressure; test_disk_full; test_journal_flood; test_dbus_restart; test_nm_crash ;;
    *) echo "Usage: $0 {all|cpu-stress|mem-pressure|disk-full|journal-flood|dbus-restart|nm-crash}"; exit 1 ;;
esac

log ""; log "  RUNTIME CHAOS: ✅ $PASS passed, ❌ $FAIL failed, ⏭️ $SKIP skipped"
cat > "$RESULTS_DIR/runtime-chaos.json" <<EOF
{"group":"runtime","timestamp":"$(date -u +%Y-%m-%dT%H:%M:%SZ)","pass":$PASS,"fail":$FAIL,"skip":$SKIP}
EOF
exit $FAIL
