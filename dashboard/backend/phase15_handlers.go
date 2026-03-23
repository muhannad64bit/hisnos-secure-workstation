// dashboard/backend/phase15_handlers.go — Phase 15 HTTP API handlers.
//
// Adds the following dashboard REST endpoints:
//   GET  /api/automation/status      — automation engine status + prediction
//   POST /api/automation/override    — suppress / reset / mark incident outcome
//   GET  /api/update/status          — OSTree deployment health + channel info
//   POST /api/update/channel         — switch update channel (staged, reboot required)
//   POST /api/update/rollback        — stage rollback to previous deployment
//   GET  /api/performance/profile    — active runtime + cmdline profile
//   POST /api/performance/profile    — switch runtime profile
//   GET  /api/modules                — module registry listing
//
// All handlers proxy to the hisnosd IPC socket (/run/user/<UID>/hisnosd.sock).
// The dashboard backend does not import any hisnosd packages directly.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func init() {
	// Register Phase 15 routes on the default mux.
	// The dashboard backend's main.go calls http.ListenAndServe using http.DefaultServeMux.
	http.HandleFunc("/api/automation/status", handleAutomationStatus)
	http.HandleFunc("/api/automation/override", handleAutomationOverride)
	http.HandleFunc("/api/update/status", handleUpdateStatus)
	http.HandleFunc("/api/update/channel", handleUpdateChannel)
	http.HandleFunc("/api/update/rollback", handleUpdateRollback)
	http.HandleFunc("/api/performance/profile", handlePerformanceProfile)
	http.HandleFunc("/api/modules", handleModuleRegistry)
}

// ── Automation ─────────────────────────────────────────────────────────────────

func handleAutomationStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	proxyIPC(w, "get_automation_status", nil)
}

func handleAutomationOverride(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var params map[string]any
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	proxyIPC(w, "override_automation", params)
}

// ── Update ─────────────────────────────────────────────────────────────────────

func handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	proxyIPC(w, "get_update_status", nil)
}

func handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var params map[string]any
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	proxyIPC(w, "set_update_channel", params)
}

func handleUpdateRollback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var params map[string]any
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		params = map[string]any{}
	}
	// Safety: require explicit confirmation in the JSON body.
	if confirm, ok := params["confirm"].(bool); !ok || !confirm {
		http.Error(w, `{"error":"body must include {\"confirm\":true}"}`, http.StatusBadRequest)
		return
	}
	proxyIPC(w, "trigger_rollback", params)
}

// ── Performance ────────────────────────────────────────────────────────────────

func handlePerformanceProfile(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		proxyIPC(w, "get_performance_profile", nil)
	case http.MethodPost:
		var params map[string]any
		if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		proxyIPC(w, "set_performance_profile", params)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── Modules ────────────────────────────────────────────────────────────────────

func handleModuleRegistry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	proxyIPC(w, "get_module_registry", nil)
}

// ── IPC proxy ──────────────────────────────────────────────────────────────────

// ipcRequest is the hisnosd JSON-RPC request format.
type ipcRequest struct {
	ID      string         `json:"id"`
	Command string         `json:"command"`
	Params  map[string]any `json:"params"`
}

// ipcResponse is the hisnosd JSON-RPC response format.
type ipcResponse struct {
	ID    string         `json:"id"`
	OK    bool           `json:"ok"`
	Data  map[string]any `json:"data,omitempty"`
	Error string         `json:"error,omitempty"`
}

// proxyIPC sends a command to the hisnosd IPC socket and writes the result as JSON.
func proxyIPC(w http.ResponseWriter, command string, params map[string]any) {
	if params == nil {
		params = map[string]any{}
	}

	socketPath := ipcSocketPath()
	result, err := callIPC(socketPath, command, params)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if !result.OK {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": result.Error})
		return
	}
	json.NewEncoder(w).Encode(result.Data)
}

// callIPC opens the hisnosd socket, sends one request, and returns the response.
func callIPC(socketPath, command string, params map[string]any) (ipcResponse, error) {
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return ipcResponse{}, fmt.Errorf("dial hisnosd IPC: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(15 * time.Second))

	req := ipcRequest{
		ID:      fmt.Sprintf("dash-%d", time.Now().UnixNano()%1_000_000),
		Command: command,
		Params:  params,
	}
	enc := json.NewEncoder(conn)
	if err := enc.Encode(req); err != nil {
		return ipcResponse{}, fmt.Errorf("encode IPC request: %w", err)
	}

	sc := bufio.NewScanner(conn)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return ipcResponse{}, fmt.Errorf("read IPC response: %w", err)
		}
		return ipcResponse{}, fmt.Errorf("IPC connection closed without response")
	}

	var resp ipcResponse
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return ipcResponse{}, fmt.Errorf("parse IPC response: %w", err)
	}
	return resp, nil
}

// ipcSocketPath returns the hisnosd Unix socket path for the current user.
func ipcSocketPath() string {
	// Honour explicit override for testing.
	if p := os.Getenv("HISNOSD_SOCKET"); p != "" {
		return p
	}
	uid := os.Getuid()
	// XDG_RUNTIME_DIR is set by systemd for the active session.
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return dir + "/hisnosd.sock"
	}
	return fmt.Sprintf("/run/user/%d/hisnosd.sock", uid)
}

// corsMiddleware is applied in main.go; reproduced here for reference.
// The dashboard serves on localhost only, so CORS is permissive.
func corsHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "http://localhost:9443")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

// __ Utility __________________________________________________________________

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]string{"error": msg})
	w.Write(b)
}

// stripPrefix returns the substring after the last "/" in path.
func stripPrefix(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}
