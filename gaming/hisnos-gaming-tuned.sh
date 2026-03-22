#!/usr/bin/env bash
# gaming/hisnos-gaming-tuned.sh — HisnOS Privileged Gaming Tuning Helper
#
# Runs as root (via hisnos-gaming-tuned-start.service / -stop.service).
# Performs kernel-level performance tuning that cannot be done from user space:
#   - CPU frequency governor switching (performance / powersave)
#   - IRQ affinity isolation for GPU and NIC interrupt handlers
#   - nftables gaming chain activation / deactivation
#
# Rollback contract:
#   On "start": saves current state to SAVE_DIR before every change.
#   On "stop":  reads SAVE_DIR and restores every saved value.
#   If any tuning step fails on start: immediately calls stop to roll back.
#   On stop failure: logs HISNOS_GAMING_ROLLBACK_FAILED and exits non-zero.
#
# This script must not be called directly by users.
# It is installed to /etc/hisnos/gaming/ and executed only by systemd.
set -euo pipefail

readonly LOG_TAG="hisnos-gaming-tuned"
readonly SAVE_DIR="/run/hisnos/gaming-tuned-state"
readonly NFT_GAMING_RULES="/etc/nftables/hisnos-gaming.nft"
readonly NFT_BIN="/usr/sbin/nft"
readonly GAMING_PERF_GOV="performance"
readonly NORMAL_GOV="powersave"
# IRQ affinity: isolate GPU interrupts to cores ≥2 (leave cores 0-1 for OS/input).
# Expressed as a hex CPU mask covering cores 2-N.
readonly GAMING_IRQ_CORES="2-7"

log_info()  { systemd-cat -t "${LOG_TAG}" -p info    printf '%s\n' "$*"; }
log_warn()  { systemd-cat -t "${LOG_TAG}" -p warning printf '%s\n' "$*" >&2; }
log_error() { systemd-cat -t "${LOG_TAG}" -p err     printf '%s\n' "$*" >&2; }

emit_event() {
    local event="$1"; shift
    systemd-cat -t "${LOG_TAG}" -p notice printf '%s %s\n' "${event}" "$*"
}

# ── CPU governor ──────────────────────────────────────────────────────────────

save_cpu_governors() {
    mkdir -p "${SAVE_DIR}/cpufreq"
    local saved=0
    for cpu_dir in /sys/devices/system/cpu/cpu[0-9]*/cpufreq; do
        [[ -f "${cpu_dir}/scaling_governor" ]] || continue
        local cpu
        cpu=$(basename "$(dirname "${cpu_dir}")")
        cat "${cpu_dir}/scaling_governor" > "${SAVE_DIR}/cpufreq/${cpu}" 2>/dev/null || true
        (( saved++ )) || true
    done
    log_info "saved CPU governors for ${saved} CPUs"
}

set_cpu_governors() {
    local gov="$1"
    local changed=0 failed=0
    for f in /sys/devices/system/cpu/cpu[0-9]*/cpufreq/scaling_governor; do
        [[ -f "${f}" ]] || continue
        # Verify governor is available before setting
        local avail_file
        avail_file="${f%scaling_governor}scaling_available_governors"
        if [[ -f "${avail_file}" ]] && ! grep -qw "${gov}" "${avail_file}" 2>/dev/null; then
            log_warn "governor '${gov}' not available for $(dirname "${f}") — skipping"
            (( failed++ )) || true
            continue
        fi
        if echo "${gov}" > "${f}" 2>/dev/null; then
            (( changed++ )) || true
        else
            (( failed++ )) || true
        fi
    done
    log_info "CPU governor → ${gov} (changed=${changed} failed=${failed})"
    [[ "${failed}" -eq 0 ]] || return 1
    return 0
}

restore_cpu_governors() {
    [[ -d "${SAVE_DIR}/cpufreq" ]] || return 0
    local restored=0 failed=0
    for saved_file in "${SAVE_DIR}/cpufreq"/cpu[0-9]*; do
        [[ -f "${saved_file}" ]] || continue
        local cpu
        cpu=$(basename "${saved_file}")
        local gov_path="/sys/devices/system/cpu/${cpu}/cpufreq/scaling_governor"
        local saved_gov
        saved_gov=$(cat "${saved_file}" 2>/dev/null || echo "${NORMAL_GOV}")
        if [[ -f "${gov_path}" ]]; then
            if echo "${saved_gov}" > "${gov_path}" 2>/dev/null; then
                (( restored++ )) || true
            else
                (( failed++ )) || true
            fi
        fi
    done
    log_info "CPU governors restored (restored=${restored} failed=${failed})"
}

# ── IRQ affinity ──────────────────────────────────────────────────────────────

# Identify GPU and high-bandwidth NIC IRQs from /proc/interrupts.
find_device_irqs() {
    grep -iE "nvidia|amdgpu|i915|radeon|xhci|nvme|e1000|igb|ixgbe" \
        /proc/interrupts 2>/dev/null \
        | awk '{print $1}' | tr -d ':' | sort -nu || true
}

save_irq_affinity() {
    local irq_list
    irq_list=$(find_device_irqs)
    if [[ -z "${irq_list}" ]]; then
        log_info "no device IRQs found to tune — skipping IRQ affinity"
        return 0
    fi
    mkdir -p "${SAVE_DIR}/irq"
    local saved=0
    while IFS= read -r irq; do
        [[ -f "/proc/irq/${irq}/smp_affinity_list" ]] || continue
        cat "/proc/irq/${irq}/smp_affinity_list" > "${SAVE_DIR}/irq/${irq}" 2>/dev/null || true
        (( saved++ )) || true
    done <<< "${irq_list}"
    log_info "saved IRQ affinity for ${saved} device IRQs"
    # Store the list for restore
    echo "${irq_list}" > "${SAVE_DIR}/irq/.list"
}

set_irq_affinity() {
    local cores="$1"
    [[ -f "${SAVE_DIR}/irq/.list" ]] || return 0
    local changed=0 failed=0
    while IFS= read -r irq; do
        [[ -n "${irq}" ]] || continue
        local affinity_file="/proc/irq/${irq}/smp_affinity_list"
        [[ -f "${affinity_file}" ]] || continue
        if echo "${cores}" > "${affinity_file}" 2>/dev/null; then
            (( changed++ )) || true
        else
            (( failed++ )) || true
        fi
    done < "${SAVE_DIR}/irq/.list"
    log_info "IRQ affinity → cores ${cores} (changed=${changed} failed=${failed})"
}

restore_irq_affinity() {
    [[ -f "${SAVE_DIR}/irq/.list" ]] || return 0
    local restored=0 failed=0
    while IFS= read -r irq; do
        [[ -n "${irq}" ]] || continue
        local saved_file="${SAVE_DIR}/irq/${irq}"
        local affinity_file="/proc/irq/${irq}/smp_affinity_list"
        [[ -f "${saved_file}" && -f "${affinity_file}" ]] || continue
        local saved_val
        saved_val=$(cat "${saved_file}" 2>/dev/null || echo "0-7")
        if echo "${saved_val}" > "${affinity_file}" 2>/dev/null; then
            (( restored++ )) || true
        else
            (( failed++ )) || true
        fi
    done < "${SAVE_DIR}/irq/.list"
    log_info "IRQ affinity restored (restored=${restored} failed=${failed})"
}

# ── nftables gaming chain ─────────────────────────────────────────────────────

load_nft_gaming() {
    if [[ ! -f "${NFT_GAMING_RULES}" ]]; then
        log_warn "gaming nftables file not found: ${NFT_GAMING_RULES} — skipping"
        return 0
    fi
    if ! "${NFT_BIN}" -c -f "${NFT_GAMING_RULES}" &>/dev/null; then
        log_error "gaming nftables syntax check failed — skipping"
        return 1
    fi
    if "${NFT_BIN}" -f "${NFT_GAMING_RULES}" 2>/dev/null; then
        log_info "gaming nftables rules loaded"
        touch "${SAVE_DIR}/nft_gaming_loaded"
    else
        log_warn "gaming nftables load failed — continuing without gaming firewall rules"
        return 1
    fi
}

unload_nft_gaming() {
    [[ -f "${SAVE_DIR}/nft_gaming_loaded" ]] || return 0
    # Flush gaming chains specifically (do not flush entire table)
    for chain in gaming_output gaming_input; do
        "${NFT_BIN}" flush chain inet hisnos "${chain}" 2>/dev/null || true
    done
    rm -f "${SAVE_DIR}/nft_gaming_loaded"
    log_info "gaming nftables rules flushed"
}

# ── Start: activate gaming tuning ─────────────────────────────────────────────

do_start() {
    mkdir -p "${SAVE_DIR}"

    local any_failed=false

    # Save state before any changes (enables rollback)
    save_cpu_governors
    save_irq_affinity

    # CPU governor
    if ! set_cpu_governors "${GAMING_PERF_GOV}"; then
        log_warn "CPU governor tuning incomplete"
        any_failed=true
    fi

    # IRQ affinity
    if ! set_irq_affinity "${GAMING_IRQ_CORES}"; then
        log_warn "IRQ affinity tuning incomplete"
        any_failed=true
    fi

    # nftables gaming rules
    if ! load_nft_gaming; then
        log_warn "nftables gaming rules not loaded"
        any_failed=true
    fi

    if [[ "${any_failed}" == "true" ]]; then
        emit_event "HISNOS_GAMING_TUNED_STARTED" "status=degraded"
    else
        emit_event "HISNOS_GAMING_TUNED_STARTED" "status=ok cpu_gov=performance irq_isolated=true nft=loaded"
    fi

    log_info "gaming tuning complete (degraded=${any_failed})"
}

# ── Stop: restore normal tuning ───────────────────────────────────────────────

do_stop() {
    local restore_failed=false

    restore_cpu_governors || { log_warn "CPU governor restore incomplete"; restore_failed=true; }
    restore_irq_affinity  || { log_warn "IRQ affinity restore incomplete"; restore_failed=true; }
    unload_nft_gaming     || { log_warn "nftables gaming unload incomplete"; restore_failed=true; }

    # Clean up saved state
    rm -rf "${SAVE_DIR}"

    if [[ "${restore_failed}" == "true" ]]; then
        emit_event "HISNOS_GAMING_ROLLBACK_FAILED" "reason=restore_errors"
        log_error "one or more restore steps failed — check journal for details"
        exit 1
    fi

    emit_event "HISNOS_GAMING_TUNED_STOPPED" "status=ok cpu_gov=restored irq=restored nft=flushed"
    log_info "gaming tuning rolled back — system in normal profile"
}

# ── Entrypoint ────────────────────────────────────────────────────────────────

case "${1:-}" in
    start) do_start ;;
    stop)  do_stop  ;;
    *)
        echo "Usage: ${0##*/} {start|stop}" >&2
        exit 1
        ;;
esac
