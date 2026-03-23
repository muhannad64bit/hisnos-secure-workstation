#!/bin/bash
# dracut hook: initqueue/30-hisnos-live-initqueue.sh
#
# Called repeatedly by dracut's initqueue loop until initqueue/finished
# hooks all return 0.  We use this to detect the source device and cache
# it so the finished check is fast.

getargbool 0 hisnos.live || exit 0

. /lib/dracut/hisnos-live-lib.sh 2>/dev/null || true

# Already found in a previous iteration.
[[ -f /run/hisnos/source-dev ]] && exit 0

# Guard against running before block devices have settled.
udevadm settle --timeout=2 2>/dev/null || true

hisnos_log INFO "initqueue: scanning for source device..."
hisnos_init_dirs

if hisnos_find_source_dev; then
    echo "${HISNOS_SOURCE_DEV}" > /run/hisnos/source-dev
    hisnos_log OK "source device cached: ${HISNOS_SOURCE_DEV}"
fi

exit 0
