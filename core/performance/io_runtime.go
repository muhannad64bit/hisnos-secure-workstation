// core/performance/io_runtime.go — Block device IO scheduler and queue tuning.
//
// Targets NVMe SSDs (scheduler "none") and SATA SSDs ("mq-deadline").
// Tunes read_ahead_kb and nr_requests per device.
package performance

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// IORuntime manages block device IO scheduler and queue parameters.
type IORuntime struct{}

// IOSnapshot holds per-device IO settings before a profile switch.
type IOSnapshot struct {
	Schedulers map[string]string // device → active scheduler
	ReadAheads map[string]string // device → read_ahead_kb
	NRRequests map[string]string // device → nr_requests
}

// blockPrefixes enumerates the device name prefixes we manage.
var blockPrefixes = []string{"nvme", "sda", "sdb", "sdc", "sdd", "vda", "vdb"}

// Snapshot captures IO settings for all target block devices.
func (r *IORuntime) Snapshot() (*IOSnapshot, error) {
	devs := r.findDevices()
	snap := &IOSnapshot{
		Schedulers: make(map[string]string, len(devs)),
		ReadAheads: make(map[string]string, len(devs)),
		NRRequests: make(map[string]string, len(devs)),
	}
	for _, dev := range devs {
		q := filepath.Join("/sys/block", dev, "queue")
		if v, err := r.activeScheduler(q); err == nil {
			snap.Schedulers[dev] = v
		}
		if v, err := readFile(filepath.Join(q, "read_ahead_kb")); err == nil {
			snap.ReadAheads[dev] = v
		}
		if v, err := readFile(filepath.Join(q, "nr_requests")); err == nil {
			snap.NRRequests[dev] = v
		}
	}
	return snap, nil
}

// Apply sets the IO scheduler and queue parameters for all target devices.
// NVMe devices always use "none" regardless of scheduler argument (best for low-latency random IO).
// readAheadKB="0" disables read-ahead (optimal for pure random workloads like games).
func (r *IORuntime) Apply(scheduler, readAheadKB, nrRequests string) error {
	devs := r.findDevices()
	for _, dev := range devs {
		q := filepath.Join("/sys/block", dev, "queue")
		sched := r.selectScheduler(dev, scheduler)
		if err := writeFile(filepath.Join(q, "scheduler"), sched); err != nil {
			log.Printf("[perf/io] WARN: scheduler %s on %s: %v", sched, dev, err)
		}
		if readAheadKB != "" {
			if err := writeFile(filepath.Join(q, "read_ahead_kb"), readAheadKB); err != nil {
				log.Printf("[perf/io] WARN: read_ahead_kb %s %s: %v", dev, readAheadKB, err)
			}
		}
		if nrRequests != "" {
			if err := writeFile(filepath.Join(q, "nr_requests"), nrRequests); err != nil {
				log.Printf("[perf/io] WARN: nr_requests %s %s: %v", dev, nrRequests, err)
			}
		}
	}
	log.Printf("[perf/io] scheduler=%s applied to %d devices", scheduler, len(devs))
	return nil
}

// Restore resets IO settings to snapshotted values.
func (r *IORuntime) Restore(snap *IOSnapshot) {
	if snap == nil {
		return
	}
	for dev, sched := range snap.Schedulers {
		_ = writeFile(filepath.Join("/sys/block", dev, "queue", "scheduler"), sched)
	}
	for dev, ra := range snap.ReadAheads {
		_ = writeFile(filepath.Join("/sys/block", dev, "queue", "read_ahead_kb"), ra)
	}
	for dev, nr := range snap.NRRequests {
		_ = writeFile(filepath.Join("/sys/block", dev, "queue", "nr_requests"), nr)
	}
}

// selectScheduler returns the best scheduler for a device type.
// NVMe: always "none" (internal controller handles queuing optimally).
// Others: use caller-supplied scheduler.
func (r *IORuntime) selectScheduler(dev, requested string) string {
	if strings.HasPrefix(dev, "nvme") {
		return "none"
	}
	return requested
}

// findDevices returns block device names in /sys/block matching our prefixes.
func (r *IORuntime) findDevices() []string {
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil
	}
	var devs []string
	for _, e := range entries {
		name := e.Name()
		for _, pfx := range blockPrefixes {
			if strings.HasPrefix(name, pfx) {
				devs = append(devs, name)
				break
			}
		}
	}
	return devs
}

// activeScheduler reads the currently active scheduler from the queue/scheduler
// file content like "[none] mq-deadline kyber bfq" and extracts the bracketed value.
func (r *IORuntime) activeScheduler(queueBase string) (string, error) {
	content, err := readFile(filepath.Join(queueBase, "scheduler"))
	if err != nil {
		return "", err
	}
	for _, tok := range strings.Fields(content) {
		if strings.HasPrefix(tok, "[") && strings.HasSuffix(tok, "]") {
			return tok[1 : len(tok)-1], nil
		}
	}
	return content, nil
}
