// api/api.go — HisnOS dashboard API handler root
//
// Handler is the central type that holds all API state:
//   - HisnOS install directory path
//   - Absolute paths to HisnOS shell scripts (validated at construction)
//   - Session-scoped confirm token (generated at startup via crypto/rand)
//
// Confirmation token pattern:
//   Destructive actions (vault lock/mount, firewall reload, update apply/rollback)
//   require the caller to send X-HisnOS-Confirm: <token> where token is fetched
//   from GET /api/confirm/token. This prevents CSRF from any co-resident tab.
//   The token is generated once per daemon lifetime (not per-request).
//
// Route registration uses Go 1.22 method+path patterns.

package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	dashipc "hisnos.local/dashboard/ipc"
	cpstate "hisnos.local/dashboard/state"
)

const (
	defaultAuditDir  = "/var/lib/hisnos/audit"
	defaultThreatDir = "/var/lib/hisnos"
)

// Scripts holds validated absolute paths to HisnOS shell scripts.
type Scripts struct {
	Vault    string // hisnos-vault.sh
	Update   string // hisnos-update.sh
	Preflight string // hisnos-update-preflight.sh
}

// Handler is the root API handler; all route methods are attached to it.
type Handler struct {
	hisnos    string  // HISNOS_DIR (e.g., ~/.local/share/hisnos)
	auditDir  string  // audit log directory (default: /var/lib/hisnos/audit)
	threatDir string  // threat state directory (default: /var/lib/hisnos)
	scripts   Scripts // absolute paths to HisnOS scripts
	token     string  // session-scoped confirmation token

	stateMgr *cpstate.Manager

	// hisnosd is the optional IPC client for the Core Control Runtime.
	// When non-nil, mutating actions (vault lock, firewall reload, lab
	// start/stop, gaming start/stop, mode transitions) are routed through
	// hisnosd instead of being executed directly. Falls back to direct exec
	// when hisnosd is unavailable.
	hisnosd *dashipc.Client
}

// NewHandler creates a Handler rooted at hisnos (HISNOS_DIR).
// Missing scripts produce warnings (not errors) so the daemon starts even on
// partially-deployed systems; individual handlers return 503 if scripts are absent.
func NewHandler(hisnos string) (*Handler, error) {
	scripts := Scripts{
		Vault:     filepath.Join(hisnos, "vault", "hisnos-vault.sh"),
		Update:    filepath.Join(hisnos, "update", "hisnos-update.sh"),
		Preflight: filepath.Join(hisnos, "update", "hisnos-update-preflight.sh"),
	}

	for name, path := range map[string]string{
		"vault":     scripts.Vault,
		"update":    scripts.Update,
		"preflight": scripts.Preflight,
	} {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "[hisnos-dashboard] WARNING: %s script not found: %s\n", name, path)
		}
	}

	token, err := generateToken()
	if err != nil {
		return nil, fmt.Errorf("generate confirm token: %w", err)
	}

	auditDir := os.Getenv("HISNOS_AUDIT_DIR")
	if auditDir == "" {
		auditDir = defaultAuditDir
	}

	threatDir := os.Getenv("HISNOS_THREAT_DIR")
	if threatDir == "" {
		threatDir = defaultThreatDir
	}

	// Attempt to connect to hisnosd. Non-fatal: dashboard works without it.
	var hisnosdClient *dashipc.Client
	socketPath := dashipc.DefaultSocketPath()
	client := dashipc.NewClient(socketPath)
	if client.Available() {
		hisnosdClient = client
		log.Printf("[hisnos-dashboard] hisnosd IPC connected: %s", socketPath)
	} else {
		log.Printf("[hisnos-dashboard] hisnosd socket not found at %s — using direct exec fallback", socketPath)
	}

	return &Handler{
		hisnos:    hisnos,
		auditDir:  auditDir,
		threatDir: threatDir,
		scripts:   scripts,
		token:     token,
		stateMgr:  cpstate.NewManager(),
		hisnosd:   hisnosdClient,
	}, nil
}

// hisnosdAvailable returns true if the hisnosd IPC client is connected.
// Handlers use this to decide between IPC routing and direct exec fallback.
func (h *Handler) hisnosdAvailable() bool {
	return h.hisnosd != nil && h.hisnosd.Available()
}

func generateToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// RegisterRoutes registers all API routes on mux using Go 1.22 method+path patterns.
//
// Route taxonomy:
//   GET-only:  safe reads (vault status, firewall status, kernel status, update status)
//   POST+confirm: destructive mutations (vault lock/mount, firewall reload, update apply/rollback)
//   SSE:       streaming endpoints (update prepare, journal stream)
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// ── Confirm token (required for destructive actions) ────────────────────
	mux.HandleFunc("GET /api/confirm/token", h.ConfirmToken)

	// ── Vault ───────────────────────────────────────────────────────────────
	mux.HandleFunc("GET /api/vault/status",    h.VaultStatus)
	mux.HandleFunc("POST /api/vault/lock",     h.VaultLock)
	mux.HandleFunc("POST /api/vault/mount",    h.VaultMount)
	mux.HandleFunc("GET /api/vault/telemetry", h.VaultTelemetry)

	// ── Firewall ────────────────────────────────────────────────────────────
	mux.HandleFunc("GET /api/firewall/status",  h.FirewallStatus)
	mux.HandleFunc("POST /api/firewall/reload", h.FirewallReload)

	// ── Kernel ──────────────────────────────────────────────────────────────
	mux.HandleFunc("GET /api/kernel/status", h.KernelStatus)

	// ── Update ──────────────────────────────────────────────────────────────
	mux.HandleFunc("GET /api/update/status",    h.UpdateStatus)
	mux.HandleFunc("POST /api/update/check",    h.UpdateCheck)
	mux.HandleFunc("POST /api/update/prepare",  h.UpdatePrepare)  // SSE
	mux.HandleFunc("POST /api/update/apply",    h.UpdateApply)
	mux.HandleFunc("POST /api/update/rollback", h.UpdateRollback)
	mux.HandleFunc("POST /api/update/validate", h.UpdateValidate)

	// ── Lab isolation runtime ────────────────────────────────────────────────
	mux.HandleFunc("GET /api/lab/status",           h.LabStatus)
	mux.HandleFunc("POST /api/lab/start",           h.LabStart)
	mux.HandleFunc("POST /api/lab/stop",            h.LabStop)
	mux.HandleFunc("GET /api/lab/network-profile",  h.LabNetworkProfileGet)
	mux.HandleFunc("POST /api/lab/network-profile", h.LabNetworkProfileSet)

	// ── Audit pipeline ───────────────────────────────────────────────────────
	mux.HandleFunc("GET /api/audit/summary",         h.AuditSummary)
	mux.HandleFunc("GET /api/audit/sessions",        h.AuditSessions)
	mux.HandleFunc("GET /api/audit/firewall-events", h.AuditFirewallEvents)

	// ── Threat intelligence ───────────────────────────────────────────────────
	mux.HandleFunc("GET /api/threat/status",   h.ThreatStatus)
	mux.HandleFunc("GET /api/threat/timeline", h.ThreatTimeline)

	// ── Gaming mode ───────────────────────────────────────────────────────────
	mux.HandleFunc("GET /api/gaming/status",  h.GamingStatus)
	mux.HandleFunc("POST /api/gaming/start",  h.GamingStart)
	mux.HandleFunc("POST /api/gaming/stop",   h.GamingStop)

	// ── Journal ─────────────────────────────────────────────────────────────
	mux.HandleFunc("GET /api/journal/stream", h.JournalStream) // SSE

	// ── Utility ─────────────────────────────────────────────────────────────
	mux.HandleFunc("GET /api/health", h.Health)
	mux.HandleFunc("GET /api/system/state",   h.SystemState)
	mux.HandleFunc("POST /api/system/mode",   h.SystemModeTransition)
	mux.HandleFunc("POST /api/system/reboot", h.SystemReboot)
	// Note: "GET /" is NOT registered here — main.go mounts the embedded
	// SvelteKit static handler at "/" after RegisterRoutes returns.
}

// ── Shared helpers ────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeErrorCode(w http.ResponseWriter, status int, msg string, errorCode string) {
	writeJSON(w, status, map[string]string{
		"error":      msg,
		"error_code": errorCode,
	})
}

// requireConfirm checks the X-HisnOS-Confirm header against the session token.
// Returns true if confirmed; writes 403 and returns false if not.
func (h *Handler) requireConfirm(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-HisnOS-Confirm") != h.token {
		writeError(w, http.StatusForbidden,
			"destructive action requires X-HisnOS-Confirm header — fetch token from GET /api/confirm/token")
		return false
	}
	return true
}

// scriptAvailable returns false and writes a 503 if the given script path
// is not executable, preventing confusing "exec failed" errors downstream.
func scriptAvailable(w http.ResponseWriter, path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.Mode()&0111 == 0 {
		writeError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("script not available: %s", filepath.Base(path)))
		return false
	}
	return true
}

// ── Utility endpoints ─────────────────────────────────────────────────────────

func (h *Handler) ConfirmToken(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"token": h.token})
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":     "ok",
		"hisnos_dir": h.hisnos,
	})
}

func (h *Handler) StatusPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8"><title>HisnOS Dashboard</title>
<style>body{font-family:monospace;max-width:800px;margin:2rem auto;padding:0 1rem}
a{color:#4a9}code{background:#111;padding:.1em .3em;border-radius:3px}</style>
</head><body>
<h1>HisnOS Governance Dashboard</h1>
<p>Backend API is running. SvelteKit frontend not yet deployed.</p>
<p>HisnOS dir: <code>%s</code></p>
<h2>API Endpoints</h2>
<ul>
<li><a href="/api/health">GET /api/health</a></li>
<li><a href="/api/confirm/token">GET /api/confirm/token</a></li>
<li><a href="/api/vault/status">GET /api/vault/status</a></li>
<li><a href="/api/vault/telemetry">GET /api/vault/telemetry</a></li>
<li><a href="/api/firewall/status">GET /api/firewall/status</a></li>
<li><a href="/api/kernel/status">GET /api/kernel/status</a></li>
<li><a href="/api/update/status">GET /api/update/status</a></li>
<li><a href="/api/journal/stream">GET /api/journal/stream (SSE)</a></li>
</ul>
</body></html>
`, h.hisnos)
}
