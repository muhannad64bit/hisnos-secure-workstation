// core/ecosystem/manager.go — Top-level ecosystem coordinator.
//
// Owns the update manager, channel manager, module registry, telemetry client,
// and fleet identity. Exposes IPC handlers for:
//   get_update_status, set_update_channel, trigger_rollback,
//   get_module_registry, register_module
//
// Integration with hisnosd:
//   Call m.IPCHandlers() and register each handler with ipc.Server.RegisterCommand.
//   Call m.Beat() to satisfy watchdog heartbeat.
//   Start m.RunPeriodicUpdateCheck(ctx) as a goroutine for weekly update checks.
package ecosystem

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Manager is the single entry point for all ecosystem operations.
type Manager struct {
	update   *UpdateManager
	channel  *ChannelManager
	registry *ModuleRegistry
	telemetry *TelemetryClient
	identity  *FleetIdentity

	stateDir string
	emit     func(category, msg string, data map[string]any)
}

// NewManager initialises all ecosystem subsystems.
// stateDir: /var/lib/hisnos
// emit: structured event callback (wired to SecurityEventStream in main.go)
func NewManager(stateDir string, emit func(string, string, map[string]any)) (*Manager, error) {
	if emit == nil {
		emit = func(_, _ string, _ map[string]any) {}
	}

	identity, err := LoadFleetIdentity(stateDir)
	if err != nil {
		return nil, fmt.Errorf("fleet identity: %w", err)
	}

	ch := NewChannelManager(stateDir)
	upd := NewUpdateManager(stateDir, ch)
	reg := NewModuleRegistry(stateDir)
	tel := NewTelemetryClient(stateDir, identity)

	return &Manager{
		update:    upd,
		channel:   ch,
		registry:  reg,
		telemetry: tel,
		identity:  identity,
		stateDir:  stateDir,
		emit:      emit,
	}, nil
}

// RunPeriodicUpdateCheck performs a weekly background update availability check.
// Emits a structured event when an update is found. Does not auto-apply.
func (m *Manager) RunPeriodicUpdateCheck(ctx context.Context) {
	ticker := time.NewTicker(7 * 24 * time.Hour)
	defer ticker.Stop()

	// Run once at startup (after a short delay to let the system settle).
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Minute):
	}
	m.checkForUpdates()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkForUpdates()
		}
	}
}

func (m *Manager) checkForUpdates() {
	available, commit, err := m.update.CheckAvailable()
	if err != nil {
		log.Printf("[ecosystem] update check failed: %v", err)
		return
	}
	if available {
		log.Printf("[ecosystem] update available: %s", commit)
		m.emit("ecosystem", "update_available", map[string]any{
			"channel":        m.channel.Current(),
			"pending_commit": commit,
		})
		m.telemetry.Record("update_available", map[string]any{"commit": commit})
	}
}

// Beat is a no-op heartbeat for watchdog registration.
func (m *Manager) Beat() {}

// IPCHandlers returns all IPC command handlers for Phase 15 ecosystem commands.
func (m *Manager) IPCHandlers() map[string]func(map[string]any) (map[string]any, error) {
	return map[string]func(map[string]any) (map[string]any, error){

		"get_update_status": func(_ map[string]any) (map[string]any, error) {
			status, err := m.update.Status()
			if err != nil {
				return nil, err
			}
			b, _ := json.Marshal(status)
			var out map[string]any
			json.Unmarshal(b, &out)
			return out, nil
		},

		"set_update_channel": func(params map[string]any) (map[string]any, error) {
			channel, _ := params["channel"].(string)
			if channel == "" {
				return nil, fmt.Errorf("params.channel required (stable|beta|hardened)")
			}
			if err := m.channel.Switch(channel); err != nil {
				return nil, err
			}
			m.emit("ecosystem", "channel_switched", map[string]any{
				"channel": channel, "reboot_required": true,
			})
			m.telemetry.Record("channel_switch", map[string]any{"channel": channel})
			return map[string]any{
				"channel":         channel,
				"reboot_required": true,
				"message":         "Channel staged. Reboot to activate.",
			}, nil
		},

		"trigger_rollback": func(params map[string]any) (map[string]any, error) {
			confirm, _ := params["confirm"].(bool)
			if err := m.update.Rollback(confirm); err != nil {
				return nil, err
			}
			m.emit("ecosystem", "rollback_staged", map[string]any{"reboot_required": true})
			return map[string]any{
				"rolled_back":     true,
				"reboot_required": true,
				"message":         "Rollback staged. Reboot to activate.",
			}, nil
		},

		"get_module_registry": func(_ map[string]any) (map[string]any, error) {
			return m.registry.StatusMap(), nil
		},

		"register_module": func(params map[string]any) (map[string]any, error) {
			id, _ := params["id"].(string)
			name, _ := params["name"].(string)
			version, _ := params["version"].(string)
			sha256, _ := params["sha256"].(string)
			installPath, _ := params["install_path"].(string)
			if id == "" || name == "" {
				return nil, fmt.Errorf("params.id and params.name are required")
			}
			manifest := ModuleManifest{
				ID:          id,
				Name:        name,
				Version:     version,
				SHA256:      sha256,
				InstallPath: installPath,
				Enabled:     false,
			}
			if err := m.registry.Register(manifest); err != nil {
				return nil, err
			}
			return map[string]any{"registered": true, "id": id}, nil
		},

		"get_fleet_identity": func(_ map[string]any) (map[string]any, error) {
			return map[string]any{
				"fleet_id":       m.identity.FleetID,
				"channel":        m.identity.Channel,
				"hisnos_version": m.identity.HisnVersion,
				"telemetry_enabled": m.telemetry.Enabled(),
			}, nil
		},
	}
}

// helpers shared across ecosystem package files.

func writeAtomic(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".eco-tmp-")
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

func mustMarshalEco(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// readLineConf reads a key=value config file and returns the value for key.
func readLineConf(path, key string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
