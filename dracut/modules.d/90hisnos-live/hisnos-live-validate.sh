#!/bin/bash
# dracut hook: pre-pivot/91-hisnos-live-validate.sh
#
# Final validation before dracut calls switch_root.
# If NEWROOT is not a valid live root, aborts and launches emergency UI.

getargbool 0 hisnos.live || exit 0

. /lib/dracut/hisnos-live-lib.sh 2>/dev/null || true

hisnos_log INFO "pre-pivot: validating live root at ${NEWROOT}..."

if ! hisnos_validate_root "${NEWROOT}"; then
    hisnos_die "Live root validation failed — ${NEWROOT} is not a valid OS root. See ${HISNOS_LOG}."
fi

# Verify overlayfs is actually writable.
TESTFILE="${NEWROOT}/run/hisnos/.write-test-$$"
mkdir -p "${NEWROOT}/run/hisnos" 2>/dev/null || true
if ! touch "${TESTFILE}" 2>/dev/null; then
    hisnos_die "Overlayfs is not writable at ${NEWROOT}. RAM overlay may have failed."
fi
rm -f "${TESTFILE}" 2>/dev/null || true

hisnos_log OK "pre-pivot: live root validated — proceeding to switch_root"
exit 0
