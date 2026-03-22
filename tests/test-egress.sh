#!/usr/bin/env bash
# tests/test-egress.sh — HisnOS egress firewall compatibility test suite
#
# Three modes:
#   --check   Pre-enforcement: verify all critical workstation services can reach
#             their destinations. Run in OBSERVE mode before switching to enforce.
#             Expected result: all PASS (if they fail here, they'll fail under enforcement)
#
#   --verify  Post-enforcement: confirm critical services still work AND confirm
#             that non-allowlisted traffic is blocked. Run immediately after enforce.
#             Expected result: allowlisted services PASS, non-allowlisted BLOCKED
#
#   --observe Parse observe-mode kernel logs and summarise what would be blocked.
#             Does not make network connections — only reads journald.
#
# Usage:
#   ./tests/test-egress.sh --check    # before enforcement
#   ./tests/test-egress.sh --verify   # after enforcement
#   ./tests/test-egress.sh --observe  # log analysis only
#   ./tests/test-egress.sh --strict   # exit 1 on any FAIL

set -euo pipefail

MODE="check"
STRICT=false
TIMEOUT=5  # seconds per network test

for arg in "$@"; do
    case "${arg}" in
        --check)   MODE="check" ;;
        --verify)  MODE="verify" ;;
        --observe) MODE="observe" ;;
        --strict)  STRICT=true ;;
    esac
done

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BOLD='\033[1m'; DIM='\033[2m'; NC='\033[0m'

PASS=0; FAIL=0; WARN=0; SKIP=0

pass() { echo -e "  ${GREEN}[PASS]${NC} $*"; (( PASS++ )) || true; }
fail() { echo -e "  ${RED}[FAIL]${NC} $*"; (( FAIL++ )) || true; }
warn() { echo -e "  ${YELLOW}[WARN]${NC} $*"; (( WARN++ )) || true; }
skip() { echo -e "  ${DIM}[SKIP]${NC} $*"; (( SKIP++ )) || true; }
section() { echo -e "\n${BOLD}── $* ──${NC}"; }

# net_ok <url> — returns 0 if HTTP 200/3xx, 1 otherwise
net_ok() {
    curl --silent --max-time "${TIMEOUT}" --output /dev/null \
         --write-out "%{http_code}" "$1" 2>/dev/null \
    | grep -qE "^[23]"
}

# tcp_ok <host> <port> — returns 0 if TCP connects
tcp_ok() {
    timeout "${TIMEOUT}" bash -c ">/dev/tcp/${1}/${2}" 2>/dev/null
}

# dns_ok <domain> — returns 0 if resolves via 127.0.0.53
dns_ok() {
    dig +short +timeout=3 "@127.0.0.53" "$1" A 2>/dev/null | grep -q "^[0-9]"
}

# ── OBSERVE mode: log analysis only ───────────────────────────────────────
if [[ "${MODE}" == "observe" ]]; then
    section "Observe-mode log analysis"
    echo ""
    echo "  WOULD-QUEUE (needs OpenSnitch rule or allowlist):"
    journalctl -k --no-pager -g "HISNOS-OBS-WOULD-QUEUE" 2>/dev/null \
        | grep -oE "DST=[0-9.]+ .*DPT=[0-9]+" \
        | sort | uniq -c | sort -rn | head 30 \
        | sed 's/^/    /' \
        || echo "    (no data — load observe mode and use workstation first)"
    echo ""
    echo "  WOULD-DROP-IN (unexpected inbound traffic):"
    journalctl -k --no-pager -g "HISNOS-OBS-WOULD-DROP-IN" 2>/dev/null \
        | grep -oE "SRC=[0-9.]+ " \
        | sort | uniq -c | sort -rn | head 15 \
        | sed 's/^/    /' \
        || echo "    (none)"
    exit 0
fi

echo -e "${BOLD}HisnOS Egress Test Suite — mode: ${MODE}${NC}"

# ── SECTION 1: DNS ─────────────────────────────────────────────────────────
section "DNS resolution (via 127.0.0.53)"

if dns_ok "fedoraproject.org"; then
    pass "DNS: fedoraproject.org resolves via 127.0.0.53"
else
    fail "DNS: fedoraproject.org failed — systemd-resolved may be misconfigured"
fi

if dns_ok "flathub.org"; then
    pass "DNS: flathub.org resolves"
else
    fail "DNS: flathub.org failed"
fi

# Direct external DNS should be blocked under enforcement
if [[ "${MODE}" == "verify" ]]; then
    if timeout 3 dig +short "@8.8.8.8" fedoraproject.org A &>/dev/null; then
        fail "SECURITY: Direct external DNS (8.8.8.8) is REACHABLE — expected blocked"
    else
        pass "SECURITY: Direct external DNS (8.8.8.8) is blocked (expected)"
    fi
fi

# ── SECTION 2: NTP ─────────────────────────────────────────────────────────
section "NTP time synchronisation"

NTP_SYNCED=$(timedatectl show --property=NTPSynchronized 2>/dev/null | cut -d= -f2 || echo "unknown")
if [[ "${NTP_SYNCED}" == "yes" ]]; then
    pass "NTP: synchronized (timedatectl NTPSynchronized=yes)"
else
    warn "NTP: not synchronized — may be first boot or UDP/123 blocked"
fi

# ── SECTION 3: Fedora / rpm-ostree updates ─────────────────────────────────
section "Fedora infrastructure (rpm-ostree updates)"

info_msg() { echo -e "  ${DIM}[INFO]${NC} $*"; }

# Check metadata refresh
if sudo rpm-ostree refresh-md --force-redownload 2>/dev/null | grep -qi "success\|up to date\|fetching"; then
    pass "rpm-ostree: metadata refresh succeeded"
elif sudo rpm-ostree refresh-md 2>&1 | grep -qi "error\|fail\|curl"; then
    fail "rpm-ostree: metadata refresh failed — Fedora mirrors may be blocked"
else
    warn "rpm-ostree: ambiguous result — check manually: sudo rpm-ostree refresh-md"
fi

# Direct Fedora CDN HTTPS check
if net_ok "https://dl.fedoraproject.org/pub/fedora/README"; then
    pass "HTTP: dl.fedoraproject.org (Fedora CDN) reachable"
else
    warn "HTTP: dl.fedoraproject.org unreachable — CIDR may be stale"
fi

# ── SECTION 4: Flatpak ─────────────────────────────────────────────────────
section "Flatpak (Flathub)"

if command -v flatpak &>/dev/null; then
    # Test Flathub API reachability
    if net_ok "https://api.flathub.org/v2/appstream/com.valvesoftware.Steam" 2>/dev/null; then
        pass "Flatpak: Flathub API reachable"
    else
        warn "Flatpak: Flathub API unreachable — Flatpak updates may be blocked"
    fi

    # Test flatpak remote listing (exercises DNS + HTTPS)
    if flatpak remote-ls --columns=name flathub &>/dev/null | head -1 &>/dev/null; then
        pass "Flatpak: flathub remote-ls succeeded"
    else
        warn "Flatpak: remote-ls failed — check CIDR allowlist for Flatpak CDN"
    fi
else
    skip "Flatpak not installed"
fi

# ── SECTION 5: Git HTTPS ───────────────────────────────────────────────────
section "Git over HTTPS"

# Git HTTPS uses port 443 — goes through OpenSnitch in enforcement mode
# In --check mode: should pass (observe mode, traffic allowed)
# In --verify mode: may be blocked until OpenSnitch rule approved

if command -v git &>/dev/null; then
    # Use a lightweight ls-remote (no clone) against a well-known public repo
    if timeout "${TIMEOUT}" git ls-remote --quiet \
            "https://github.com/nicowillis/hisnos.git" HEAD &>/dev/null 2>&1; then
        pass "Git HTTPS: github.com reachable"
    elif [[ "${MODE}" == "verify" ]]; then
        warn "Git HTTPS: blocked by enforcement (expected — add OpenSnitch rule for git)"
    else
        warn "Git HTTPS: failed — check network or HTTPS to GitHub"
    fi

    # Fedora Pagure (part of Fedora CIDR set — should always pass)
    if timeout "${TIMEOUT}" git ls-remote --quiet \
            "https://pagure.io/fedora-kickstarts.git" HEAD &>/dev/null 2>&1; then
        pass "Git HTTPS: pagure.io (Fedora CDN set) reachable"
    else
        warn "Git HTTPS: pagure.io failed — Fedora CIDR allowlist may need update"
    fi
else
    skip "git not installed"
fi

# ── SECTION 6: Git SSH ─────────────────────────────────────────────────────
section "Git over SSH"

# SSH port 22 — not in kernel-level allowlist; handled by OpenSnitch
# In --check mode: should pass (observe mode)
# In --verify mode: expected to be blocked (needs OpenSnitch rule)

if [[ "${MODE}" == "check" ]]; then
    if tcp_ok "github.com" 22; then
        pass "Git SSH: github.com:22 reachable (observe mode)"
    else
        warn "Git SSH: github.com:22 unreachable — may be blocked by firewall or network"
    fi
elif [[ "${MODE}" == "verify" ]]; then
    if tcp_ok "github.com" 22; then
        warn "Git SSH: github.com:22 reachable — expected blocked until OpenSnitch rule approved"
    else
        pass "Git SSH: blocked by enforcement (expected — approve in OpenSnitch when needed)"
    fi
fi

# ── SECTION 7: Steam ──────────────────────────────────────────────────────
section "Steam connectivity"

# Steam API — allowed only in gaming_temp chain (when GameMode is active)
# In --check mode: should pass (observe mode, traffic flows)
# In --verify mode: blocked unless gaming_temp is active

if [[ "${MODE}" == "check" ]]; then
    if net_ok "https://api.steampowered.com/ISteamWebAPIUtil/GetSupportedAPIList/v1/"; then
        pass "Steam API: reachable (observe mode)"
    else
        warn "Steam API: unreachable in observe mode — unexpected"
    fi
elif [[ "${MODE}" == "verify" ]]; then
    if net_ok "https://api.steampowered.com/ISteamWebAPIUtil/GetSupportedAPIList/v1/"; then
        warn "Steam API: reachable in enforcement without GameMode — check gaming_temp chain"
    else
        pass "Steam API: blocked at idle (expected — allowed only during GameMode sessions)"
    fi
fi

# Steam CDN download endpoint (Valve-owned IPs — in steam_cidrs set)
# These are only populated by hisnos-gaming.nft via GameMode
if [[ "${MODE}" == "verify" ]]; then
    if tcp_ok "205.196.6.66" 443; then
        warn "Steam CDN (205.196.6.66): reachable at idle — steam_cidrs set may be pre-populated"
    else
        pass "Steam CDN: blocked at idle (expected — populated by GameMode only)"
    fi
fi

# ── SECTION 8: General HTTPS (browser traffic) ────────────────────────────
section "General HTTPS (browser/app traffic via OpenSnitch)"

# In --check mode: should pass (observe)
# In --verify mode: should be BLOCKED (goes to NFQUEUE, OpenSnitch not yet approved)

GENERAL_SITES=("https://example.com" "https://www.google.com")
for site in "${GENERAL_SITES[@]}"; do
    if [[ "${MODE}" == "check" ]]; then
        if net_ok "${site}"; then
            pass "General HTTPS: ${site} reachable (observe mode)"
        else
            warn "General HTTPS: ${site} unreachable in observe — check base network"
        fi
    elif [[ "${MODE}" == "verify" ]]; then
        if net_ok "${site}"; then
            warn "General HTTPS: ${site} REACHABLE under enforcement — OpenSnitch approved?"
            warn "  (This is OK if you've already approved the app in OpenSnitch)"
        else
            pass "General HTTPS: ${site} blocked (expected — needs OpenSnitch rule)"
        fi
    fi
done

# ── SECTION 9: Loopback + local services ──────────────────────────────────
section "Loopback and local services"

if ping -c 1 -W 1 127.0.0.1 &>/dev/null; then
    pass "Loopback: 127.0.0.1 responds"
else
    fail "Loopback: 127.0.0.1 unreachable — critical failure"
fi

# systemd-resolved stub
if ss -ulnp 2>/dev/null | grep -q "127.0.0.53:53"; then
    pass "DNS stub: 127.0.0.53:53 listening"
else
    fail "DNS stub: 127.0.0.53:53 not listening — DNS will fail under enforcement"
fi

# ── SECTION 10: nftables rule verification ────────────────────────────────
section "nftables ruleset integrity"

if sudo nft list table inet hisnos &>/dev/null; then
    POLICY=$(sudo nft list table inet hisnos 2>/dev/null \
        | grep "hook output" | grep -oE "policy (accept|drop)" || echo "unknown")
    if [[ "${MODE}" == "verify" ]]; then
        if echo "${POLICY}" | grep -q "drop"; then
            pass "nftables: OUTPUT policy is DROP (enforcement mode)"
        else
            warn "nftables: OUTPUT policy is ${POLICY} — expected DROP in verify mode"
        fi
    elif [[ "${MODE}" == "check" ]]; then
        if echo "${POLICY}" | grep -q "accept"; then
            pass "nftables: OUTPUT policy is ACCEPT (observe mode)"
        else
            warn "nftables: OUTPUT policy is ${POLICY} — expected ACCEPT in check mode"
        fi
    fi

    # Verify logging chains exist
    if sudo nft list chain inet hisnos log_out_drop &>/dev/null; then
        pass "nftables: log_out_drop chain present"
    else
        warn "nftables: log_out_drop chain missing — base ruleset may be outdated"
    fi
else
    fail "nftables: inet hisnos table not loaded"
fi

# ── Summary ────────────────────────────────────────────────────────────────
echo ""
echo -e "${BOLD}══ Test Summary (mode: ${MODE}) ══${NC}"
echo -e "  ${GREEN}PASS${NC}: ${PASS}  ${YELLOW}WARN${NC}: ${WARN}  ${RED}FAIL${NC}: ${FAIL}  SKIP: ${SKIP}"
echo ""

if [[ ${FAIL} -gt 0 ]]; then
    echo -e "  ${RED}Action required:${NC} failed checks must be resolved before enforcement"
fi

if [[ "${MODE}" == "verify" && ${FAIL} -eq 0 ]]; then
    echo -e "  ${GREEN}Enforcement mode is working correctly.${NC}"
    echo "  Cancel dead-man timer: sudo hisnos-egress status"
fi

${STRICT} && [[ ${FAIL} -gt 0 ]] && exit 1
exit 0
