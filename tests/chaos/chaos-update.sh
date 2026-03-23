#!/bin/bash
# HisnOS Chaos Test — Group 3: Update Chaos (source-validation mode)
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
SVC="$PROJECT_ROOT/systemd"
SCRIPTS="$PROJECT_ROOT/systemd/scripts"
CONFIG="$PROJECT_ROOT/config/sysctl"

LOG="/tmp/chaos-update.log"
RESULTS_DIR="/tmp/chaos-results"
PASS=0; FAIL=0; SKIP=0
mkdir -p "$RESULTS_DIR" 2>/dev/null || true

log()    { echo "[$(date -u +%T)] $*" | tee -a "$LOG"; }
pass()   { log "  ✅ PASS: $*"; PASS=$((PASS+1)); }
fail()   { log "  ❌ FAIL: $*"; FAIL=$((FAIL+1)); }
skip()   { log "  ⏭️  SKIP: $*"; SKIP=$((SKIP+1)); }
header() { log ""; log "═══ TEST: $* ═══"; }

# ── 3.1: Interrupted deploy — watchdog monitors rpm-ostree ────────────────
test_interrupted_deploy() {
    header "3.1 — Watchdog monitors rpm-ostree"
    local ws="$SCRIPTS/watchdog.sh"
    [[ -f "$ws" ]] || { fail "Missing watchdog.sh"; return; }
    grep -q 'rpm-ostree' "$ws" && pass "Watchdog monitors rpm-ostree" || fail "No rpm-ostree monitoring"
    grep -q 'UPDATE_MAX_SEC' "$ws" && pass "Update timeout defined" || fail "Missing update timeout"
    local timeout=$(grep 'UPDATE_MAX_SEC=' "$ws" | head -1 | grep -o '[0-9]*')
    [[ -n "$timeout" && "$timeout" -le 3600 ]] && pass "Update timeout ≤1h ($timeout s)" || fail "Update timeout too long"
    grep -q 'safe_kill.*rpm-ostree\|SIGTERM\|SIGKILL' "$ws" && pass "Has kill escalation" || fail "No kill escalation"
}

# ── 3.2: Power loss — panic rollback detects unclean shutdown ─────────────
test_power_loss() {
    header "3.2 — Panic rollback: unclean shutdown detection"
    local rs="$SCRIPTS/hisnos-panic-rollback.sh"
    [[ -f "$rs" ]] || { fail "Missing panic-rollback.sh"; return; }
    bash -n "$rs" && pass "Panic rollback syntax OK" || fail "Syntax error"
    grep -q 'last-boot-clean' "$rs" && pass "Tracks clean shutdown marker" || fail "No clean marker"
    grep -q 'PANIC_THRESHOLD' "$rs" && pass "Has panic threshold" || fail "No threshold"

    # Verify service creates marker on ExecStop
    local svc="$SVC/hisnos-panic-rollback.service"
    [[ -f "$svc" ]] || { fail "Missing panic-rollback.service"; return; }
    grep -q 'ExecStop.*last-boot-clean' "$svc" && pass "ExecStop creates marker" || fail "No ExecStop marker"
    grep -q 'RemainAfterExit=yes' "$svc" && pass "RemainAfterExit=yes" || fail "Missing RemainAfterExit"

    # Verify kernel.panic sysctl
    local sysctl="$CONFIG/hisnos-stable.conf"
    [[ -f "$sysctl" ]] && grep -q 'kernel.panic = 10' "$sysctl" && pass "kernel.panic=10 set" || fail "Missing kernel.panic"
    [[ -f "$sysctl" ]] && grep -q 'kernel.panic_on_oops = 1' "$sysctl" && pass "panic_on_oops=1" || fail "Missing panic_on_oops"
}

# ── 3.3: Rollback loop — counter resets on clean boot ─────────────────────
test_rollback_loop() {
    header "3.3 — Rollback loop: counter management"
    local rs="$SCRIPTS/hisnos-panic-rollback.sh"
    [[ -f "$rs" ]] || { fail "Missing panic-rollback.sh"; return; }
    grep -q 'PANIC_THRESHOLD=3' "$rs" && pass "Threshold is 3" || fail "Wrong threshold"
    grep -q 'PANIC_COUNT_FILE' "$rs" && pass "Counter file tracked" || fail "No counter"
    grep -q 'echo "0"' "$rs" && pass "Counter resets to 0 on clean" || fail "Counter never resets"
    grep -q 'rpm-ostree rollback' "$rs" && pass "Triggers rpm-ostree rollback" || fail "No rollback action"
}

# ── 3.4: Bad deployment GRUB marking ─────────────────────────────────────
test_bad_deploy() {
    header "3.4 — Bad deployment: GRUB env marking"
    local rs="$SCRIPTS/hisnos-panic-rollback.sh"
    [[ -f "$rs" ]] || { fail "Missing panic-rollback.sh"; return; }
    grep -q 'grub2-editenv\|hisnos_bad_deploy' "$rs" && pass "Marks bad deploy in GRUB" || fail "No GRUB marking"
}

case "${1:-all}" in
    interrupted-deploy) test_interrupted_deploy ;; power-loss) test_power_loss ;;
    rollback-loop) test_rollback_loop ;; bad-deploy) test_bad_deploy ;;
    all) test_interrupted_deploy; test_power_loss; test_rollback_loop; test_bad_deploy ;;
    *) echo "Usage: $0 {all|interrupted-deploy|power-loss|rollback-loop|bad-deploy}"; exit 1 ;;
esac

log ""; log "  UPDATE CHAOS: ✅ $PASS passed, ❌ $FAIL failed, ⏭️ $SKIP skipped"
cat > "$RESULTS_DIR/update-chaos.json" <<EOF
{"group":"update","timestamp":"$(date -u +%Y-%m-%dT%H:%M:%SZ)","pass":$PASS,"fail":$FAIL,"skip":$SKIP}
EOF
exit $FAIL
