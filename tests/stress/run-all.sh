#!/usr/bin/env bash
# tests/stress/run-all.sh — HisnOS Stress Test Suite Aggregator
#
# Runs all stress tests in sequence, collects machine-readable JSON results,
# and emits a summary JSON array to stdout.
#
# Usage:
#   ./run-all.sh [--suite <name>] [--json-only] [--no-color]
#
# Flags:
#   --suite <name>    run only named suite (firewall, lab-ns, vault, audit, update, suspend)
#   --json-only       suppress all progress output (stderr quiet); emit only JSON to stdout
#   --no-color        disable ANSI colors in progress output
#
# Output format (stdout):
#   {
#     "run_id": "<uuid>",
#     "timestamp": "<RFC3339>",
#     "host": "<hostname>",
#     "results": [ {per-test result objects} ... ],
#     "summary": {
#       "total": N,
#       "pass": N, "fail": N, "warn": N, "skip": N,
#       "overall_score": 0-100,
#       "suites": { "<suite>": { "pass":N, "fail":N, "score":N } }
#     }
#   }
#
# Exit code:
#   0 — all tests passed or skipped
#   1 — at least one test failed or warned
#   2 — suite runner error

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

# ── Argument parsing ──────────────────────────────────────────────────────────

TARGET_SUITE=""
JSON_ONLY=false
NO_COLOR=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --suite)
            TARGET_SUITE="${2:-}"
            shift 2
            ;;
        --json-only)
            JSON_ONLY=true
            shift
            ;;
        --no-color)
            NO_COLOR=true
            shift
            ;;
        -h|--help)
            sed -n '/^# Usage:/,/^[^#]/p' "$0" | grep "^#" | sed 's/^# \?//'
            exit 0
            ;;
        *)
            echo "Unknown flag: $1" >&2
            exit 2
            ;;
    esac
done

if [[ "${JSON_ONLY}" == "true" ]]; then
    # Redirect all stderr to /dev/null
    exec 2>/dev/null
fi

if [[ "${NO_COLOR}" == "true" ]]; then
    RED=""; GREEN=""; YELLOW=""; NC=""
fi

# ── Suite registry ────────────────────────────────────────────────────────────

declare -A SUITES
SUITES["firewall"]="${SCRIPT_DIR}/stress-firewall.sh"
SUITES["lab-ns"]="${SCRIPT_DIR}/stress-lab-ns.sh"
SUITES["vault"]="${SCRIPT_DIR}/stress-vault.sh"
SUITES["audit"]="${SCRIPT_DIR}/stress-audit.sh"
SUITES["update"]="${SCRIPT_DIR}/stress-update.sh"
SUITES["suspend"]="${SCRIPT_DIR}/stress-suspend.sh"

SUITE_ORDER=("firewall" "lab-ns" "vault" "audit" "update" "suspend")

# ── Helpers ───────────────────────────────────────────────────────────────────

generate_run_id() {
    # Generate a pseudo-UUID from /proc/sys/kernel/random/uuid or date+PID
    if [[ -r /proc/sys/kernel/random/uuid ]]; then
        cat /proc/sys/kernel/random/uuid
    else
        printf '%08x-%04x-4%03x-%04x-%012x' \
            "$(( RANDOM * RANDOM ))" "$(( RANDOM ))" "$(( RANDOM & 0xfff ))" \
            "$(( (RANDOM & 0x3fff) | 0x8000 ))" "$(( RANDOM * RANDOM * RANDOM ))"
    fi
}

run_suite() {
    local suite_name="$1"
    local script_path="$2"

    if [[ ! -x "${script_path}" ]]; then
        warn "Suite '${suite_name}': script not executable or not found: ${script_path}"
        printf '{"test":"%s_suite","status":"skip","score":0,"timestamp":"%s","details":{"reason":"script not found: %s"}}\n' \
            "${suite_name}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${script_path}"
        return
    fi

    info "Running suite: ${suite_name}"
    # Capture stdout (JSON results) only; stderr passes through to terminal
    bash "${script_path}" 2>&1 1>&3
}

# ── Main ──────────────────────────────────────────────────────────────────────

RUN_ID=$(generate_run_id)
RUN_TS=$(date -u +%Y-%m-%dT%H:%M:%SZ)
HOST=$(hostname -s 2>/dev/null || echo "unknown")

sep "HisnOS Stress Test Suite — run_id=${RUN_ID}"
info "Host: ${HOST} | Timestamp: ${RUN_TS}"

# Collect all result JSON lines into a temp file
RESULTS_TMP=$(mktemp /tmp/hisnos-stress-XXXXXX.jsonl)
trap 'rm -f "${RESULTS_TMP}"' EXIT

# Redirect stdout (JSON) to temp file, keep fd3 as original stdout
exec 3>&1 1>"${RESULTS_TMP}"

# Determine which suites to run
run_suites=()
if [[ -n "${TARGET_SUITE}" ]]; then
    if [[ -z "${SUITES[${TARGET_SUITE}]+_}" ]]; then
        echo "Unknown suite: ${TARGET_SUITE}. Available: ${SUITE_ORDER[*]}" >&2
        exit 2
    fi
    run_suites=("${TARGET_SUITE}")
else
    run_suites=("${SUITE_ORDER[@]}")
fi

# Run selected suites
for suite in "${run_suites[@]}"; do
    run_suite "${suite}" "${SUITES[${suite}]}"
done

# Restore stdout
exec 1>&3 3>&-

# ── Aggregate results ─────────────────────────────────────────────────────────

# Parse results with python3 (available on Fedora)
python3 - "${RESULTS_TMP}" "${RUN_ID}" "${RUN_TS}" "${HOST}" <<'EOF'
import sys, json

results_file = sys.argv[1]
run_id = sys.argv[2]
run_ts = sys.argv[3]
host = sys.argv[4]

results = []
with open(results_file) as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        try:
            results.append(json.loads(line))
        except json.JSONDecodeError:
            pass  # skip unparseable lines

# Tally summary
total = len(results)
counts = {"pass": 0, "fail": 0, "warn": 0, "skip": 0}
score_sum = 0
score_count = 0

suites = {}

for r in results:
    status = r.get("status", "skip")
    counts[status] = counts.get(status, 0) + 1

    score = r.get("score", 0)
    if status in ("pass", "fail", "warn"):
        score_sum += score
        score_count += 1

    # Group by suite (test name prefix before first underscore)
    test = r.get("test", "")
    parts = test.rsplit("_", 1)
    # Use test name prefix to determine suite
    suite_key = "misc"
    if test.startswith("blocked_") or test.startswith("rule_") or \
       test.startswith("gaming_chain_") or test.startswith("conntrack_"):
        suite_key = "firewall"
    elif test.startswith("ns_") or test.startswith("bwrap_") or test.startswith("veth_"):
        suite_key = "lab-ns"
    elif test.startswith("vault_"):
        suite_key = "vault"
    elif test.startswith("logd_") or test.startswith("audit_") or test.startswith("auditd_"):
        suite_key = "audit"
    elif test.startswith("preflight_") or test.startswith("update_") or \
         test.startswith("ostree_") or test.startswith("rollback_"):
        suite_key = "update"
    elif test.startswith("pre_suspend_") or test.startswith("suspend_") or \
         test.startswith("post_suspend_") or test.startswith("network_resume_"):
        suite_key = "suspend"

    if suite_key not in suites:
        suites[suite_key] = {"pass": 0, "fail": 0, "warn": 0, "skip": 0, "score_sum": 0, "score_count": 0}
    suites[suite_key][status] = suites[suite_key].get(status, 0) + 1
    if status in ("pass", "fail", "warn"):
        suites[suite_key]["score_sum"] += score
        suites[suite_key]["score_count"] += 1

overall_score = round(score_sum / score_count) if score_count > 0 else 0

suite_summary = {}
for k, v in suites.items():
    sc = round(v["score_sum"] / v["score_count"]) if v["score_count"] > 0 else 0
    suite_summary[k] = {
        "pass": v["pass"],
        "fail": v["fail"],
        "warn": v["warn"],
        "skip": v["skip"],
        "score": sc
    }

output = {
    "run_id": run_id,
    "timestamp": run_ts,
    "host": host,
    "results": results,
    "summary": {
        "total": total,
        "pass": counts.get("pass", 0),
        "fail": counts.get("fail", 0),
        "warn": counts.get("warn", 0),
        "skip": counts.get("skip", 0),
        "overall_score": overall_score,
        "suites": suite_summary
    }
}

print(json.dumps(output, indent=2))
EOF

# Print human-readable summary to stderr
python3 - "${RESULTS_TMP}" >&2 <<'PYEOF'
import sys, json

results = []
with open(sys.argv[1]) as f:
    for line in f:
        line = line.strip()
        if line:
            try:
                results.append(json.loads(line))
            except Exception:
                pass

print("\n── Test Results Summary ──", file=sys.stderr)
for r in results:
    status = r.get("status","?").upper()
    score = r.get("score", 0)
    test = r.get("test","?")
    sym = {"PASS": "✓", "FAIL": "✗", "WARN": "!", "SKIP": "~"}.get(status, "?")
    print(f"  [{sym}] {status:4s}  score={score:3d}  {test}", file=sys.stderr)

counts = {}
for r in results:
    s = r.get("status","skip")
    counts[s] = counts.get(s,0)+1

passed = counts.get("pass",0)
failed = counts.get("fail",0)
warned = counts.get("warn",0)
skipped = counts.get("skip",0)
total = len(results)

print(f"\n  Total: {total}  Pass: {passed}  Fail: {failed}  Warn: {warned}  Skip: {skipped}", file=sys.stderr)
PYEOF

# Exit with failure if any test failed
if grep -q '"status":"fail"' "${RESULTS_TMP}" 2>/dev/null; then
    exit 1
elif grep -q '"status":"warn"' "${RESULTS_TMP}" 2>/dev/null; then
    exit 1
fi
exit 0
