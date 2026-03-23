#!/bin/bash
# dracut hook: initqueue/finished/30-hisnos-live-finished.sh
#
# Returns 0 when the source device has been found (unblocking the initqueue
# loop).  Returns 1 to keep waiting.

getargbool 0 hisnos.live || exit 0

# We're ready when we have a cached source device path.
[[ -f /run/hisnos/source-dev ]] && exit 0

# Not ready yet.
exit 1
