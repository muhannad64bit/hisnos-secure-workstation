// core/performance/thermal_controller.go — Thermal and power feedback controller.
//
// Reads CPU/GPU temperature sensors via hwmon and detects frequency throttling.
// Under thermal stress: reduces CPU cgroup quotas for non-critical hisnosd
// background services to free thermal headroom for the game process.
//
// Throttle detection:
//   scaling_cur_freq < 85% of cpuinfo_max_freq → throttling
//   temp_input > (crit - 15°C) → pre-throttle warning
//
// Response tiers:
//   Tier 0 (nominal):     no action
//   Tier 1 (warm >75°C):  reduce hisnosd background to 10% CPUQuota
//   Tier 2 (throttle):    reduce to 5% CPUQuota, emit alert, suggest perf profile
//   Tier 3 (critical):    request profile downgrade to balanced, force GC
//
// CPUQuota is written to /sys/fs/cgroup/system.slice/hisnosd.service/cpu.max
// using the cgroup v2 format: "$quota $period" (e.g. "50000 1000000" = 5%).
package performance

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	thermalWarnThresholdOffset = 15000  // millidegrees below critical
	throttleFreqRatio          = 0.85   // cur_freq < 85% of max → throttling
	thermalScanInterval        = 5 * time.Second
	cgroupBase                 = "/sys/fs/cgroup"
)

// ThermalTier classifies the current thermal state.
type ThermalTier int

const (
	TierNominal  ThermalTier = 0
	TierWarm     ThermalTier = 1
	TierThrottle ThermalTier = 2
	TierCritical ThermalTier = 3
)

var tierName = map[ThermalTier]string{
	TierNominal: "nominal", TierWarm: "warm",
	TierThrottle: "throttle", TierCritical: "critical",
}

// sensor describes a discovered hwmon temperature sensor.
type sensor struct {
	inputPath string // e.g. /sys/class/hwmon/hwmon0/temp1_input
	critPath  string // e.g. /sys/class/hwmon/hwmon0/temp1_crit  (may be "")
	label     string // e.g. "Package id 0" or hwmon name
}

// ThermalController monitors system thermals and adjusts cgroup CPU budgets.
type ThermalController struct {
	mu       sync.Mutex
	sensors  []sensor
	lastTier ThermalTier

	// Callback: called when controller recommends a profile downgrade.
	onDowngrade func(reason string)
	emit        func(string, string, map[string]any)
}

// NewThermalController discovers sensors and initialises the controller.
func NewThermalController(
	onDowngrade func(reason string),
	emit func(string, string, map[string]any),
) *ThermalController {
	if emit == nil {
		emit = func(_, _ string, _ map[string]any) {}
	}
	tc := &ThermalController{onDowngrade: onDowngrade, emit: emit}
	tc.discoverSensors()
	log.Printf("[thermal] discovered %d sensors", len(tc.sensors))
	return tc
}

// Tick performs one evaluation cycle. Call every thermalScanInterval.
func (tc *ThermalController) Tick() {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	maxTemp, critTemp := tc.maxTemperature()
	throttling := tc.isThrottling()

	tier := tc.classifyTier(maxTemp, critTemp, throttling)

	if tier != tc.lastTier {
		log.Printf("[thermal] tier %s → %s (temp=%d°C throttle=%v)",
			tierName[tc.lastTier], tierName[tier], maxTemp/1000, throttling)
		tc.applyTier(tier, maxTemp, throttling)
		tc.lastTier = tier
	}
}

// CurrentTier returns the last evaluated thermal tier.
func (tc *ThermalController) CurrentTier() ThermalTier {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.lastTier
}

// Status returns a summary for IPC.
func (tc *ThermalController) Status() map[string]any {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	maxTemp, critTemp := tc.maxTemperature()
	return map[string]any{
		"tier":         tierName[tc.lastTier],
		"max_temp_c":   maxTemp / 1000,
		"crit_temp_c":  critTemp / 1000,
		"throttling":   tc.isThrottling(),
		"sensor_count": len(tc.sensors),
	}
}

// maxTemperature returns (maxTempMilliC, critTempMilliC) from all discovered sensors.
func (tc *ThermalController) maxTemperature() (int64, int64) {
	var maxTemp, critTemp int64
	for _, s := range tc.sensors {
		v, err := readMilliDeg(s.inputPath)
		if err != nil {
			continue
		}
		if v > maxTemp {
			maxTemp = v
		}
		if s.critPath != "" {
			c, err := readMilliDeg(s.critPath)
			if err == nil && c > critTemp {
				critTemp = c
			}
		}
	}
	if critTemp == 0 {
		critTemp = 100000 // default 100°C critical if not specified
	}
	return maxTemp, critTemp
}

// isThrottling checks if any CPU is running below 85% of its rated max frequency.
func (tc *ThermalController) isThrottling() bool {
	cpus, _ := onlineCPUs()
	for _, cpu := range cpus {
		base := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq", cpu)
		cur, err1 := readFreq(filepath.Join(base, "scaling_cur_freq"))
		max, err2 := readFreq(filepath.Join(base, "cpuinfo_max_freq"))
		if err1 != nil || err2 != nil || max == 0 {
			continue
		}
		if float64(cur)/float64(max) < throttleFreqRatio {
			return true
		}
	}
	return false
}

// classifyTier maps temperature and throttle state to a ThermalTier.
func (tc *ThermalController) classifyTier(maxTemp, critTemp int64, throttling bool) ThermalTier {
	warnTemp := critTemp - thermalWarnThresholdOffset
	switch {
	case throttling || maxTemp >= critTemp-5000:
		return TierCritical
	case maxTemp >= warnTemp:
		return TierThrottle
	case maxTemp > 75000: // 75°C
		return TierWarm
	default:
		return TierNominal
	}
}

// applyTier enacts the thermal response for the given tier.
func (tc *ThermalController) applyTier(tier ThermalTier, maxTemp int64, throttling bool) {
	quota := "" // cgroup v2 cpu.max: "quota period" or "max period"
	switch tier {
	case TierNominal:
		quota = "200000 1000000" // 20% — restore headroom
	case TierWarm:
		quota = "100000 1000000" // 10%
	case TierThrottle:
		quota = "50000 1000000"  // 5%
		tc.emit("performance", "thermal_throttle_detected", map[string]any{
			"max_temp_c": maxTemp / 1000, "throttling": throttling,
		})
	case TierCritical:
		quota = "25000 1000000" // 2.5%
		tc.emit("performance", "thermal_critical", map[string]any{
			"max_temp_c": maxTemp / 1000, "action": "profile_downgrade_requested",
		})
		if tc.onDowngrade != nil {
			tc.onDowngrade(fmt.Sprintf("thermal critical: %d°C", maxTemp/1000))
		}
	}
	if quota != "" {
		tc.setCgroupCPUMax("system.slice/hisnosd.service", quota)
	}
}

// setCgroupCPUMax writes to cgroup v2 cpu.max for the given slice path.
func (tc *ThermalController) setCgroupCPUMax(slice, quota string) {
	path := filepath.Join(cgroupBase, slice, "cpu.max")
	if err := writeFile(path, quota); err != nil {
		log.Printf("[thermal] WARN: set cpu.max %s: %v", slice, err)
	}
}

// discoverSensors walks /sys/class/hwmon to find temperature sensor paths.
func (tc *ThermalController) discoverSensors() {
	hwmonBase := "/sys/class/hwmon"
	entries, err := os.ReadDir(hwmonBase)
	if err != nil {
		return
	}
	for _, e := range entries {
		base := filepath.Join(hwmonBase, e.Name())
		name, _ := readFile(filepath.Join(base, "name"))
		// Find all temp*_input files.
		subEntries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, se := range subEntries {
			seName := se.Name()
			if !strings.HasSuffix(seName, "_input") || !strings.HasPrefix(seName, "temp") {
				continue
			}
			inputPath := filepath.Join(base, seName)
			prefix := strings.TrimSuffix(seName, "_input")
			critPath := filepath.Join(base, prefix+"_crit")
			if _, err := os.Stat(critPath); err != nil {
				critPath = ""
			}
			label, _ := readFile(filepath.Join(base, prefix+"_label"))
			if label == "" {
				label = name
			}
			tc.sensors = append(tc.sensors, sensor{
				inputPath: inputPath, critPath: critPath, label: label,
			})
		}
	}
}

func readMilliDeg(path string) (int64, error) {
	s, err := readFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(s, 10, 64)
}

func readFreq(path string) (int64, error) {
	s, err := readFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(s, 10, 64)
}
