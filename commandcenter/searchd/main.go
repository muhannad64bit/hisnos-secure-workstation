// commandcenter/searchd/main.go — HisnOS Search Daemon
//
// Startup sequence:
//   1. Load snapshot (best-effort, non-fatal)
//   2. Load commands.json from HISNOS_COMMANDS_FILE
//   3. Full directory scan (Scanner.Run)
//   4. Start inotify Watcher goroutine
//   5. Start TelemetryTailer goroutine
//   6. Start IPC server
//   7. On SIGTERM/SIGINT: save snapshot, exit cleanly
//
// Environment variables:
//   HISNOS_SEARCH_SOCKET    — IPC socket path (default: /run/user/$UID/hisnos-search.sock)
//   HISNOS_SNAPSHOT_FILE    — index snapshot path (default: /run/user/$UID/hisnos-search-index.json)
//   HISNOS_COMMANDS_FILE    — static commands.json path (default: /etc/hisnos/commands.json)
//   HOME                    — used as scan root

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"hisnos.local/searchd/index"
	"hisnos.local/searchd/ipc"
)

func main() {
	log.SetPrefix("[searchd] ")
	log.SetFlags(log.Ltime | log.Lshortfile)

	home := os.Getenv("HOME")
	if home == "" {
		home = "/root"
	}

	socketPath := envOr("HISNOS_SEARCH_SOCKET", ipc.DefaultSocketPath())
	snapshotPath := envOr("HISNOS_SNAPSHOT_FILE", filepath.Join(runtimeDir(), "hisnos-search-index.json"))
	commandsFile := envOr("HISNOS_COMMANDS_FILE", "/etc/hisnos/commands.json")

	// --- Index ---
	idx := index.New()

	// Load snapshot (warm start).
	if err := idx.LoadSnapshot(snapshotPath); err != nil {
		log.Printf("no snapshot loaded (%v) — performing full scan", err)
	} else {
		log.Printf("snapshot loaded: %d files", idx.FileCount())
	}

	// Load static commands.
	if cmds, err := loadCommands(commandsFile); err != nil {
		log.Printf("commands.json not loaded: %v", err)
	} else {
		idx.LoadCommands(cmds)
		log.Printf("loaded %d commands", len(cmds))
	}

	// --- Full scan ---
	scanCfg := index.DefaultScanConfig(home)
	scanner := index.NewScanner(scanCfg, idx)
	scanStart := time.Now()
	scanner.Run()
	log.Printf("full scan complete in %v: %d files", time.Since(scanStart).Round(time.Millisecond), idx.FileCount())

	// --- inotify Watcher ---
	watcher, err := index.NewWatcher(idx, scanCfg)
	if err != nil {
		log.Printf("inotify unavailable: %v — incremental updates disabled", err)
	} else {
		// Register watches for all scan roots.
		for _, root := range scanCfg.Roots {
			if err := watcher.WatchDir(root); err != nil {
				log.Printf("watch %s: %v", root, err)
			}
		}
	}

	done := make(chan struct{})

	if watcher != nil {
		go watcher.Run(done)
	}

	// --- Telemetry tailer ---
	telCfg := index.DefaultTelemetryConfig()
	tailer := index.NewTelemetryTailer(telCfg, idx)
	go tailer.Run(done)
	log.Printf("telemetry tailer started")

	// --- IPC server ---
	ctx, cancel := context.WithCancel(context.Background())
	srv := ipc.NewServer(socketPath, idx)

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("IPC server listening on %s", socketPath)
		serverErr <- srv.Run(ctx)
	}()

	// --- Signal handling ---
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

	select {
	case s := <-sig:
		log.Printf("received %v — shutting down", s)
	case err := <-serverErr:
		if err != nil {
			log.Printf("IPC server error: %v", err)
		}
	}

	cancel()
	close(done)

	// Save snapshot on clean exit.
	if err := idx.SaveSnapshot(snapshotPath); err != nil {
		log.Printf("snapshot save failed: %v", err)
	} else {
		log.Printf("snapshot saved: %d files", idx.FileCount())
	}
}

func loadCommands(path string) ([]*index.CommandRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cmds []*index.CommandRecord
	if err := json.Unmarshal(data, &cmds); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return cmds, nil
}

func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return fmt.Sprintf("/run/user/%d", os.Getuid())
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
