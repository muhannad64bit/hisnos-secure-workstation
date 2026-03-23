# HisnOS Chaos Testing Matrix

| Scenario | Trigger Command | Expected System Behavior | Observer |
| --- | --- | --- | --- |
| Kernel Panic (Oops) | `echo c > /proc/sysrq-trigger` | Immediate reset via `kernel.panic=10`. Boot flags trigger fallback UI next boot. | Watchdog / Kernel / Dracut |
| OOM Starvation | `stress-ng --vm-bytes $(awk '/MemAvailable/{printf "%d\n", $2 * 0.9}' < /proc/meminfo)k --vm-keep -m 1` | `vm.panic_on_oom=1` or systemd-oomd terminates stress-ng. System avoids freeze. | Kernel / Watchdog |
| CPU Lockup | `stress-ng --cpu 0 --cpu-method all` | Watchdog detects soft lockup or processes throttled via `CPUWeight=10`. `systemd` unblocked. | systemd / Watchdog |
| Disk Full (/) | `dd if=/dev/zero of=/var/tmp/filler.img bs=1M` | 6h guard detects >90% usage. Auto escalates to `hisnos-safe.target`. | 6h guard |
| Bad Dracut Payload | Corrupt `/boot/initramfs.img` | Dracut fails to mount `rootfs`. Drops immediately to Emergency TUI menu. | 90hisnos-live |
| UI Crash Loop | `killall -9 gnome-shell` (repeatedly) | systemd rate-limits restart. If excessive, falls back to `hisnos-safe.target`. | systemd |
| Excessive Swapping | `stress-ng --vm 4 --vm-bytes 100%` | 6h guard detects swap >80%, escalates to Safe Mode. | 6h guard |
| Stuck rpm-ostree | Terminate post-script execution abruptly | Watchdog detects ET > 1800s, kills process and isolates to `hisnos-safe.target`. | Watchdog |
