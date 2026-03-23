#!/usr/bin/env bash
# hisnos-perf-apply — Re-apply the persisted performance profile via hisnosd IPC.
# Installed to /usr/local/bin/hisnos-perf-apply by bootstrap step 15.
# Called by hisnos-performance.service on session start.
set -euo pipefail

SOCKET="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/hisnosd.sock"
STATE_FILE="/var/lib/hisnos/perf-state.json"

# If no persisted state, default to balanced.
if [[ ! -f "${STATE_FILE}" ]]; then
    echo "[hisnos-perf] no state file — defaulting to balanced" | systemd-cat -t hisnos-perf -p info
    exit 0
fi

PROFILE=$(python3 -c "import json,sys; d=json.load(open('${STATE_FILE}')); print(d.get('active_profile','balanced'))" 2>/dev/null || echo "balanced")
echo "[hisnos-perf] re-applying profile=${PROFILE}" | systemd-cat -t hisnos-perf -p info

# Send IPC command to hisnosd.
RESPONSE=$(echo "{\"id\":\"boot-apply\",\"command\":\"set_performance_profile\",\"params\":{\"mode\":\"${PROFILE}\"}}" \
    | socat - "UNIX-CONNECT:${SOCKET}" 2>/dev/null || true)

if echo "${RESPONSE}" | python3 -c "import json,sys; d=json.load(sys.stdin); sys.exit(0 if d.get('ok') else 1)" 2>/dev/null; then
    echo "[hisnos-perf] profile=${PROFILE} applied successfully" | systemd-cat -t hisnos-perf -p info
else
    echo "[hisnos-perf] WARN: profile apply returned error; check hisnosd logs" | systemd-cat -t hisnos-perf -p warning
    exit 1
fi
