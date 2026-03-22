#!/usr/bin/env bash
# bootstrap/post-install.sh — HisnOS Phase 2 post-install deployment
#
# Deploys and activates the HisnOS egress firewall (nftables + OpenSnitch).
# Runs AFTER install.sh and after the Phase 1 reboot.
#
# Safe enablement sequence:
#   1. Validate environment (nft available, /etc/nftables writable)
#   2. Deploy ruleset files to /etc/nftables/
#   3. Deploy egress control script to /usr/local/sbin/
#   4. Refresh CIDR allowlists (resolve domain lists → nft sets)
#   5. Syntax-check all ruleset files
#   6. Load OBSERVE mode (fail-open: all traffic passes, would-drops logged)
#   7. Run pre-enforcement compatibility check (tests/test-egress.sh --check)
#   8. Prompt user: switch to ENFORCE mode with dead-man timer
#   9. If ENFORCE: run post-enforcement connectivity test
#  10. If test passes: cancel timer, enable nftables.service for boot
#  11. Configure OpenSnitch daemon
#
# Idempotent: safe to re-run. Each section checks before acting.
# Run as: regular user with sudo privileges.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NFT_DIR="/etc/nftables"
NFT_SRC="${SCRIPT_DIR}/egress/nftables"
EGRESS_SCRIPT="${SCRIPT_DIR}/egress/hisnos-egress.sh"
TEST_SCRIPT="${SCRIPT_DIR}/tests/test-egress.sh"
CIDR_REFRESH="${SCRIPT_DIR}/egress/allowlists/update-cidrs.sh"
STEP_LOG="${HOME}/.local/share/hisnos/install-steps.log"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BOLD='\033[1m'; NC='\033[0m'
info()    { echo -e "${GREEN}[post-install]${NC} $*"; }
warn()    { echo -e "${YELLOW}[post-install WARN]${NC} $*"; }
error()   { echo -e "${RED}[post-install ERROR]${NC} $*" >&2; exit 1; }
section() { echo -e "\n${BOLD}══ $* ══${NC}"; }

step_done() { [[ -f "${STEP_LOG}" ]] && grep -qx "$1" "${STEP_LOG}" 2>/dev/null; }
mark_done() { grep -qx "$1" "${STEP_LOG}" 2>/dev/null || echo "$1" >> "${STEP_LOG}"; }

[[ "${EUID}" -ne 0 ]] || error "Do not run as root. sudo is used internally."

# ── Step 1: Environment validation ────────────────────────────────────────
section "Environment validation"

command -v nft &>/dev/null || error "nft not found. Install: sudo rpm-ostree install nftables"
command -v rpm-ostree &>/dev/null || error "rpm-ostree not found — not on Kinoite?"
[[ -f "${NFT_SRC}/hisnos-base.nft" ]]    || error "hisnos-base.nft not found in ${NFT_SRC}"
[[ -f "${NFT_SRC}/hisnos-observe.nft" ]] || error "hisnos-observe.nft not found"
[[ -f "${NFT_SRC}/hisnos-updates.nft" ]] || error "hisnos-updates.nft not found"
[[ -f "${EGRESS_SCRIPT}" ]]              || error "hisnos-egress.sh not found at ${EGRESS_SCRIPT}"

info "Environment: OK"

# ── Step 2: Deploy ruleset files ──────────────────────────────────────────
section "Deploying nftables rulesets"

if ! step_done "nft-deployed"; then
    sudo mkdir -p "${NFT_DIR}"

    for f in hisnos-base.nft hisnos-observe.nft hisnos-updates.nft hisnos-gaming.nft; do
        if [[ -f "${NFT_SRC}/${f}" ]]; then
            sudo cp "${NFT_SRC}/${f}" "${NFT_DIR}/${f}"
            info "Deployed: ${NFT_DIR}/${f}"
        fi
    done
    mark_done "nft-deployed"
else
    info "Ruleset files already deployed — refreshing..."
    for f in hisnos-base.nft hisnos-observe.nft hisnos-updates.nft hisnos-gaming.nft; do
        [[ -f "${NFT_SRC}/${f}" ]] && sudo cp "${NFT_SRC}/${f}" "${NFT_DIR}/${f}"
    done
fi

# ── Step 3: Deploy egress control script ─────────────────────────────────
section "Deploying egress control script"

sudo install -m 755 "${EGRESS_SCRIPT}" /usr/local/sbin/hisnos-egress
info "Deployed: /usr/local/sbin/hisnos-egress"
info "Usage: sudo hisnos-egress observe|enforce|flush|status|analyse"

# ── Step 4: Refresh CIDR allowlists ──────────────────────────────────────
section "Refreshing CIDR allowlists"

if [[ -f "${CIDR_REFRESH}" ]]; then
    info "Running CIDR refresh (resolves domain lists → nft sets)..."
    bash "${CIDR_REFRESH}" --write-only 2>/dev/null && info "CIDR sets refreshed." \
        || warn "CIDR refresh failed — using bundled static CIDRs. Check network connectivity."
else
    warn "update-cidrs.sh not found — using static CIDRs from hisnos-updates.nft"
fi

# ── Step 5: Syntax validation ─────────────────────────────────────────────
section "Validating ruleset syntax"

for f in hisnos-base.nft hisnos-observe.nft hisnos-updates.nft; do
    if sudo nft -c -f "${NFT_DIR}/${f}" 2>/dev/null; then
        info "Syntax OK: ${f}"
    else
        error "Syntax error in ${NFT_DIR}/${f} — fix before proceeding"
    fi
done

# ── Step 6: Load OBSERVE mode ─────────────────────────────────────────────
section "Loading OBSERVE mode (staging)"

sudo hisnos-egress observe
info "Observe mode active. Traffic flows freely. Would-deny flows are logged."
echo ""
info "Recommended: use the workstation normally for a few minutes, then continue."
echo "  Watch logs: journalctl -k -f -g 'HISNOS-OBS' (Ctrl+C when done)"
echo ""
read -r -p "Press Enter to continue with compatibility checks..."

# ── Step 7: Pre-enforcement compatibility check ───────────────────────────
section "Pre-enforcement compatibility check"

if [[ -f "${TEST_SCRIPT}" ]]; then
    info "Running workstation compatibility suite..."
    echo ""
    bash "${TEST_SCRIPT}" --check || {
        warn "Some compatibility checks failed."
        warn "Review output above. Missing CIDRs should be added to allowlists."
        echo ""
        read -r -p "Continue to enforcement anyway? [y/N] " CONFIRM
        [[ "${CONFIRM}" =~ ^[Yy]$ ]] || {
            info "Staying in observe mode. Fix failures and re-run post-install.sh."
            exit 0
        }
    }
else
    warn "test-egress.sh not found — skipping compatibility check."
    warn "Proceeding without pre-flight validation."
fi

# ── Step 8: Switch to ENFORCE mode ────────────────────────────────────────
section "Enabling ENFORCE mode"

echo ""
echo -e "${BOLD}Ready to enable firewall enforcement.${NC}"
echo ""
echo "  A 5-minute dead-man timer will be set. If you lose network access,"
echo "  the firewall will automatically revert to observe mode after 5 minutes."
echo "  Cancel it after confirming network works: sudo hisnos-egress status"
echo ""
read -r -p "Enable enforcement now? [y/N] " CONFIRM

if [[ ! "${CONFIRM}" =~ ^[Yy]$ ]]; then
    info "Staying in observe mode. Re-run this script when ready to enforce."
    exit 0
fi

sudo hisnos-egress enforce --timeout 300

# ── Step 9: Post-enforcement connectivity test ────────────────────────────
section "Post-enforcement connectivity test"
echo ""
info "Testing connectivity under enforcement..."
sleep 3  # brief pause for rules to settle

ENFORCE_OK=true
if [[ -f "${TEST_SCRIPT}" ]]; then
    bash "${TEST_SCRIPT}" --verify || ENFORCE_OK=false
else
    # Minimal inline tests if test script not available
    if ! ping -c 1 -W 2 127.0.0.1 &>/dev/null; then
        warn "Loopback ping failed"
        ENFORCE_OK=false
    fi
    if ! dig @127.0.0.53 fedoraproject.org A &>/dev/null; then
        warn "DNS resolution failed"
        ENFORCE_OK=false
    fi
fi

if ${ENFORCE_OK}; then
    # ── Step 10: Cancel timer, enable at boot ─────────────────────────────
    section "Finalizing"
    sudo systemctl stop hisnos-egress-revert.timer 2>/dev/null || true
    info "Dead-man timer cancelled — enforcement confirmed stable."

    # Enable nftables.service to load at boot
    # The service loads /etc/nftables.conf; we write that to include our files.
    sudo tee /etc/nftables.conf > /dev/null <<'CONF'
#!/usr/sbin/nft -f
# HisnOS nftables boot configuration
# Managed by HisnOS bootstrap/post-install.sh
include "/etc/nftables/hisnos-base.nft"
include "/etc/nftables/hisnos-updates.nft"
CONF

    sudo systemctl enable --now nftables.service
    mark_done "nftables-enabled"
    info "nftables.service enabled — firewall loads automatically at boot."
else
    warn "Connectivity tests FAILED under enforcement."
    warn "Dead-man timer will revert to observe in ~5 minutes."
    warn "Or revert manually: sudo hisnos-egress observe"
    warn "Review: sudo hisnos-egress analyse"
    exit 1
fi

# ── Step 11: OpenSnitch ───────────────────────────────────────────────────
section "OpenSnitch configuration"

if ! step_done "opensnitch-configured"; then
    if rpm -q opensnitch &>/dev/null; then
        # OpenSnitch config directory
        sudo mkdir -p /etc/opensnitchd/rules

        # Write base OpenSnitch daemon config
        # intercept_unknown: queue — send unmatched to our nfqueue
        # default_action: deny — fail-closed at the app layer too
        sudo tee /etc/opensnitchd/default-config.json > /dev/null <<'JSON'
{
  "Server": {
    "Address": "unix:///tmp/osui.sock",
    "LogFile": "/var/log/opensnitchd.log"
  },
  "DefaultAction": "deny",
  "DefaultDuration": "until restart",
  "InterceptUnknown": true,
  "ProcMonitorMethod": "ebpf",
  "LogLevel": 1,
  "Firewall": "nftables"
}
JSON

        sudo systemctl enable --now opensnitchd.service
        mark_done "opensnitch-configured"
        info "OpenSnitch configured and started."
        warn "FIRST BOOT: You will see per-app prompts for every new outbound connection."
        warn "Approve trusted apps (browser, flatpak, git) to build your rules database."
    else
        warn "opensnitch package not installed."
        warn "Install: sudo rpm-ostree install opensnitch && reboot, then re-run"
        warn "Without OpenSnitch: all non-allowlisted outbound traffic is dropped (fail-closed NFQUEUE)."
    fi
else
    info "OpenSnitch already configured — skipped"
fi

# ── Complete ──────────────────────────────────────────────────────────────
section "Phase 2 complete"
echo ""
echo "  Firewall: ENFORCE mode active"
echo "  Loaded:   nftables.service (boots automatically)"
echo "  Control:  sudo hisnos-egress <observe|enforce|flush|status|analyse>"
echo ""
echo "  Monitor:  journalctl -k -f -g 'HISNOS-'"
echo "  Logs:     journalctl -k -g 'HISNOS-OUT-DROP' --since today"
echo ""
echo "  Next: Phase 3 — gocryptfs vault (vault/hisnos-vault.sh)"
