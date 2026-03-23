#!/usr/bin/env bash
# dracut/95hisnos/hisnos-vault-unlock.sh
#
# Pre-pivot vault unlock policy.
# NOT installed as a dracut hook directly — sourced by hisnos-boot.sh
# when vault.preunlock=1 is present on the kernel cmdline.
#
# Only unlocks the vault in pre-pivot if:
#   1. vault.preunlock=1 is on the kernel cmdline
#   2. A TPM2-sealed passphrase is available
#   3. gocryptfs is in the initramfs
#
# Standard vault unlock happens at user-session login via
# hisnos-vault.sh (systemd user service).

. /hisnos-lib.sh 2>/dev/null || true

# Exit if pre-unlock not requested
hisnos_getarg "vault.preunlock" &>/dev/null || exit 0

SYSROOT="${NEWROOT:-/sysroot}"
VAULT_BASE="${SYSROOT}/var/lib/hisnos"
VAULT_CIPHER="${VAULT_BASE}/vault-cipher"
VAULT_MOUNT="${VAULT_BASE}/vault"

hisnos_log INFO "Vault pre-unlock requested"

# ─── Dependency checks ───────────────────────────────────────────────────────
if ! command -v gocryptfs &>/dev/null; then
    hisnos_log WARN "gocryptfs not in initramfs — vault pre-unlock unavailable"
    exit 0
fi

if ! command -v tpm2_unseal &>/dev/null; then
    hisnos_log WARN "tpm2-tools not in initramfs — no TPM unsealing available"
    exit 0
fi

# ─── Find vault cipher directory ──────────────────────────────────────────────
if [[ ! -d "${VAULT_CIPHER}" ]]; then
    hisnos_log INFO "No vault cipher directory found at ${VAULT_CIPHER} — nothing to unlock"
    exit 0
fi

# ─── TPM2 unseal passphrase ───────────────────────────────────────────────────
TPM_HANDLE="$(hisnos_getarg "vault.tpm.handle" 2>/dev/null || echo "")"
if [[ -z "$TPM_HANDLE" ]]; then
    TPM_HANDLE="0x81000001"  # default primary key handle
fi

hisnos_log INFO "Attempting TPM2 unseal from handle ${TPM_HANDLE}..."
PASSPHRASE="$(tpm2_unseal --handle="${TPM_HANDLE}" 2>/dev/null)"
if [[ -z "$PASSPHRASE" ]]; then
    hisnos_log WARN "TPM2 unseal failed — vault pre-unlock skipped"
    exit 0
fi

# ─── Mount vault ─────────────────────────────────────────────────────────────
mkdir -p "${VAULT_MOUNT}" 2>/dev/null || true
if echo "$PASSPHRASE" | gocryptfs \
    -passfile /dev/stdin \
    "${VAULT_CIPHER}" "${VAULT_MOUNT}" &>/dev/null; then
    hisnos_log OK "Vault pre-unlocked at ${VAULT_MOUNT}"
    hisnos_flag set vault_preunlocked 1
else
    hisnos_log ERROR "Vault mount failed — passphrase incorrect or cipher corrupted"
    hisnos_flag set vault_preunlock_failed 1
fi
