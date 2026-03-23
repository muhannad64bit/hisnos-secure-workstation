#!/bin/bash
# dracut hook: cmdline/91-hisnos-live-cmdline.sh
#
# Parses HisnOS live boot parameters.
# Sets rootok=1 so dracut knows we are handling root mounting ourselves.
# Must run before the standard rootfs-block cmdline hooks (which give up
# if they see an unrecognised root= format).

# Source dracut functions (available in all hooks).
# shellcheck source=/dev/null
. /lib/dracut/hisnos-live-lib.sh 2>/dev/null || true

# Only activate if hisnos.live=1 is on the cmdline.
getargbool 0 hisnos.live || exit 0

hisnos_log INFO "cmdline hook: hisnos.live=1 detected"

# Tell dracut we know how to handle this root.
# This prevents dracut from hanging in initqueue waiting for a root device
# that matches none of the built-in handlers.
rootok=1
export rootok

# Parse optional parameters.
HISNOS_CDLABEL=$(getarg hisnos.cdlabel= 2>/dev/null || echo "HISNOS_LIVE")
export HISNOS_CDLABEL

# If root= is not set or is a generic value, set a sensible default.
root=$(getarg root= 2>/dev/null || true)
if [[ -z "${root}" || "${root}" == "block:/dev/nfs" ]]; then
    root="live:CDLABEL=${HISNOS_CDLABEL}"
    export root
fi

hisnos_log INFO "root=${root} cdlabel=${HISNOS_CDLABEL}"
