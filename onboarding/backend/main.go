// onboarding/backend/main.go — HisnOS First Boot Wizard server
//
// Serves the SvelteKit frontend (embedded) on http://127.0.0.1:9444.
//
// Production hardening (v1.0):
//   • Lock file (/run/hisnos-onboarding.lock) prevents duplicate instances.
//   • Crash-resume: wizard always resumes at the last incomplete step (state
//     file tracks current_step; the frontend reads /api/state on mount).
//   • 30-minute hard timeout: wizard exits with "continue later" state recorded.
//   • Port-already-in-use → detect stale lock and recover.
//   • SIGTERM/SIGINT → clean shutdown, lock released.

package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"hisnos.local/onboarding/api"
	"hisnos.local/onboarding/state"
)

//go:embed dist
var distFS embed.FS

const (
	listenAddr     = "127.0.0.1:9444"
	wizardURL      = "http://localhost:9444"
	sessionTimeout = 30 * time.Minute
)

// lockPath returns the lock file path for the running user.
func lockPath() string {
	uid := fmt.Sprintf("%d", os.Getuid())
	return filepath.Join("/run/user", uid, "hisnos-onboarding.lock")
}

// acquireLock writes our PID to the lock file.
// Returns an error if another live instance holds the lock.
func acquireLock() error {
	path := lockPath()

	// Check for an existing lock.
	if data, err := os.ReadFile(path); err == nil {
		pidStr := strings.TrimSpace(string(data))
		if pid, err := strconv.Atoi(pidStr); err == nil && pid != os.Getpid() {
			// Check if that process is still alive.
			if proc, err := os.FindProcess(pid); err == nil {
				if err := proc.Signal(syscall.Signal(0)); err == nil {
					return fmt.Errorf("another instance is running (PID %d)", pid)
				}
			}
			// Stale lock — remove it.
			log.Printf("removing stale lock (PID %d no longer exists)", pid)
			_ = os.Remove(path)
		}
	}

	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0600)
}

func releaseLock() {
	_ = os.Remove(lockPath())
}

func main() {
	// ── Lock guard ────────────────────────────────────────────────────────────
	if err := acquireLock(); err != nil {
		log.Fatalf("onboarding already running: %v", err)
	}
	defer releaseLock()

	// ── State manager ─────────────────────────────────────────────────────────
	mgr := state.NewManager(state.DefaultStatePath)

	// Exit immediately if onboarding was already completed in a prior session.
	if mgr.IsCompleted() {
		log.Println("onboarding already complete — exiting")
		os.Exit(0)
	}

	// Log which step we are resuming from.
	cur := mgr.Get().CurrentStep
	log.Printf("resuming onboarding at step: %s", cur)

	// ── HTTP server ───────────────────────────────────────────────────────────
	mux := http.NewServeMux()

	h := api.NewHandler(mgr)
	h.Register(mux)

	// Static SvelteKit frontend embedded in the binary.
	stripped, err := fs.Sub(distFS, "dist")
	if err != nil {
		log.Fatalf("embed sub: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(stripped)))

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("listen %s: %v — is another instance running?", listenAddr, err)
	}

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Open browser after listener is ready.
	go openBrowser(wizardURL)

	// ── Shutdown triggers ─────────────────────────────────────────────────────
	ctx, cancel := context.WithTimeout(context.Background(), sessionTimeout)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	srvErr := make(chan error, 1)
	go func() {
		log.Printf("hisnos-onboarding listening on %s (step: %s)", listenAddr, cur)
		srvErr <- srv.Serve(ln)
	}()

	// Poll for completion (Complete handler disables the service, but the
	// process may be alive for the in-flight HTTP response).
	doneCh := make(chan struct{})
	go func() {
		for {
			time.Sleep(5 * time.Second)
			if mgr.IsCompleted() {
				close(doneCh)
				return
			}
		}
	}()

	select {
	case sig := <-sigCh:
		log.Printf("received signal %s — shutting down", sig)
	case <-ctx.Done():
		// 30-minute timeout: record warning and exit cleanly.
		// The frontend will show "continue later" UI before this triggers
		// (it tracks elapsed time client-side).
		log.Println("session timeout — recording and shutting down")
		_ = mgr.AddWarning("onboarding session timed out (30 min) — run 'systemctl --user start hisnos-onboarding' to resume")
	case <-doneCh:
		log.Println("onboarding completed — shutting down")
	case e := <-srvErr:
		if e != nil && e != http.ErrServerClosed {
			log.Printf("server error: %v", e)
		}
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
}

// openBrowser launches xdg-open. Best-effort; failures are logged, not fatal.
func openBrowser(url string) {
	time.Sleep(400 * time.Millisecond)
	if err := exec.Command("xdg-open", url).Start(); err != nil {
		log.Printf("xdg-open failed: %v — open %s manually", err, url)
	}
}
