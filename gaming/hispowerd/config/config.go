// config/config.go — hispowerd configuration loader
//
// Config file: /etc/hisnos/gaming/hispowerd.json (optional).
// All fields have safe production defaults.
// Re-read at startup only; daemon restart required for changes.

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const DefaultConfigPath = "/etc/hisnos/gaming/hispowerd.json"

// Config holds all hispowerd runtime parameters.
type Config struct {
	// Phase 1 — detection
	ScanIntervalSeconds int      `json:"scan_interval_seconds"` // default 2
	SteamDetection      bool     `json:"steam_detection"`       // default true
	ProtonDetection     bool     `json:"proton_detection"`      // default true
	GameAllowlist       []string `json:"game_allowlist"`        // executable names
	SessionLockFile     string   `json:"session_lock_file"`     // manual override path

	// Phase 2 — CPU isolation
	GamingCores []int `json:"gaming_cores"` // default [2,3,4,5,6,7]
	SystemCores []int `json:"system_cores"` // default [0,1]
	ManagedDaemons []string `json:"managed_daemons"` // service names to move to system cores

	// Phase 3 — IRQ
	GPUIRQPatterns []string `json:"gpu_irq_patterns"` // substrings in /proc/interrupts last field
	NICIRQPatterns []string `json:"nic_irq_patterns"`

	// Phase 4 — throttle
	ThreatdSlowIntervalSeconds int    `json:"threatd_slow_interval_seconds"` // default 180
	VaultIdleTimer             string `json:"vault_idle_timer_unit"`         // default hisnos-vault-idle.timer
	ThrottleDaemons            []string `json:"throttle_daemons"`            // services to reduce CPU quota

	// Phase 5 — firewall
	FastNFTFile string `json:"fast_nft_file"` // default /etc/nftables/hisnos-gaming-fast.nft

	// Phase 6 — tuning
	CPUGovernor      string   `json:"cpu_governor"`     // default "performance"
	GameNiceValue    int      `json:"game_nice_value"`  // default -10 (needs CAP_SYS_NICE)
	InjectEnvVars    bool     `json:"inject_env_vars"`  // default true
	GameEnvVars      []string `json:"game_env_vars"`    // e.g. MANGOHUD=1

	// Phase 7 — state
	GamingStateFile      string `json:"gaming_state_file"`       // /var/lib/hisnos/gaming-state.json
	ControlPlaneStateFile string `json:"control_plane_state_file"` // /var/lib/hisnos/core-state.json
	HisnosdSocket         string `json:"hisnosd_socket"`           // /run/user/<uid>/hisnosd.sock

	// Internal paths
	LogDir string `json:"log_dir"` // /var/log/hisnos/gaming
}

// Default returns production-safe defaults.
func Default() *Config {
	home := os.Getenv("HOME")
	uid := os.Getenv("UID")
	if uid == "" {
		uid = "1000"
	}
	return &Config{
		ScanIntervalSeconds: 2,
		SteamDetection:      true,
		ProtonDetection:     true,
		GameAllowlist: []string{
			"hl2.exe", "dota2", "csgo", "cs2", "cyberpunk2077.exe",
			"witcher3.exe", "doom64.exe", "eldenring.exe",
			"GenshinImpact.exe", "valorant.exe", "elden_ring.exe",
		},
		SessionLockFile: filepath.Join(home, ".local", "share", "hisnos", "gaming", "session.lock"),

		GamingCores: []int{2, 3, 4, 5, 6, 7},
		SystemCores: []int{0, 1},
		ManagedDaemons: []string{
			"hisnos-threatd.service",
			"hisnos-logd.service",
			"hisnos-dashboard.service",
			"hisnos-vault-watcher.service",
		},

		GPUIRQPatterns: []string{"nvidia", "amdgpu", "i915", "radeon", "nouveau"},
		NICIRQPatterns: []string{"eth", "eno", "enp", "ens", "wl", "iwlwifi", "rtw", "r8169"},

		ThreatdSlowIntervalSeconds: 180,
		VaultIdleTimer:             "hisnos-vault-idle.timer",
		ThrottleDaemons: []string{
			"hisnos-threatd.service",
			"hisnos-logd.service",
		},

		FastNFTFile: "/etc/nftables/hisnos-gaming-fast.nft",

		CPUGovernor:   "performance",
		GameNiceValue: -5, // -5 reachable without CAP_SYS_NICE from nice 0 baseline
		InjectEnvVars: true,
		GameEnvVars: []string{
			"MANGOHUD=1",
			"DXVK_ASYNC=1",
			"__GL_THREADED_OPTIMIZATIONS=1",
			"mesa_glthread=true",
		},

		GamingStateFile:       "/var/lib/hisnos/gaming-state.json",
		ControlPlaneStateFile: "/var/lib/hisnos/core-state.json",
		HisnosdSocket:         "/run/user/" + uid + "/hisnosd.sock",

		LogDir: "/var/log/hisnos/gaming",
	}
}

// Load reads the config file (if present) and merges with defaults.
// Missing file is not an error — defaults are used.
func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil // use defaults
		}
		return cfg, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// GamingCoreMask returns a uint64 bitmask with gaming core bits set.
func (c *Config) GamingCoreMask() uint64 {
	return coreMask(c.GamingCores)
}

// SystemCoreMask returns a uint64 bitmask with system core bits set.
func (c *Config) SystemCoreMask() uint64 {
	return coreMask(c.SystemCores)
}

func coreMask(cores []int) uint64 {
	var mask uint64
	for _, cpu := range cores {
		if cpu >= 0 && cpu < 64 {
			mask |= 1 << uint(cpu)
		}
	}
	return mask
}
