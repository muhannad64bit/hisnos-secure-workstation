#!/usr/bin/env bash
# hisnos-observe-report.sh — 24h observe-mode telemetry analysis
#
# Parses journald for HISNOS-OBS-* kernel log entries and produces a
# structured report showing what traffic your workstation generates that
# is NOT covered by the current allowlists.
#
# This report is the primary input for allowlist refinement before switching
# to enforcement mode. Run after 24-48h of observe-mode operation.
#
# Usage:
#   ./hisnos-observe-report.sh                  # last 24h
#   ./hisnos-observe-report.sh --since 48h      # last 48h
#   ./hisnos-observe-report.sh --since 2026-03-19  # since date
#   ./hisnos-observe-report.sh --format cidr    # output CIDR candidates only
#   ./hisnos-observe-report.sh --format nft     # output ready-to-paste nft elements
#
# Output sections:
#   1. Summary (counts by log category)
#   2. Top outbound destinations that would hit OpenSnitch (need rules)
#   3. Top outbound destinations that would be dropped (not in allowlist + no OpenSnitch)
#   4. Unexpected inbound (port scan / unsolicited connections)
#   5. Allowlist hit rate (how much traffic is kernel-covered vs OpenSnitch)
#   6. Actionable recommendations
#
# Observe mode must have been active during the collection window.
# Check: sudo hisnos-egress status

set -euo pipefail

SINCE="24h"
FORMAT="report"

for i in "$@"; do
    case "${i}" in
        --since)     shift; SINCE="${1:-24h}"; shift ;;
        --since=*)   SINCE="${i#--since=}" ;;
        --format)    shift; FORMAT="${1:-report}"; shift ;;
        --format=*)  FORMAT="${i#--format=}" ;;
    esac
done 2>/dev/null || true

# Re-parse properly
args=("$@")
i=0
while [[ ${i} -lt ${#args[@]} ]]; do
    case "${args[$i]}" in
        --since)   SINCE="${args[$((i+1))]}"; i=$(( i + 2 )) ;;
        --since=*) SINCE="${args[$i]#--since=}"; i=$(( i + 1 )) ;;
        --format)  FORMAT="${args[$((i+1))]}"; i=$(( i + 2 )) ;;
        --format=*)FORMAT="${args[$i]#--format=}"; i=$(( i + 1 )) ;;
        *)         i=$(( i + 1 )) ;;
    esac
done

BOLD='\033[1m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'
DIM='\033[2m'; NC='\033[0m'

section() { echo -e "\n${BOLD}── $* ──${NC}"; }
info()    { echo -e "  ${GREEN}●${NC} $*"; }
warn()    { echo -e "  ${YELLOW}!${NC} $*"; }

# Build journalctl time arg
if [[ "${SINCE}" =~ ^[0-9]+h$ ]]; then
    JCTL_SINCE="--since=-${SINCE}"
elif [[ "${SINCE}" =~ ^[0-9]+-[0-9]+-[0-9]+$ ]]; then
    JCTL_SINCE="--since=${SINCE}"
else
    JCTL_SINCE="--since=-24h"
fi

# Pull all HISNOS-OBS entries from kernel log
JCTL_BASE="journalctl -k --no-pager ${JCTL_SINCE}"

# Counts
COUNT_ALLOW=$(    eval "${JCTL_BASE}" -g "HISNOS-OBS-ALLOW"          2>/dev/null | wc -l || echo 0)
COUNT_QUEUE=$(    eval "${JCTL_BASE}" -g "HISNOS-OBS-WOULD-QUEUE"     2>/dev/null | wc -l || echo 0)
COUNT_DROP_IN=$(  eval "${JCTL_BASE}" -g "HISNOS-OBS-WOULD-DROP-IN"   2>/dev/null | wc -l || echo 0)
COUNT_DROP_FWD=$( eval "${JCTL_BASE}" -g "HISNOS-OBS-WOULD-DROP-FWD"  2>/dev/null | wc -l || echo 0)

TOTAL=$(( COUNT_ALLOW + COUNT_QUEUE + COUNT_DROP_IN + COUNT_DROP_FWD ))

# ── CIDR format: just print candidate IPs ────────────────────────────────────
if [[ "${FORMAT}" == "cidr" ]]; then
    eval "${JCTL_BASE}" -g "HISNOS-OBS-WOULD-QUEUE" 2>/dev/null \
        | grep -oE "DST=[0-9.]+" | sed 's/DST=//' | sort -u
    exit 0
fi

# ── NFT format: ready-to-paste nft add element commands ──────────────────────
if [[ "${FORMAT}" == "nft" ]]; then
    echo "# CIDR candidates from observe mode — review before adding"
    echo "# Add to egress/nftables/hisnos-updates.nft or a custom set"
    echo ""
    QUEUE_IPS=$(eval "${JCTL_BASE}" -g "HISNOS-OBS-WOULD-QUEUE" 2>/dev/null \
        | grep -oE "DST=[0-9.]+" | sed 's/DST=//' | sort -u | tr '\n' ',' | sed 's/,$//')
    [[ -n "${QUEUE_IPS}" ]] && echo "# add element inet hisnos <your_set> { ${QUEUE_IPS} }"
    exit 0
fi

# ── Full report ───────────────────────────────────────────────────────────────
echo -e "${BOLD}HisnOS Observe-Mode Telemetry Report${NC}"
echo -e "${DIM}Window: last ${SINCE} | $(date)${NC}"
echo ""

# ── Section 1: Summary ───────────────────────────────────────────────────────
section "1. Traffic Summary"
if [[ ${TOTAL} -eq 0 ]]; then
    warn "No HISNOS-OBS log entries found."
    warn "Ensure observe mode was active: sudo hisnos-egress status"
    warn "Check: journalctl -k -g 'HISNOS' --since=-1h"
    exit 0
fi

info "Total logged events:              ${TOTAL}"
info "Kernel allowlist (ALLOW):         ${COUNT_ALLOW}  (DNS, NTP, Fedora, Flatpak)"
info "Would-queue (OpenSnitch needed):  ${COUNT_QUEUE}"
info "Would-drop inbound (unexpected):  ${COUNT_DROP_IN}"
info "Would-drop forward (lab):         ${COUNT_DROP_FWD}"
echo ""

if [[ ${TOTAL} -gt 0 ]]; then
    ALLOW_PCT=$(( COUNT_ALLOW * 100 / TOTAL ))
    QUEUE_PCT=$(( COUNT_QUEUE * 100 / TOTAL ))
    echo -e "  Coverage: ${ALLOW_PCT}% kernel-allowlisted | ${QUEUE_PCT}% needs OpenSnitch"
fi

# ── Section 2: Top outbound destinations → OpenSnitch ────────────────────────
section "2. Top Outbound → OpenSnitch (would-queue)"
echo -e "  ${DIM}These need OpenSnitch rules or allowlist entries.${NC}"
echo -e "  ${DIM}Format: count DST=ip DPT=port${NC}"
echo ""
eval "${JCTL_BASE}" -g "HISNOS-OBS-WOULD-QUEUE" 2>/dev/null \
    | grep -oE "DST=[0-9.]+ .*DPT=[0-9]+" \
    | sort | uniq -c | sort -rn | head 25 \
    | while read -r cnt rest; do
        printf "  %5d  %s\n" "${cnt}" "${rest}"
    done || echo "  (no entries)"

# ── Section 3: Top destinations by port (to identify app patterns) ────────────
section "3. Outbound Port Distribution (would-queue)"
echo ""
eval "${JCTL_BASE}" -g "HISNOS-OBS-WOULD-QUEUE" 2>/dev/null \
    | grep -oE "DPT=[0-9]+ " | sort | uniq -c | sort -rn | head 15 \
    | while read -r cnt port; do
        port_num="${port#DPT=}"
        # Label common ports
        label=""
        case "${port_num% }" in
            443)  label="HTTPS" ;;
            80)   label="HTTP" ;;
            22)   label="SSH" ;;
            8080|8443) label="alt-HTTP/S" ;;
            5228|5229|5230) label="Google-FCM" ;;
            1935) label="RTMP-streaming" ;;
            3478|3479) label="STUN-WebRTC" ;;
            27015*|27036) label="Steam" ;;
        esac
        printf "  %5d  port %-6s %s\n" "${cnt}" "${port_num% }" "${label}"
    done || echo "  (no entries)"

# ── Section 4: Unexpected inbound ────────────────────────────────────────────
section "4. Unexpected Inbound (would-drop)"
echo -e "  ${DIM}Source IPs sending unsolicited inbound connections.${NC}"
echo -e "  ${DIM}High counts may indicate port scanning or misconfigured services.${NC}"
echo ""
eval "${JCTL_BASE}" -g "HISNOS-OBS-WOULD-DROP-IN" 2>/dev/null \
    | grep -oE "SRC=[0-9.]+ .*DPT=[0-9]+" \
    | sort | uniq -c | sort -rn | head 15 \
    | while read -r cnt rest; do
        printf "  %5d  %s\n" "${cnt}" "${rest}"
    done || echo "  (none — good)"

# ── Section 5: Allowlist hit rate analysis ────────────────────────────────────
section "5. Kernel Allowlist Hit Analysis"
echo -e "  ${DIM}Which destinations pass the static kernel allowlists?${NC}"
echo ""
eval "${JCTL_BASE}" -g "HISNOS-OBS-ALLOW" 2>/dev/null \
    | grep -oE "DST=[0-9.]+ DPT=[0-9]+" \
    | sort | uniq -c | sort -rn | head 15 \
    | while read -r cnt rest; do
        printf "  %5d  %s\n" "${cnt}" "${rest}"
    done || echo "  (no ALLOW entries — allowlist sets may not be populated)"

echo ""
echo -e "  ${DIM}If ALLOW count is 0: run: sudo egress/allowlists/update-cidrs.sh${NC}"

# ── Section 6: Actionable recommendations ─────────────────────────────────────
section "6. Recommendations"
echo ""

# Missing OpenSnitch rules
if [[ ${COUNT_QUEUE} -gt 0 ]]; then
    TOP_WOULD_QUEUE=$(eval "${JCTL_BASE}" -g "HISNOS-OBS-WOULD-QUEUE" 2>/dev/null \
        | grep -oE "DST=[0-9.]+" | sed 's/DST=//' | sort | uniq -c | sort -rn | head 5 \
        | awk '{print $2}' | tr '\n' ' ')
    echo -e "  ${YELLOW}ACTION${NC}: ${COUNT_QUEUE} flows need OpenSnitch rules."
    echo "    Top destinations: ${TOP_WOULD_QUEUE}"
    echo "    When enforcement is active, these flows will be queued to OpenSnitch."
    echo "    Pre-approve apps in OpenSnitch UI or add to allowlists."
    echo ""
fi

# Allowlist gaps (high-frequency WOULD-QUEUE to well-known CDN ranges)
HIGH_FREQ=$(eval "${JCTL_BASE}" -g "HISNOS-OBS-WOULD-QUEUE" 2>/dev/null \
    | grep -oE "DST=[0-9.]+" | sed 's/DST=//' | sort | uniq -c | sort -rn \
    | awk '$1 > 100 {print $2}' | head 5 || true)
if [[ -n "${HIGH_FREQ}" ]]; then
    echo -e "  ${YELLOW}ACTION${NC}: High-frequency destinations (>100 hits) may belong in kernel allowlists:"
    echo "    ${HIGH_FREQ}"
    echo "    Add to allowlist files and run: sudo egress/allowlists/update-cidrs.sh"
    echo ""
fi

# Unexpected inbound
if [[ ${COUNT_DROP_IN} -gt 50 ]]; then
    echo -e "  ${YELLOW}NOTICE${NC}: ${COUNT_DROP_IN} unexpected inbound events — review section 4."
    echo "    If from your router/LAN, this is normal (ARP, mDNS, DHCP)."
    echo "    External IPs in inbound drops may indicate active scanning."
    echo ""
fi

# Low allowlist coverage
if [[ ${TOTAL} -gt 0 && ${ALLOW_PCT:-0} -lt 20 ]]; then
    echo -e "  ${YELLOW}NOTICE${NC}: Low kernel allowlist coverage (${ALLOW_PCT:-0}%)."
    echo "    CIDR sets may be empty. Run: sudo egress/allowlists/update-cidrs.sh"
    echo ""
fi

if [[ ${COUNT_QUEUE} -eq 0 && ${COUNT_DROP_IN} -eq 0 ]]; then
    echo -e "  ${GREEN}READY${NC}: Observe mode shows no critical gaps. Consider switching to enforcement:"
    echo "    sudo hisnos-egress enforce"
fi

echo ""
echo -e "  ${DIM}Export CIDR candidates:  ${0} --format cidr > /tmp/new-cidrs.txt${NC}"
echo -e "  ${DIM}Export nft elements:     ${0} --format nft${NC}"
