// main.go — HisnOS Governance Dashboard backend entry point
//
// Architecture:
//   - HTTP/1.1 server over TCP (127.0.0.1:7374 by default)
//   - systemd socket activation: if SD_LISTEN_FDS=1, inherits fd 3 from systemd
//   - All API routes are under /api/; root "/" serves the embedded SvelteKit SPA
//   - Destructive actions (vault lock/mount, firewall reload, update apply/rollback)
//     require an X-HisnOS-Confirm header with the session token from GET /api/confirm/token
//
// Security model:
//   - Binds to loopback only — no external exposure
//   - Runs as the unprivileged user (same UID as vault, systemctl --user)
//   - Subprocess calls use absolute paths, no shell interpolation
//   - Confirmation tokens prevent CSRF from any open browser tab
//
// Static frontend:
//   The built SvelteKit dist/ is embedded via web/static.go (//go:embed all:dist).
//   bootstrap-installer.sh copies frontend/dist/ → backend/web/dist/ before go build.
//   SPA fallback: any path not found in the FS is served index.html so the
//   SvelteKit client router handles navigation.
//
// Environment variables:
//   HISNOS_DASHBOARD_ADDR  — listen address (default: 127.0.0.1:7374)
//   HISNOS_DIR             — HisnOS install dir (default: $HOME/.local/share/hisnos)
//   LOG_LEVEL              — debug|info|warn|error (default: info)

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"hisnos.local/dashboard/api"
	"hisnos.local/dashboard/web"
)

const (
	defaultListenAddr = "127.0.0.1:7374"
	shutdownTimeout   = 10 * time.Second
)

// config holds runtime configuration loaded from environment variables.
type config struct {
	listenAddr string
	hisnos     string
	logLevel   slog.Level
}

func loadConfig() config {
	c := config{
		listenAddr: envOr("HISNOS_DASHBOARD_ADDR", defaultListenAddr),
		hisnos:     envOr("HISNOS_DIR", filepath.Join(os.Getenv("HOME"), ".local", "share", "hisnos")),
		logLevel:   slog.LevelInfo,
	}
	switch envOr("LOG_LEVEL", "info") {
	case "debug":
		c.logLevel = slog.LevelDebug
	case "warn":
		c.logLevel = slog.LevelWarn
	case "error":
		c.logLevel = slog.LevelError
	}
	return c
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// makeListener returns a net.Listener using systemd socket activation if
// SD_LISTEN_FDS=1 is set, otherwise binds TCP on addr.
//
// Socket activation: systemd creates the socket before the service starts and
// passes it as fd 3 (LISTEN_FDS_START). The service inherits it, allowing
// zero-downtime restarts and on-demand activation.
func makeListener(addr string) (net.Listener, error) {
	if nfds, _ := strconv.Atoi(os.Getenv("SD_LISTEN_FDS")); nfds >= 1 {
		// Inherit pre-activated socket from systemd (always fd 3)
		f := os.NewFile(uintptr(3), "systemd-socket")
		if f == nil {
			return nil, fmt.Errorf("SD_LISTEN_FDS=1 but fd 3 is invalid")
		}
		ln, err := net.FileListener(f)
		f.Close() // FileListener dups the fd; close the original
		if err != nil {
			return nil, fmt.Errorf("socket activation: %w", err)
		}
		return ln, nil
	}
	// Fallback: bind TCP on loopback
	return net.Listen("tcp", addr)
}

func main() {
	cfg := loadConfig()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: cfg.logLevel,
	}))
	slog.SetDefault(logger)

	slog.Info("hisnos-dashboard starting",
		"hisnos_dir", cfg.hisnos,
		"addr", cfg.listenAddr,
	)

	h, err := api.NewHandler(cfg.hisnos)
	if err != nil {
		slog.Error("failed to create API handler", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()

	// API routes — must be registered first so Go 1.22 specificity rules
	// ensure "/api/..." beats the root catch-all "/" for API paths.
	h.RegisterRoutes(mux)

	// Embedded SvelteKit SPA — mounted at "/" after API routes.
	// The spaHandler serves static files and falls back to index.html for any
	// path that doesn't exist in the embedded FS (client-side routing support).
	staticHandler, err := web.Handler()
	if err != nil {
		slog.Error("failed to initialise static file server", "err", err)
		os.Exit(1)
	}
	mux.Handle("/", staticHandler)
	slog.Info("hisnos-dashboard static frontend mounted")

	ln, err := makeListener(cfg.listenAddr)
	if err != nil {
		slog.Error("failed to create listener", "err", err, "addr", cfg.listenAddr)
		os.Exit(1)
	}

	srv := &http.Server{
		Handler: mux,
		// Short read timeout; SSE and prepare/stream routes use ResponseController
		// to extend their own write deadlines.
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 10 * time.Minute, // SSE streams may run for minutes
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown on SIGTERM or SIGINT
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		<-ctx.Done()
		slog.Info("shutdown signal received")
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			slog.Error("shutdown error", "err", err)
		}
	}()

	slog.Info("listening", "addr", ln.Addr().String())
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
	slog.Info("server stopped")
}
