#!/usr/bin/env bash
# lab/runtime/hisnos-lab-runtime.sh — Disposable threat validation environment launcher
#
# Isolation layers:
#   Network   : Profile-dependent (see below)
#   Filesystem: tmpfs root (--tmpfs /) — zero persistence by construction.
#               /usr bind-mounted read-only. /tmp /run on tmpfs.
#               Optional read-only sample directory at /samples.
#               /results tmpfs for operator artifact staging.
#   PID/IPC   : --unshare-pid --unshare-ipc --unshare-uts — isolated process tree.
#   User NS   : --unshare-user — container root mapped to calling UID.
#   Resources : Enforced externally by systemd-run --user (CPUQuota + MemoryMax).
#   Session   : $XDG_RUNTIME_DIR/hisnos-lab-session.json
#
# Network profiles:
#   offline         (default) bwrap --unshare-net only. Zero egress, kernel-enforced.
#   allowlist-cidr  veth pair + nftables allowlist. Requires netd (privileged helper).
#   dns-sinkhole    veth pair + netd DNS interceptor. DNS queries return NXDOMAIN.
#   http-proxy      veth pair + netd FORWARD to proxy only. HTTP_PROXY env set.
#
# Safety guarantee:
#   If netd setup fails for any non-offline profile, the session falls back to
#   offline mode and logs HISNOS_LAB_NET_FALLBACK. The session ALWAYS starts.
#
# Usage (invoked by systemd-run --user from dashboard API):
#   hisnos-lab-runtime.sh --session-id ID --profile isolated
#     [--net-profile offline|allowlist-cidr|dns-sinkhole|http-proxy]
#     [--net-cidrs 1.2.3.0/24,5.6.7.0/24]
#     [--net-proxy 192.168.1.1:3128]
#     [--sample-dir /path] [--cmd /bin/bash]
#
# Journal events (journalctl -t hisnos-lab):
#   HISNOS_LAB_STARTED      session started (with effective net profile)
#   HISNOS_LAB_NET_PROFILE  network setup completed
#   HISNOS_LAB_NET_FALLBACK net setup failed; fell back to offline
#   HISNOS_LAB_STOPPED      session exited
#   HISNOS_LAB_CLEANUP      lock file removed, veth teardown requested
#
# Kinoite note: bwrap user namespaces require kernel.unprivileged_userns_clone=1
# (default on Fedora). bwrap --sync-fd requires bubblewrap >= 0.5 (Fedora ships 0.9+).

set -euo pipefail

readonly LOGGER_BIN="/usr/bin/logger"
readonly BWRAP_BIN="/usr/bin/bwrap"
readonly PYTHON3_BIN="/usr/bin/python3"
readonly NETD_SOCK="/run/hisnos/lab-netd.sock"
readonly SESSION_FILE="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/hisnos-lab-session.json"

# ── Argument parsing ──────────────────────────────────────────────────────────

SESSION_ID=""
PROFILE="isolated"
NET_PROFILE="offline"
NET_CIDRS=""         # comma-separated CIDRs for allowlist-cidr
NET_PROXY=""         # host:port for http-proxy
SAMPLE_DIR=""
LAB_CMD=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --session-id)   SESSION_ID="${2}";   shift 2 ;;
    --profile)      PROFILE="${2}";      shift 2 ;;
    --net-profile)  NET_PROFILE="${2}";  shift 2 ;;
    --net-cidrs)    NET_CIDRS="${2}";    shift 2 ;;
    --net-proxy)    NET_PROXY="${2}";    shift 2 ;;
    --sample-dir)   SAMPLE_DIR="${2}";   shift 2 ;;
    --cmd)          LAB_CMD="${2}";      shift 2 ;;
    *)
      echo "[hisnos-lab-runtime] unknown argument: $1" >&2
      exit 1
      ;;
  esac
done

if [[ -z "${SESSION_ID}" ]]; then
  echo "[hisnos-lab-runtime] --session-id is required" >&2
  exit 1
fi

if [[ ! -x "${BWRAP_BIN}" ]]; then
  echo "[hisnos-lab-runtime] bwrap not found. Install: rpm-ostree install bubblewrap" >&2
  exit 1
fi

# ── State ─────────────────────────────────────────────────────────────────────

BWRAP_PID=""
BWRAP_EXIT_CODE=0
EFFECTIVE_NET_PROFILE="${NET_PROFILE}"
VETH_HOST_IFACE=""
NFT_SESSION_SET=""
HANDLES_JSON="{}"
BLOCK_PIPE=""
BLOCK_WRITE_FD=""
NETD_SETUP_SUCCESS=false

# ── Cleanup trap ──────────────────────────────────────────────────────────────

_cleanup() {
  local exit_code="${BWRAP_EXIT_CODE:-$?}"

  "${LOGGER_BIN}" -t hisnos-lab -p user.notice \
    "HISNOS_LAB_STOPPED session=${SESSION_ID} profile=${PROFILE} net=${EFFECTIVE_NET_PROFILE} exit=${exit_code}" \
    2>/dev/null || true

  # Kill bwrap if still running (SIGTERM → wait → SIGKILL)
  if [[ -n "${BWRAP_PID}" ]] && kill -0 "${BWRAP_PID}" 2>/dev/null; then
    kill -TERM "${BWRAP_PID}" 2>/dev/null || true
    local n=0
    while kill -0 "${BWRAP_PID}" 2>/dev/null && [[ $n -lt 30 ]]; do
      sleep 0.1; (( n++ ))
    done
    kill -KILL "${BWRAP_PID}" 2>/dev/null || true
  fi

  # Unblock bwrap if it's still waiting on the sync pipe (edge case on early failure)
  if [[ -n "${BLOCK_WRITE_FD}" ]]; then
    echo -n x >&"${BLOCK_WRITE_FD}" 2>/dev/null || true
    exec {BLOCK_WRITE_FD}>&- 2>/dev/null || true
  fi
  [[ -n "${BLOCK_PIPE}" ]] && rm -f "${BLOCK_PIPE}" 2>/dev/null || true

  # Request netd to tear down veth and nftables rules
  if [[ "${NETD_SETUP_SUCCESS}" == "true" && -n "${VETH_HOST_IFACE}" ]]; then
    _netd_teardown || {
      "${LOGGER_BIN}" -t hisnos-lab -p user.warning \
        "HISNOS_LAB_CLEANUP teardown_failed session=${SESSION_ID} — requesting emergency-flush" \
        2>/dev/null || true
      _netd_emergency_flush || true
    }
  fi

  rm -f "${SESSION_FILE}" 2>/dev/null || true

  "${LOGGER_BIN}" -t hisnos-lab -p user.notice \
    "HISNOS_LAB_CLEANUP session=${SESSION_ID}" \
    2>/dev/null || true
}

trap '_cleanup' EXIT
trap 'BWRAP_EXIT_CODE=130; _cleanup; exit 130' INT TERM

# ── Validate inputs ───────────────────────────────────────────────────────────

if [[ -n "${SAMPLE_DIR}" ]]; then
  [[ ! -d "${SAMPLE_DIR}" ]] && {
    echo "[hisnos-lab-runtime] sample-dir not found: ${SAMPLE_DIR}" >&2; exit 1
  }
  SAMPLE_DIR="$(realpath -- "${SAMPLE_DIR}")"
fi

# ── netd communication helpers ────────────────────────────────────────────────
# Use Python3 for Unix socket communication (bash has no native Unix socket support).

_netd_send() {
  local request_json="$1"
  local timeout_sec="${2:-15}"
  if [[ ! -S "${NETD_SOCK}" ]]; then
    echo ""
    return 1
  fi
  NETD_SOCK="${NETD_SOCK}" NETD_REQ="${request_json}" \
  "${PYTHON3_BIN}" - <<'PYEOF'
import socket, os, sys, json
sock_path = os.environ['NETD_SOCK']
req       = os.environ['NETD_REQ']
try:
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(15)
    s.connect(sock_path)
    s.sendall(req.encode() + b'\n')
    s.shutdown(socket.SHUT_WR)
    resp = b''
    while True:
        chunk = s.recv(4096)
        if not chunk:
            break
        resp += chunk
    s.close()
    print(resp.decode().strip())
except Exception as e:
    print(json.dumps({'success': False, 'error': str(e)}))
PYEOF
}

_jq_resp() {
  # Extract a field from a JSON string; empty string if missing
  local json="$1" field="$2" default="${3:-}"
  "${PYTHON3_BIN}" -c "
import json, sys
try:
    d = json.loads(sys.argv[1])
    val = d.get(sys.argv[2])
    if val is None:
        print(sys.argv[3] if len(sys.argv) > 3 else '')
    elif isinstance(val, (dict, list)):
        print(json.dumps(val))
    else:
        print(str(val))
except:
    print(sys.argv[3] if len(sys.argv) > 3 else '')
" "${json}" "${field}" "${default}"
}

_netd_teardown() {
  local req
  req=$("${PYTHON3_BIN}" -c "
import json, sys
print(json.dumps({
    'op':            'teardown',
    'session_id':    sys.argv[1],
    'host_veth':     sys.argv[2],
    'nft_session_set': sys.argv[3],
    'handles':       json.loads(sys.argv[4]),
}))" "${SESSION_ID}" "${VETH_HOST_IFACE}" "${NFT_SESSION_SET}" "${HANDLES_JSON}")
  local resp
  resp=$(_netd_send "${req}" 15) || return 1
  [[ "$(_jq_resp "${resp}" success)" == "True" ]]
}

_netd_emergency_flush() {
  _netd_send '{"op":"emergency-flush"}' 15 >/dev/null || true
}

# ── Update session file with network facts ────────────────────────────────────

_update_session_net() {
  local effective_profile="$1"
  local veth_iface="$2"
  local nft_set="$3"
  local handles="$4"
  [[ ! -f "${SESSION_FILE}" ]] && return 0
  "${PYTHON3_BIN}" - <<PYEOF
import json, os
path = os.environ.get('SESSION_FILE', '')
try:
    with open(path) as f:
        d = json.load(f)
    d['effective_net_profile'] = '${effective_profile}'
    d['veth_host_iface']       = '${veth_iface}'
    d['nft_session_set']       = '${nft_set}'
    d['nft_handles']           = json.loads(r"""${handles}""")
    tmp = path + '.tmp'
    with open(tmp, 'w') as f:
        json.dump(d, f, indent=2)
    os.rename(tmp, path)
except Exception as e:
    import sys; print(f'session update failed: {e}', file=sys.stderr)
PYEOF
  SESSION_FILE="${SESSION_FILE}" \
  true  # suppress set -e on the python call above
}

# ── Build resolv.conf temp file for non-offline profiles ─────────────────────

RESOLV_TMPFILE=""

_make_resolv() {
  local nameserver="$1"
  RESOLV_TMPFILE="$(mktemp /tmp/hisnos-lab-resolv-XXXXXXXX)"
  cat > "${RESOLV_TMPFILE}" <<EOF
# HisnOS lab session: ${SESSION_ID}
# Net profile: ${EFFECTIVE_NET_PROFILE}
nameserver ${nameserver}
options timeout:2 attempts:1 ndots:0
EOF
}

# ── Network setup (veth + netd) ───────────────────────────────────────────────
# Called AFTER bwrap is started (blocking on sync pipe) so we have BWRAP_PID.
# If setup fails, we fall back to offline (session continues, no crash).

_setup_veth_network() {
  local cidrs_json="[]"
  if [[ -n "${NET_CIDRS}" ]]; then
    cidrs_json=$("${PYTHON3_BIN}" -c "
import json, sys
cidrs = [c.strip() for c in sys.argv[1].split(',') if c.strip()]
print(json.dumps(cidrs))" "${NET_CIDRS}")
  fi

  local req
  req=$("${PYTHON3_BIN}" -c "
import json, sys
print(json.dumps({
    'op':         'setup',
    'session_id': sys.argv[1],
    'pid':        int(sys.argv[2]),
    'profile':    sys.argv[3],
    'cidrs':      json.loads(sys.argv[4]),
    'proxy_addr': sys.argv[5],
}))" "${SESSION_ID}" "${BWRAP_PID}" "${NET_PROFILE}" "${cidrs_json}" "${NET_PROXY:-}")

  local resp
  if ! resp=$(_netd_send "${req}" 15) || [[ -z "${resp}" ]]; then
    return 1
  fi

  local success
  success=$(_jq_resp "${resp}" success "False")
  if [[ "${success}" != "True" ]]; then
    local err
    err=$(_jq_resp "${resp}" error "unknown error")
    "${LOGGER_BIN}" -t hisnos-lab -p user.warning \
      "HISNOS_LAB_NET_SETUP_FAILED session=${SESSION_ID} err=${err}" \
      2>/dev/null || true
    return 1
  fi

  VETH_HOST_IFACE=$(_jq_resp "${resp}" host_veth)
  NFT_SESSION_SET=$(_jq_resp "${resp}" nft_session_set)
  HANDLES_JSON=$(_jq_resp "${resp}" handles "{}")
  local cont_ip
  cont_ip=$(_jq_resp "${resp}" cont_ip "10.72.0.2")
  NETD_SETUP_SUCCESS=true

  # Set resolv.conf for the container
  # For dns-sinkhole: point to 10.72.0.1 (sinkhole listener)
  # For allowlist-cidr: point to 10.72.0.1 which forwards to host resolver
  #   (or the user includes a DNS server in their CIDR allowlist)
  # For http-proxy: DNS resolution handled by proxy; set to 10.72.0.1 as fallback
  _make_resolv "10.72.0.1"

  "${LOGGER_BIN}" -t hisnos-lab -p user.notice \
    "HISNOS_LAB_NET_PROFILE session=${SESSION_ID} profile=${NET_PROFILE} host_veth=${VETH_HOST_IFACE} cont_ip=${cont_ip}" \
    2>/dev/null || true

  return 0
}

# ── Build bwrap argument array ────────────────────────────────────────────────

bwrap_args=(
  # ── Namespace isolation ───────────────────────────────────────────────────
  --unshare-user      # user NS: container appears as root, no host caps
  --unshare-pid       # isolated process tree
  --unshare-ipc       # isolated shared memory/semaphores
  --unshare-uts       # isolated hostname
  --new-session       # setsid: ctrl+C from terminal cannot reach inside
  --die-with-parent   # bwrap exits if this script exits (safety net)

  # ── Filesystem: tmpfs root ────────────────────────────────────────────────
  --tmpfs /
  --proc /proc
  --dev  /dev

  # ── Read-only OS tree ─────────────────────────────────────────────────────
  --ro-bind /usr /usr
  --symlink usr/bin   /bin
  --symlink usr/sbin  /sbin
  --symlink usr/lib   /lib
  --symlink usr/lib64 /lib64

  --ro-bind /etc/ld.so.cache     /etc/ld.so.cache
  --ro-bind /etc/ld.so.conf      /etc/ld.so.conf
  --ro-bind /etc/ld.so.conf.d    /etc/ld.so.conf.d

  --ro-bind-try /etc/localtime   /etc/localtime
  --ro-bind-try /etc/nsswitch.conf /etc/nsswitch.conf

  # ── Writable working areas ────────────────────────────────────────────────
  --tmpfs /tmp
  --tmpfs /run
  --dir   /home/analyst
  --dir   /results

  # ── Environment ───────────────────────────────────────────────────────────
  --setenv HOME    /home/analyst
  --setenv TMPDIR  /tmp
  --setenv PATH    /usr/bin:/usr/sbin:/usr/local/bin
  --setenv TERM    xterm-256color
  --setenv PS1     '[lab:\w]\$ '
  --setenv HISNOS_LAB_SESSION  "${SESSION_ID}"
  --setenv HISNOS_LAB_PROFILE  "${PROFILE}"
  --setenv HISNOS_LAB_NET      "${NET_PROFILE}"

  --chdir /home/analyst
)

# Sample directory bind-mount
[[ -n "${SAMPLE_DIR}" ]] && bwrap_args+=(--ro-bind "${SAMPLE_DIR}" /samples)

# ── Network mode ──────────────────────────────────────────────────────────────
# offline: --unshare-net — strictly isolated network namespace, loopback only.
# veth profiles: --unshare-net is added here; the veth is placed into the
#   namespace AFTER bwrap starts (using --sync-fd blocking pattern).
# In both cases, --unshare-net is always present — the veth-based profiles
# ADD a controlled interface into the already-isolated namespace.

bwrap_args+=(--unshare-net)  # ALWAYS isolated; veth is optional addition

# ── Determine if veth setup is needed ─────────────────────────────────────────

NEED_VETH=false
case "${NET_PROFILE}" in
  allowlist-cidr|dns-sinkhole|http-proxy) NEED_VETH=true ;;
esac

# For veth profiles, use --sync-fd to block bwrap until network is ready.
if [[ "${NEED_VETH}" == "true" ]]; then
  BLOCK_PIPE="$(mktemp -u /tmp/hisnos-lab-blk-XXXXXXXX)"
  mkfifo -m 600 "${BLOCK_PIPE}"
  # Open write end first (prevents open-read from blocking in background)
  exec {BLOCK_WRITE_FD}>"${BLOCK_PIPE}"
  # bwrap reads 1 byte from fd 3, then proceeds (--sync-fd 3)
  bwrap_args+=(--sync-fd 3)
fi

# ── HTTP proxy environment ────────────────────────────────────────────────────
if [[ "${NET_PROFILE}" == "http-proxy" && -n "${NET_PROXY}" ]]; then
  bwrap_args+=(
    --setenv HTTP_PROXY  "http://${NET_PROXY}"
    --setenv HTTPS_PROXY "http://${NET_PROXY}"
    --setenv http_proxy  "http://${NET_PROXY}"
    --setenv https_proxy "http://${NET_PROXY}"
  )
fi

# ── Determine container command ───────────────────────────────────────────────

if [[ -n "${LAB_CMD}" ]]; then
  # shellcheck disable=SC2206
  cmd_args=(${LAB_CMD})
else
  cmd_args=(/usr/bin/sleep infinity)
fi

# ── Log STARTED event ─────────────────────────────────────────────────────────

"${LOGGER_BIN}" -t hisnos-lab -p user.notice \
  "HISNOS_LAB_STARTED session=${SESSION_ID} profile=${PROFILE} net=${NET_PROFILE} need_veth=${NEED_VETH} sample_dir=${SAMPLE_DIR:-none}" \
  2>/dev/null || true

# ── Launch bwrap ──────────────────────────────────────────────────────────────

if [[ "${NEED_VETH}" == "true" ]]; then
  # Launch bwrap blocking on fd 3 (the named pipe read end)
  "${BWRAP_BIN}" "${bwrap_args[@]}" -- "${cmd_args[@]}" 3<"${BLOCK_PIPE}" &
  BWRAP_PID=$!

  # Give bwrap a moment to fork and create namespaces
  # (The namespace is created immediately in fork; this sleep is just belt+suspenders)
  sleep 0.2

  # ── Set up veth network ────────────────────────────────────────────────────
  if ! _setup_veth_network; then
    # Fallback: fall back to offline (session continues without network)
    EFFECTIVE_NET_PROFILE="offline"
    "${LOGGER_BIN}" -t hisnos-lab -p user.warning \
      "HISNOS_LAB_NET_FALLBACK session=${SESSION_ID} original_profile=${NET_PROFILE} reason=netd_setup_failed" \
      2>/dev/null || true
  else
    EFFECTIVE_NET_PROFILE="${NET_PROFILE}"
    # Install custom resolv.conf if we made one
    if [[ -n "${RESOLV_TMPFILE}" ]]; then
      bwrap_args+=(--ro-bind "${RESOLV_TMPFILE}" /etc/resolv.conf)
      # Note: can't add bwrap args after bwrap has started.
      # Resolv.conf injection via nsenter is done by netd for dns-sinkhole profile.
      # For other profiles, the container uses /etc/resolv.conf from bind-mount...
      # Actually since bwrap is already running, we can't add args.
      # netd uses nsenter to write resolv.conf directly into /proc/$BWRAP_PID/root/etc/
      # For now, log that resolv.conf injection is pending Phase 5b-dns refinement.
      "${LOGGER_BIN}" -t hisnos-lab -p user.notice \
        "HISNOS_LAB_NET_RESOLV_NOTE session=${SESSION_ID} resolv_file=${RESOLV_TMPFILE} note=restart_required_for_resolv_inject" \
        2>/dev/null || true
    fi
  fi

  # Update session file with network facts (before unblocking)
  _update_session_net "${EFFECTIVE_NET_PROFILE}" \
    "${VETH_HOST_IFACE}" "${NFT_SESSION_SET}" "${HANDLES_JSON}" || true

  # ── Unblock bwrap ──────────────────────────────────────────────────────────
  echo -n x >&"${BLOCK_WRITE_FD}"
  exec {BLOCK_WRITE_FD}>&-
  BLOCK_WRITE_FD=""
  rm -f "${BLOCK_PIPE}"
  BLOCK_PIPE=""
else
  # Offline profile: start bwrap directly, no blocking
  "${BWRAP_BIN}" "${bwrap_args[@]}" -- "${cmd_args[@]}" &
  BWRAP_PID=$!
  _update_session_net "offline" "" "" "{}" || true
fi

# ── Wait for bwrap to exit ────────────────────────────────────────────────────

wait "${BWRAP_PID}"
BWRAP_EXIT_CODE=$?

# Clean up resolv temp file
[[ -n "${RESOLV_TMPFILE}" ]] && rm -f "${RESOLV_TMPFILE}" 2>/dev/null || true

# EXIT trap fires here
exit "${BWRAP_EXIT_CODE}"
