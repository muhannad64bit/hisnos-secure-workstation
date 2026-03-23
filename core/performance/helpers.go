// core/performance/helpers.go — shared sysfs I/O helpers for the performance package.
package performance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// readFile reads a sysfs/procfs text file, trimming surrounding whitespace.
func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// writeFile writes a value (plus newline) to a sysfs/procfs file.
func writeFile(path, value string) error {
	return os.WriteFile(path, []byte(value+"\n"), 0644)
}

// writeFileAtomic writes content to path via a temp-file + rename for atomicity.
// Used for persistent state files (not sysfs).
func writeFileAtomic(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".perf-tmp-")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	return os.Rename(tmpPath, path)
}

// mustMarshal JSON-encodes v; returns "{}" on error.
func mustMarshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// parseCPUList parses a kernel cpulist string like "0-3,5,7-9" into CPU indices.
func parseCPUList(s string) ([]int, error) {
	var cpus []int
	for _, part := range strings.Split(strings.TrimSpace(s), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if idx := strings.IndexByte(part, '-'); idx >= 0 {
			lo, err1 := strconv.Atoi(part[:idx])
			hi, err2 := strconv.Atoi(part[idx+1:])
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("bad cpulist range: %q", part)
			}
			for i := lo; i <= hi; i++ {
				cpus = append(cpus, i)
			}
		} else {
			n, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("bad cpulist entry: %q", part)
			}
			cpus = append(cpus, n)
		}
	}
	return cpus, nil
}

// onlineCPUs returns the indices of all online CPUs from /sys/devices/system/cpu/online.
func onlineCPUs() ([]int, error) {
	raw, err := readFile("/sys/devices/system/cpu/online")
	if err != nil {
		return nil, fmt.Errorf("read cpu/online: %w", err)
	}
	return parseCPUList(raw)
}

// sqrtApprox computes the square root of x using Newton-Raphson (stdlib-free).
// Accurate to <0.01% for values encountered in performance metrics.
func sqrtApprox(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x / 2
	for i := 0; i < 16; i++ {
		z -= (z*z - x) / (2 * z)
	}
	return z
}

// formatCPUList formats a slice of CPU indices as a kernel cpulist string.
func formatCPUList(cpus []int) string {
	if len(cpus) == 0 {
		return ""
	}
	// Build compact range notation.
	var parts []string
	start := cpus[0]
	prev := cpus[0]
	for i := 1; i < len(cpus); i++ {
		if cpus[i] == prev+1 {
			prev = cpus[i]
			continue
		}
		if prev == start {
			parts = append(parts, strconv.Itoa(start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", start, prev))
		}
		start = cpus[i]
		prev = cpus[i]
	}
	if prev == start {
		parts = append(parts, strconv.Itoa(start))
	} else {
		parts = append(parts, fmt.Sprintf("%d-%d", start, prev))
	}
	return strings.Join(parts, ",")
}
