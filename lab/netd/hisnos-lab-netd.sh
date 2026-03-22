#!/usr/bin/env bash
# lab/netd/hisnos-lab-netd.sh — Privileged lab network setup daemon
#
# Activation model:
#   systemd socket activation (Accept=yes, one instance per connection).
#   Stdin and stdout are the connected socket — one JSON request, one JSON response.
#   Runs as root. Capability set: CAP_NET_ADMIN only (see service unit).
#
# Protocol: newline-terminated JSON over Unix socket /run/hisnos/lab-netd.sock
#
#   Request (setup):
#     {"op":"setup","session_id":"abc123","pid":12345,
#      "profile":"allowlist-cidr","cidrs":["1.2.3.0/24"],
#      "proxy_addr":"10.0.0.1:3128","dns_sinkhole_ip":""}
#
#   Response (setup):
#     {"success":true,"host_veth":"vlh-abc123","cont_veth":"vlc-abc123",
#      "host_ip":"10.72.0.1","cont_ip":"10.72.0.2",
#      "handles":{"fwd_allow":"42","fwd_drop":"43","masq":"7","in_drop":"12"}
#      "nft_session_set":"lab_al_abc123","error":""}
#
#   Request (teardown):
#     {"op":"teardown","session_id":"abc123","host_veth":"vlh-abc123",
#      "handles":{"fwd_allow":"42","fwd_drop":"43","masq":"7","in_drop":"12"},
#      "nft_session_set":"lab_al_abc123"}
#
#   Response (teardown):
#     {"success":true,"error":""}
#
#   Request (emergency-flush):
#     {"op":"emergency-flush"}
#   Response:
#     {"success":true,"flushed_ifaces":["vlh-abc","vlh-def"],"error":""}
#
# Security model:
#   - Only CAP_NET_ADMIN is retained (set by service unit capability bounding set)
#   - NoNewPrivileges=yes in service unit — cannot escalate further
#   - Socket is mode 0660, group hisnos-lab — only group members can connect
#   - Session ID is validated to be hex-only before use in interface names
#   - All ip/nft commands use absolute paths, no shell interpolation on user data
#   - Interface names capped at IFNAMSIZ-1 (15 chars): vlh-XXXXXXXX = 12 chars ✓
#
# Kinoite note: /etc/systemd/system/ is writable on Kinoite (only /usr is
# immutable). System service units installed there survive base OS updates.

set -euo pipefail

readonly NFT_BIN="/usr/sbin/nft"
readonly IP_BIN="/usr/sbin/ip"
readonly NSENTER_BIN="/usr/bin/nsenter"
readonly LOGGER_BIN="/usr/bin/logger"
readonly SINKHOLE_SCRIPT="/etc/hisnos/lab/netd/hisnos-lab-dns-sinkhole.py"

readonly LAB_TABLE_FAMILY="inet"
readonly LAB_TABLE_NAME="hisnos"
readonly LAB_FWD_CHAIN="lab_forward"
readonly LAB_OUT_CHAIN="lab_veth_output"
readonly LAB_IN_CHAIN="lab_veth_input"
readonly LAB_POSTROUTING_CHAIN="postrouting"

# Fixed subnet for the lab veth pair (one session at a time).
# Phase 6+ with multiple sessions would use a DHCP-style allocator.
readonly LAB_HOST_IP="10.72.0.1"
readonly LAB_CONT_IP="10.72.0.2"
readonly LAB_PREFIX="30"
readonly LAB_SUBNET_CIDR="10.72.0.0/30"

# ── JSON helpers ──────────────────────────────────────────────────────────────

_log() {
  "${LOGGER_BIN}" -t hisnos-lab-netd -p daemon.notice "$*" 2>/dev/null || true
}

_log_err() {
  "${LOGGER_BIN}" -t hisnos-lab-netd -p daemon.err "$*" 2>/dev/null || true
}

respond_ok() {
  # $1: JSON string (already formatted)
  printf '%s\n' "$1"
}

respond_error() {
  local msg="${1//\"/\\\"}"  # escape double quotes
  _log_err "netd error: ${msg}"
  printf '{"success":false,"error":"%s"}\n' "${msg}"
}

# Extract one field from the request JSON using python3.
# Usage: _jq <json_string> <field_name> [default]
_jq() {
  python3 -c "
import json, sys
try:
    d = json.loads(sys.argv[1])
    val = d.get(sys.argv[2], sys.argv[3] if len(sys.argv) > 3 else '')
    print(str(val) if not isinstance(val, (list, dict)) else json.dumps(val))
except:
    print(sys.argv[3] if len(sys.argv) > 3 else '')
" "$1" "$2" "${3:-}"
}

# Add an nft rule and return its handle number.
# Usage: _nft_add_rule_get_handle <family> <table> <chain> <rule...>
_nft_add_rule_get_handle() {
  local family="$1" table="$2" chain="$3"
  shift 3
  local out
  out=$("${NFT_BIN}" -e add rule "${family}" "${table}" "${chain}" "$@" 2>&1) || {
    _log_err "nft add rule failed: ${out}"
    echo ""
    return 1
  }
  echo "${out}" | grep -oP '(?<=# handle )\d+' || echo ""
}

# ── Determine host uplink interface ──────────────────────────────────────────

_get_uplink() {
  "${IP_BIN}" route show default 2>/dev/null \
    | awk '/^default/ { for(i=1;i<=NF;i++) if($i=="dev") { print $(i+1); exit } }'
}

# ── Setup operation ───────────────────────────────────────────────────────────

do_setup() {
  local req="$1"
  local sid pid profile cidrs_json proxy_addr dns_sinkhole_ip

  sid=$(_jq "${req}" "session_id")
  pid=$(_jq "${req}" "pid")
  profile=$(_jq "${req}" "profile" "offline")
  cidrs_json=$(_jq "${req}" "cidrs" "[]")
  proxy_addr=$(_jq "${req}" "proxy_addr")
  dns_sinkhole_ip=$(_jq "${req}" "dns_sinkhole_ip" "${LAB_HOST_IP}")

  # Validate session ID: hex chars only, 1–16 chars
  if [[ ! "${sid}" =~ ^[a-f0-9]{1,16}$ ]]; then
    respond_error "invalid session_id (must be hex)"
    return
  fi

  # Validate PID
  if [[ ! "${pid}" =~ ^[0-9]+$ ]] || [[ "${pid}" -le 1 ]]; then
    respond_error "invalid pid: ${pid}"
    return
  fi

  # Check PID exists and has a network namespace
  if [[ ! -L "/proc/${pid}/ns/net" ]]; then
    respond_error "pid ${pid} not found or has no network namespace"
    return
  fi

  local sid_short="${sid:0:8}"
  local host_veth="vlh-${sid_short}"   # max 12 chars, within IFNAMSIZ-1
  local cont_veth="vlc-${sid_short}"
  local sess_set="lab_al_${sid_short}" # nftables set name for this session's allowlist
  local ns_path="/proc/${pid}/ns/net"

  _log "HISNOS_LAB_NET_SETUP session=${sid} profile=${profile} pid=${pid}"

  # ── Step 1: create veth pair ──────────────────────────────────────────────
  if "${IP_BIN}" link show "${host_veth}" &>/dev/null; then
    "${IP_BIN}" link delete "${host_veth}" 2>/dev/null || true
  fi
  if ! "${IP_BIN}" link add "${host_veth}" type veth peer name "${cont_veth}" 2>&1; then
    respond_error "failed to create veth pair ${host_veth}/${cont_veth}"
    return
  fi

  # ── Step 2: move container end into the bwrap network namespace ───────────
  if ! "${IP_BIN}" link set "${cont_veth}" netns "${pid}" 2>&1; then
    "${IP_BIN}" link delete "${host_veth}" 2>/dev/null || true
    respond_error "failed to move ${cont_veth} into namespace of pid ${pid}"
    return
  fi

  # ── Step 3: configure host-side veth ──────────────────────────────────────
  "${IP_BIN}" link set "${host_veth}" up
  "${IP_BIN}" addr add "${LAB_HOST_IP}/${LAB_PREFIX}" dev "${host_veth}"

  # ── Step 4: configure container-side (nsenter as root into net namespace) ──
  "${NSENTER_BIN}" --net="${ns_path}" -- "${IP_BIN}" link set lo up
  "${NSENTER_BIN}" --net="${ns_path}" -- "${IP_BIN}" link set "${cont_veth}" up
  "${NSENTER_BIN}" --net="${ns_path}" -- \
    "${IP_BIN}" addr add "${LAB_CONT_IP}/${LAB_PREFIX}" dev "${cont_veth}"
  "${NSENTER_BIN}" --net="${ns_path}" -- \
    "${IP_BIN}" route add default via "${LAB_HOST_IP}" dev "${cont_veth}"

  # ── Step 5: enable IP forwarding (required for container→internet routing) ─
  echo 1 > /proc/sys/net/ipv4/ip_forward

  # ── Step 6: nftables — create session-specific allowlist set ──────────────
  if ! "${NFT_BIN}" add set "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" "${sess_set}" \
      "{ type ipv4_addr; flags interval; }" 2>/dev/null; then
    # Set may already exist from a previous crashed session; flush and reuse
    "${NFT_BIN}" flush set "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" "${sess_set}" 2>/dev/null || true
  fi

  # Populate allowlist CIDRs if provided
  if [[ "${cidrs_json}" != "[]" && -n "${cidrs_json}" ]]; then
    local cidrs_nft
    cidrs_nft=$(python3 -c "
import json, sys
cidrs = json.loads(sys.argv[1])
print('{ ' + ', '.join(str(c) for c in cidrs if c) + ' }') if cidrs else print('')
" "${cidrs_json}")
    if [[ -n "${cidrs_nft}" ]]; then
      "${NFT_BIN}" add element "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" "${sess_set}" \
        "${cidrs_nft}" 2>/dev/null || _log_err "CIDR injection failed for ${sid}"
    fi
  fi

  # ── Step 7: profile-specific nftables rules ────────────────────────────────
  # All rules are tagged with session context for handle-based cleanup.
  local uplink
  uplink=$(_get_uplink)

  local h_fwd_allow="" h_fwd_drop="" h_masq="" h_in_drop="" h_in_dns=""

  case "${profile}" in
    allowlist-cidr)
      # FORWARD: allow established + allowlisted CIDRs; drop rest for this veth
      h_fwd_allow=$(_nft_add_rule_get_handle \
        "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" "${LAB_FWD_CHAIN}" \
        iif "${host_veth}" oif "${uplink}" \
        ip daddr "@${sess_set}" ct state new,established,related accept) || true
      h_fwd_drop=$(_nft_add_rule_get_handle \
        "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" "${LAB_FWD_CHAIN}" \
        iif "${host_veth}" drop) || true
      # NAT masquerade for lab subnet via uplink
      h_masq=$(_nft_add_rule_get_handle \
        "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" "${LAB_POSTROUTING_CHAIN}" \
        ip saddr "${LAB_SUBNET_CIDR}" oif "${uplink}" masquerade) || true
      # INPUT: block container→host (protect host services)
      h_in_drop=$(_nft_add_rule_get_handle \
        "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" input \
        iif "${host_veth}" drop) || true
      ;;

    dns-sinkhole)
      # INPUT: allow DNS queries from container to host veth IP
      h_in_dns=$(_nft_add_rule_get_handle \
        "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" input \
        iif "${host_veth}" ip daddr "${LAB_HOST_IP}" udp dport 53 accept) || true
      _nft_add_rule_get_handle \
        "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" input \
        iif "${host_veth}" ip daddr "${LAB_HOST_IP}" tcp dport 53 accept >/dev/null 2>&1 || true
      # FORWARD: drop all container→internet (no outbound)
      h_fwd_drop=$(_nft_add_rule_get_handle \
        "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" "${LAB_FWD_CHAIN}" \
        iif "${host_veth}" drop) || true
      # INPUT: drop all other container→host
      h_in_drop=$(_nft_add_rule_get_handle \
        "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" input \
        iif "${host_veth}" drop) || true
      # Start DNS sinkhole on host veth IP
      if [[ -x "${SINKHOLE_SCRIPT}" ]]; then
        python3 "${SINKHOLE_SCRIPT}" --bind "${dns_sinkhole_ip}" --sid "${sid}" &
        _log "DNS sinkhole started on ${dns_sinkhole_ip}:53 for session ${sid}"
      else
        _log_err "dns-sinkhole: sinkhole script not found at ${SINKHOLE_SCRIPT} — DNS will time out"
      fi
      ;;

    http-proxy)
      if [[ -z "${proxy_addr}" ]]; then
        respond_error "http-proxy profile requires proxy_addr"
        # Roll back veth
        "${IP_BIN}" link delete "${host_veth}" 2>/dev/null || true
        return
      fi
      local proxy_ip="${proxy_addr%:*}"
      local proxy_port="${proxy_addr##*:}"
      # FORWARD: allow traffic to proxy host only
      h_fwd_allow=$(_nft_add_rule_get_handle \
        "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" "${LAB_FWD_CHAIN}" \
        iif "${host_veth}" oif "${uplink}" \
        ip daddr "${proxy_ip}" tcp dport "${proxy_port}" ct state new,established,related accept) || true
      h_fwd_drop=$(_nft_add_rule_get_handle \
        "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" "${LAB_FWD_CHAIN}" \
        iif "${host_veth}" drop) || true
      h_masq=$(_nft_add_rule_get_handle \
        "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" "${LAB_POSTROUTING_CHAIN}" \
        ip saddr "${LAB_SUBNET_CIDR}" oif "${uplink}" masquerade) || true
      h_in_drop=$(_nft_add_rule_get_handle \
        "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" input \
        iif "${host_veth}" drop) || true
      ;;

    offline|*)
      # offline: no nftables rules needed; isolation is the network namespace itself.
      # The veth pair is NOT created for offline — this case is handled by the caller.
      # If we're here with offline profile, something is wrong; return error.
      respond_error "offline profile does not require netd setup — caller should not invoke setup for offline"
      "${IP_BIN}" link delete "${host_veth}" 2>/dev/null || true
      return
      ;;
  esac

  _log "HISNOS_LAB_NET_PROFILE session=${sid} profile=${profile} host_veth=${host_veth} uplink=${uplink}"

  # ── Step 8: return success with all teardown info ─────────────────────────
  python3 -c "
import json, sys
print(json.dumps({
  'success':       True,
  'host_veth':     sys.argv[1],
  'cont_veth':     sys.argv[2],
  'host_ip':       sys.argv[3],
  'cont_ip':       sys.argv[4],
  'subnet_cidr':   sys.argv[5],
  'nft_session_set': sys.argv[6],
  'handles': {
    'fwd_allow': sys.argv[7],
    'fwd_drop':  sys.argv[8],
    'masq':      sys.argv[9],
    'in_drop':   sys.argv[10],
    'in_dns':    sys.argv[11],
  },
  'error': ''
}))" \
  "${host_veth}" "${cont_veth}" \
  "${LAB_HOST_IP}" "${LAB_CONT_IP}" "${LAB_SUBNET_CIDR}" \
  "${sess_set}" \
  "${h_fwd_allow}" "${h_fwd_drop}" "${h_masq}" "${h_in_drop}" "${h_in_dns:-}"
}

# ── Teardown operation ────────────────────────────────────────────────────────

do_teardown() {
  local req="$1"
  local sid host_veth sess_set handles_json

  sid=$(_jq "${req}" "session_id")
  host_veth=$(_jq "${req}" "host_veth")
  sess_set=$(_jq "${req}" "nft_session_set")
  handles_json=$(_jq "${req}" "handles" "{}")

  _log "HISNOS_LAB_NET_TEARDOWN session=${sid} host_veth=${host_veth}"

  local errors=()

  # ── Delete nftables rules by handle (most precise cleanup) ────────────────
  if [[ -n "${handles_json}" && "${handles_json}" != "{}" ]]; then
    python3 -c "
import json, sys
handles = json.loads(sys.argv[1])
for name, h in handles.items():
    if h:
        print(h)
" "${handles_json}" | while read -r handle; do
      [[ -z "${handle}" ]] && continue
      # Try each chain where we might have added rules
      for chain in "${LAB_FWD_CHAIN}" "${LAB_POSTROUTING_CHAIN}" input output; do
        "${NFT_BIN}" delete rule "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" \
          "${chain}" handle "${handle}" 2>/dev/null && break || true
      done
    done
  fi

  # ── Delete session allowlist set ──────────────────────────────────────────
  if [[ -n "${sess_set}" ]]; then
    "${NFT_BIN}" flush  set "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" "${sess_set}" 2>/dev/null || true
    "${NFT_BIN}" delete set "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" "${sess_set}" 2>/dev/null || true
  fi

  # ── Kill DNS sinkhole if running ──────────────────────────────────────────
  if [[ -n "${sid}" ]]; then
    pkill -f "hisnos-lab-dns-sinkhole.*--sid.*${sid}" 2>/dev/null || true
  fi

  # ── Delete host veth (also removes route and addr; releases container end) ─
  if [[ -n "${host_veth}" ]] && "${IP_BIN}" link show "${host_veth}" &>/dev/null; then
    if ! "${IP_BIN}" link delete "${host_veth}" 2>&1; then
      errors+=("veth delete failed for ${host_veth}")
      _log_err "failed to delete veth ${host_veth}"
    fi
  fi

  if [[ ${#errors[@]} -gt 0 ]]; then
    respond_error "teardown partial: ${errors[*]}"
  else
    _log "HISNOS_LAB_NET_CLEANUP session=${sid}"
    printf '{"success":true,"error":""}\n'
  fi
}

# ── Emergency flush ───────────────────────────────────────────────────────────
# Called when the runtime detects a stale session or netd teardown failed.
# Brute-forces removal of ALL lab veth interfaces and flushes lab chains.

do_emergency_flush() {
  _log_err "HISNOS_LAB_EMERGENCY_FLUSH executing"

  local flushed=()

  # Delete all lab veth interfaces (vlh-* and vlc-*)
  while read -r iface; do
    [[ -z "${iface}" ]] && continue
    if "${IP_BIN}" link delete "${iface}" 2>/dev/null; then
      flushed+=("${iface}")
    fi
  done < <("${IP_BIN}" link show | awk -F': ' '/^[0-9]+: vl[hc]-/ {print $2}')

  # Flush lab chains in nftables
  "${NFT_BIN}" flush chain "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" "${LAB_FWD_CHAIN}"    2>/dev/null || true
  "${NFT_BIN}" flush chain "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" "${LAB_OUT_CHAIN}"    2>/dev/null || true
  "${NFT_BIN}" flush chain "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" "${LAB_IN_CHAIN}"     2>/dev/null || true

  # Flush the postrouting masquerade rules for lab subnet
  # (Cannot flush selectively by handle here — flush entire chain if input/fwd are clean)
  # Note: postrouting chain may have KVM rules; we only remove lab subnet masquerades
  while read -r handle; do
    [[ -z "${handle}" ]] && continue
    "${NFT_BIN}" delete rule "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" \
      "${LAB_POSTROUTING_CHAIN}" handle "${handle}" 2>/dev/null || true
  done < <("${NFT_BIN}" -e list chain "${LAB_TABLE_FAMILY}" "${LAB_TABLE_NAME}" \
    "${LAB_POSTROUTING_CHAIN}" 2>/dev/null \
    | awk '/10\.72\.0\.0\/30.*masquerade/ { match($0, /handle ([0-9]+)/, a); if(a[1]) print a[1] }')

  # Kill any running DNS sinkholes
  pkill -f "hisnos-lab-dns-sinkhole" 2>/dev/null || true

  _log "HISNOS_LAB_EMERGENCY_FLUSH complete flushed_ifaces=${flushed[*]:-none}"

  python3 -c "
import json, sys
flushed = sys.argv[1:]
print(json.dumps({'success': True, 'flushed_ifaces': flushed, 'error': ''}))" "${flushed[@]:-}"
}

# ── Main ──────────────────────────────────────────────────────────────────────
# Read the full request from stdin (socket send EOF on shutdown)

request=$(cat)

if [[ -z "${request}" ]]; then
  respond_error "empty request"
  exit 0
fi

op=$(_jq "${request}" "op")

case "${op}" in
  setup)           do_setup           "${request}" ;;
  teardown)        do_teardown        "${request}" ;;
  emergency-flush) do_emergency_flush ;;
  *)               respond_error "unknown op: ${op}" ;;
esac
