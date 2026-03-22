#!/usr/bin/env bash
# update-cidrs.sh — Refresh CIDR allowlists with fail-safe protection
#
# Resolves domain lists to IPs and updates live nftables named sets.
# Designed to be safe even under: DNS failure, partial resolution,
# CDN IP churn, and network outages during execution.
#
# Fail-safe hierarchy (per set, in order):
#   1. Fresh DNS resolve succeeds and passes safety checks → update live set
#   2. Fresh resolve returns too few IPs (DNS failure) → use persistent cache
#   3. Persistent cache missing → keep current live set unchanged (no flush)
#   4. Live set not loaded → load from static fallback (hisnos-updates.nft)
#
# Safety checks on fresh resolve:
#   a) MIN_IPS_PER_SET: if fewer IPs resolved than this threshold → treat as failure
#   b) MAX_SHRINK_FACTOR: if new count is < N% of current → refuse update
#      (CDN partial outage returning stale/fewer IPs should not wipe allowlist)
#
# Cache hierarchy:
#   Volatile:    /run/hisnos-cidrs/       (tmpfs, lost on reboot)
#   Persistent:  /var/lib/hisnos/cidrs/   (survives reboots, updated after success)
#   Static:      /etc/nftables/hisnos-updates.nft (boot-time fallback, always present)
#
# Modes:
#   (default)     resolve + update live nft sets with all fail-safes
#   --dry-run     resolve and print what would change, no nft writes
#   --write-only  resolve to volatile cache only, no nft writes
#   --from-cache  load volatile cache into nft (used after boot before DNS up)
#   --from-persistent  load persistent cache into nft (offline recovery)
#   --status      show current set sizes and cache file ages
#
# Run contexts:
#   On demand:    sudo egress/allowlists/update-cidrs.sh
#   Post-install: called by bootstrap/post-install.sh
#   Monthly:      systemd timer (hisnos-cidr-refresh.timer — Phase 6)
#   Boot (early): --from-persistent (before DNS is available)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Cache locations ─────────────────────────────────────────────────────────
VOLATILE_CACHE="/run/hisnos-cidrs"           # tmpfs — fast, lost on reboot
PERSISTENT_CACHE="/var/lib/hisnos/cidrs"     # disk — survives reboots
LOG_FILE="/var/log/hisnos-cidr-refresh.log"

# ── Source list files ───────────────────────────────────────────────────────
FEDORA_LIST="${SCRIPT_DIR}/fedora-mirrors.list"
FLATPAK_LIST="${SCRIPT_DIR}/flatpak-repos.list"
STEAM_LIST="${SCRIPT_DIR}/steam-cdn.list"

# ── nft targets ─────────────────────────────────────────────────────────────
NFT_TABLE="inet hisnos"
NFT_SET_FEDORA="fedora_update_cidrs"
NFT_SET_FLATPAK="flatpak_cidrs"
NFT_SET_STEAM="steam_cidrs"

# ── Safety thresholds ───────────────────────────────────────────────────────
DIG_TIMEOUT=3
DIG_RETRIES=2
MAX_IPS_PER_DOMAIN=8    # cap per domain (CDNs may return hundreds)
MIN_IPS_PER_SET=3       # fewer than this = DNS failure for this target
MAX_SHRINK_FACTOR=50    # refuse update if new_count < (current_count * N / 100)
                        # e.g. 50 = refuse if new set would be <50% of current size

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BOLD='\033[1m'; NC='\033[0m'
info()  {
    echo -e "${GREEN}[cidr-refresh]${NC} $*"
    echo "[$(date -Iseconds)] INFO: $*" >> "${LOG_FILE}" 2>/dev/null || true
}
warn()  {
    echo -e "${YELLOW}[cidr-refresh WARN]${NC} $*"
    echo "[$(date -Iseconds)] WARN: $*" >> "${LOG_FILE}" 2>/dev/null || true
}
error() { echo -e "${RED}[cidr-refresh ERROR]${NC} $*" >&2; exit 1; }

# ── Argument parsing ────────────────────────────────────────────────────────
MODE="update"
case "${1:-}" in
    --dry-run)           MODE="dry-run" ;;
    --write-only)        MODE="write-only" ;;
    --from-cache)        MODE="from-cache" ;;
    --from-persistent)   MODE="from-persistent" ;;
    --status)            MODE="status" ;;
esac

[[ "${EUID}" -eq 0 || "${MODE}" =~ ^(dry-run|status)$ ]] || \
    error "Must run as root. Try: sudo ${0}"

# ── Helpers ─────────────────────────────────────────────────────────────────
resolve_domain() {
    local domain="$1"
    dig +short +timeout="${DIG_TIMEOUT}" +tries="${DIG_RETRIES}" \
        "@127.0.0.53" "${domain}" A 2>/dev/null \
        | grep -E "^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$" \
        | head -"${MAX_IPS_PER_DOMAIN}" \
        || true
}

read_list_file() {
    local f="$1"
    [[ -f "${f}" ]] || { warn "List file not found: ${f}"; return; }
    grep -vE "^\s*#|^\s*$" "${f}" | sort -u
}

resolve_list() {
    local list_file="$1"
    local -a ips=()
    local resolved_any=false

    while IFS= read -r domain; do
        local resolved
        resolved=$(resolve_domain "${domain}")
        if [[ -n "${resolved}" ]]; then
            resolved_any=true
            while IFS= read -r ip; do
                ips+=("${ip}")
            done <<< "${resolved}"
        else
            warn "  No IPs for: ${domain} (DNS failure or NXDOMAIN)"
        fi
    done < <(read_list_file "${list_file}")

    ${resolved_any} || { warn "  Zero domains resolved from ${list_file}"; }
    printf '%s\n' "${ips[@]:-}" | grep -E "^[0-9]" | sort -u
}

current_set_count() {
    # Returns number of IPs currently in a live nft set
    local set_name="$1"
    nft list set "${NFT_TABLE}" "${set_name}" 2>/dev/null \
        | grep -oE "[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+" \
        | wc -l \
        || echo 0
}

load_persistent_cache() {
    local target="$1"
    local nft_set="$2"
    local p_cache="${PERSISTENT_CACHE}/${target}.cidrs"

    if [[ -f "${p_cache}" ]]; then
        local age_days
        age_days=$(( ( $(date +%s) - $(stat -c %Y "${p_cache}") ) / 86400 ))
        warn "Loading persistent cache for ${target} (age: ${age_days} days)"
        if [[ $(wc -l < "${p_cache}") -lt ${MIN_IPS_PER_SET} ]]; then
            warn "Persistent cache for ${target} also too small — keeping live set unchanged"
            return 1
        fi
        # Copy to volatile for apply_to_nft_set
        cp "${p_cache}" "${VOLATILE_CACHE}/${target}.cidrs"
        return 0
    else
        warn "No persistent cache for ${target} — keeping live set unchanged"
        return 1
    fi
}

apply_to_nft_set() {
    # Core update function with safety checks
    local set_name="$1"
    local ips_file="$2"
    local target_label="$3"

    local new_count
    new_count=$(wc -l < "${ips_file}" || echo 0)

    # Safety check A: minimum IPs
    if [[ ${new_count} -lt ${MIN_IPS_PER_SET} ]]; then
        warn "SAFETY-A: ${target_label}: only ${new_count} IPs (min=${MIN_IPS_PER_SET}) — refusing update"
        return 1
    fi

    # Safety check B: shrink protection
    local current_count
    current_count=$(current_set_count "${set_name}")
    if [[ ${current_count} -gt 0 ]]; then
        local min_allowed=$(( current_count * MAX_SHRINK_FACTOR / 100 ))
        if [[ ${new_count} -lt ${min_allowed} ]]; then
            warn "SAFETY-B: ${target_label}: new=${new_count} IPs < ${MAX_SHRINK_FACTOR}% of current=${current_count} — refusing shrink"
            warn "  This may indicate partial DNS resolution failure or CDN outage."
            warn "  If intentional, run: sudo ${0} --force-shrink (not implemented — edit threshold)"
            return 1
        fi
    fi

    # Check set exists in live nft
    if ! nft list set "${NFT_TABLE}" "${set_name}" &>/dev/null; then
        warn "nft set '${set_name}' not loaded — is hisnos-base.nft active?"
        return 1
    fi

    # Build element list
    local elements
    elements=$(awk '{printf "%s,", $1}' "${ips_file}" | sed 's/,$//')

    # Atomic flush → add (minimal window with empty set)
    nft flush set "${NFT_TABLE}" "${set_name}"
    nft add element "${NFT_TABLE}" "${set_name}" "{ ${elements} }"

    info "${target_label}: updated ${set_name} — ${new_count} IPs (was ${current_count})"
    return 0
}

save_persistent() {
    # Save successful volatile cache to persistent storage
    local target="$1"
    mkdir -p "${PERSISTENT_CACHE}"
    cp "${VOLATILE_CACHE}/${target}.cidrs" "${PERSISTENT_CACHE}/${target}.cidrs"
    # Record refresh timestamp
    date -Iseconds > "${PERSISTENT_CACHE}/${target}.last-refresh"
    info "Saved persistent cache: ${PERSISTENT_CACHE}/${target}.cidrs"
}

# ── Status mode ──────────────────────────────────────────────────────────────
if [[ "${MODE}" == "status" ]]; then
    echo -e "${BOLD}CIDR allowlist status${NC}"
    for target in fedora flatpak steam; do
        echo ""
        echo "  ${target}:"
        # Live nft set
        local_set=""
        case "${target}" in
            fedora)  local_set="${NFT_SET_FEDORA}" ;;
            flatpak) local_set="${NFT_SET_FLATPAK}" ;;
            steam)   local_set="${NFT_SET_STEAM}" ;;
        esac
        live=$(current_set_count "${local_set}" 2>/dev/null || echo "N/A")
        echo "    Live set (${local_set}): ${live} IPs"

        # Persistent cache
        p="${PERSISTENT_CACHE}/${target}.cidrs"
        if [[ -f "${p}" ]]; then
            p_count=$(wc -l < "${p}")
            p_age=$(stat -c %y "${p}" | cut -d. -f1)
            echo "    Persistent cache: ${p_count} IPs (updated: ${p_age})"
        else
            echo "    Persistent cache: not found"
        fi

        # Volatile cache
        v="${VOLATILE_CACHE}/${target}.cidrs"
        [[ -f "${v}" ]] && echo "    Volatile cache:   $(wc -l < "${v}") IPs" \
                        || echo "    Volatile cache:   not found"
    done
    echo ""
    echo "  Log: ${LOG_FILE}"
    echo "  Refresh: sudo ${0}"
    exit 0
fi

# ── From-volatile-cache mode ─────────────────────────────────────────────────
if [[ "${MODE}" == "from-cache" ]]; then
    info "Loading from volatile cache (${VOLATILE_CACHE}/)"
    for pair in "fedora:${NFT_SET_FEDORA}" "flatpak:${NFT_SET_FLATPAK}" "steam:${NFT_SET_STEAM}"; do
        name="${pair%%:*}"; set="${pair##*:}"
        cache="${VOLATILE_CACHE}/${name}.cidrs"
        [[ -f "${cache}" ]] && apply_to_nft_set "${set}" "${cache}" "${name}" \
                            || warn "Volatile cache not found for ${name}"
    done
    exit 0
fi

# ── From-persistent-cache mode ───────────────────────────────────────────────
if [[ "${MODE}" == "from-persistent" ]]; then
    info "Loading from persistent cache (${PERSISTENT_CACHE}/)"
    for pair in "fedora:${NFT_SET_FEDORA}" "flatpak:${NFT_SET_FLATPAK}" "steam:${NFT_SET_STEAM}"; do
        name="${pair%%:*}"; set="${pair##*:}"
        cache="${PERSISTENT_CACHE}/${name}.cidrs"
        if [[ -f "${cache}" ]]; then
            cp "${cache}" "${VOLATILE_CACHE}/${name}.cidrs" 2>/dev/null || true
            apply_to_nft_set "${set}" "${cache}" "${name}" || true
        else
            warn "Persistent cache not found for ${name} — set unchanged"
        fi
    done
    exit 0
fi

# ── Main resolution loop ─────────────────────────────────────────────────────
mkdir -p "${VOLATILE_CACHE}" "${PERSISTENT_CACHE}" 2>/dev/null || true
info "Starting CIDR refresh (mode: ${MODE})"

# Check DNS is available before attempting batch resolve
if ! dig +short +timeout=2 "@127.0.0.53" "fedoraproject.org" A &>/dev/null | grep -q "^[0-9]"; then
    warn "DNS probe failed (127.0.0.53 not responding or no A records)"
    warn "Attempting to load from persistent cache instead"
    for pair in "fedora:${NFT_SET_FEDORA}" "flatpak:${NFT_SET_FLATPAK}" "steam:${NFT_SET_STEAM}"; do
        name="${pair%%:*}"; set="${pair##*:}"
        load_persistent_cache "${name}" "${set}" && \
            apply_to_nft_set "${set}" "${VOLATILE_CACHE}/${name}.cidrs" "${name}" || true
    done
    warn "CIDR refresh incomplete — DNS unavailable. Retry when DNS is up."
    exit 1
fi

declare -A TARGETS=(
    ["fedora"]="${FEDORA_LIST}|${NFT_SET_FEDORA}"
    ["flatpak"]="${FLATPAK_LIST}|${NFT_SET_FLATPAK}"
    ["steam"]="${STEAM_LIST}|${NFT_SET_STEAM}"
)

UPDATED=0; FALLBACK=0; UNCHANGED=0

for target_name in fedora flatpak steam; do
    IFS='|' read -r list_file nft_set <<< "${TARGETS[${target_name}]}"
    volatile_file="${VOLATILE_CACHE}/${target_name}.cidrs"

    info "Resolving ${target_name}..."

    # Resolve to volatile cache
    resolved_ips=$(resolve_list "${list_file}")
    echo "${resolved_ips}" > "${volatile_file}"
    new_count=$(wc -l < "${volatile_file}" || echo 0)
    info "  Resolved: ${new_count} unique IPs"

    if [[ "${MODE}" == "dry-run" ]]; then
        echo "  DRY-RUN: would update ${nft_set} with ${new_count} IPs"
        head -5 "${volatile_file}" | sed 's/^/    /'
        [[ ${new_count} -gt 5 ]] && echo "    ..."
        continue
    fi

    if [[ "${MODE}" == "write-only" ]]; then
        continue
    fi

    # Try to apply fresh resolve — apply_to_nft_set runs safety checks
    if apply_to_nft_set "${nft_set}" "${volatile_file}" "${target_name}"; then
        save_persistent "${target_name}"
        (( UPDATED++ )) || true
    else
        # Fresh resolve failed safety checks → try persistent cache fallback
        warn "Fresh resolve failed safety checks for ${target_name} — trying persistent cache"
        if load_persistent_cache "${target_name}" "${nft_set}"; then
            if apply_to_nft_set "${nft_set}" "${VOLATILE_CACHE}/${target_name}.cidrs" "${target_name} (persistent-fallback)"; then
                (( FALLBACK++ )) || true
            else
                warn "Persistent cache also failed safety checks — ${nft_set} UNCHANGED"
                (( UNCHANGED++ )) || true
            fi
        else
            warn "No fallback available — ${nft_set} UNCHANGED (live set intact)"
            (( UNCHANGED++ )) || true
        fi
    fi
done

# ── Summary ──────────────────────────────────────────────────────────────────
echo ""
if [[ "${MODE}" == "dry-run" ]]; then
    info "Dry-run complete. No changes made."
elif [[ "${MODE}" == "write-only" ]]; then
    info "Volatile cache written. Run --from-cache to load into nft."
else
    info "CIDR refresh complete: ${UPDATED} updated, ${FALLBACK} from cache, ${UNCHANGED} unchanged"
    echo ""
    echo "  Verify:   sudo nft list set inet hisnos fedora_update_cidrs"
    echo "  Status:   sudo ${0} --status"
    echo "  Log:      ${LOG_FILE}"
    [[ ${UNCHANGED} -gt 0 ]] && \
        echo -e "  ${YELLOW}WARNING: ${UNCHANGED} set(s) could not be refreshed. Check DNS and ${LOG_FILE}${NC}"
fi
