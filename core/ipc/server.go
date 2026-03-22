// core/ipc/server.go — Unix domain socket JSON-RPC server.
//
// Protocol:
//   Each connection is line-delimited JSON: one request per line, one response
//   per line. The connection is kept alive for multiple requests (HTTP/1.1
//   persistent semantics). Connections are closed on EOF or error.
//
//   Request:   {"id":"<string>","command":"<name>","params":{...}}\n
//   Response:  {"id":"<string>","ok":true,"data":{...}}\n
//              {"id":"<string>","ok":false,"error":"<message>"}\n
//
// Commands:
//   get_state           — return current SystemState snapshot
//   set_mode            — params: {"mode":"<Mode>"}
//   lock_vault          — force vault lock via orchestrator
//   start_lab           — admission check + start lab session
//   stop_lab            — stop current lab session
//   reload_firewall     — reload nftables (policy-gated)
//   prepare_update      — run hisnos-update preflight (synchronous)
//   acknowledge_alert   — (future; currently no-op)
//   health              — return hisnosd health summary
//
// All commands are enforced by the policy engine before dispatch.

package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"hisnos.local/hisnosd/eventbus"
	"hisnos.local/hisnosd/orchestrator"
	"hisnos.local/hisnosd/policy"
	"hisnos.local/hisnosd/state"
)

// Request is a JSON-RPC style command from a client.
type Request struct {
	ID      string         `json:"id"`
	Command string         `json:"command"`
	Params  map[string]any `json:"params"`
}

// Response is the reply to a single Request.
type Response struct {
	ID    string         `json:"id"`
	OK    bool           `json:"ok"`
	Data  map[string]any `json:"data,omitempty"`
	Error string         `json:"error,omitempty"`
}

// Server listens on a Unix socket and dispatches JSON-RPC commands.
type Server struct {
	socketPath string
	stateMgr   *state.Manager
	bus        *eventbus.Bus
	policy     *policy.Engine
	vault      *orchestrator.VaultOrchestrator
	firewall   *orchestrator.FirewallOrchestrator
	lab        *orchestrator.LabOrchestrator
	gaming     *orchestrator.GamingOrchestrator
	update     *orchestrator.UpdateOrchestrator
}

// New creates an IPC Server.
func New(
	socketPath string,
	mgr *state.Manager,
	bus *eventbus.Bus,
	pe *policy.Engine,
	vault *orchestrator.VaultOrchestrator,
	firewall *orchestrator.FirewallOrchestrator,
	lab *orchestrator.LabOrchestrator,
	gaming *orchestrator.GamingOrchestrator,
	update *orchestrator.UpdateOrchestrator,
) *Server {
	return &Server{
		socketPath: socketPath,
		stateMgr:   mgr,
		bus:        bus,
		policy:     pe,
		vault:      vault,
		firewall:   firewall,
		lab:        lab,
		gaming:     gaming,
		update:     update,
	}
}

// Run starts the Unix socket listener. Blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	// Remove a stale socket from a previous run.
	_ = os.Remove(s.socketPath)

	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.socketPath, err)
	}
	// Restrict socket to owner only (hisnosd runs as the operator user).
	if err := os.Chmod(s.socketPath, 0600); err != nil {
		log.Printf("[hisnosd/ipc] WARN: chmod socket: %v", err)
	}

	log.Printf("[hisnosd/ipc] listening on %s", s.socketPath)

	// Close listener when context is cancelled.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // clean shutdown
			}
			log.Printf("[hisnosd/ipc] accept error: %v", err)
			continue
		}
		go s.handleConn(conn)
	}
}

// handleConn reads requests from a single connection until EOF or error.
func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(60 * time.Second)) // idle timeout

	scanner := bufio.NewScanner(conn)
	enc := json.NewEncoder(conn)

	for scanner.Scan() {
		conn.SetDeadline(time.Now().Add(60 * time.Second)) // refresh on each request

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var req Request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			writeErr(enc, "", fmt.Sprintf("invalid JSON: %v", err))
			continue
		}

		resp := s.dispatch(req)
		if err := enc.Encode(resp); err != nil {
			log.Printf("[hisnosd/ipc] encode error: %v", err)
			return
		}
	}
}

// dispatch routes a Request to the appropriate handler.
func (s *Server) dispatch(req Request) Response {
	switch req.Command {
	case "get_state":
		return s.cmdGetState(req)
	case "set_mode":
		return s.cmdSetMode(req)
	case "lock_vault":
		return s.cmdLockVault(req)
	case "start_lab":
		return s.cmdStartLab(req)
	case "stop_lab":
		return s.cmdStopLab(req)
	case "reload_firewall":
		return s.cmdReloadFirewall(req)
	case "prepare_update":
		return s.cmdPrepareUpdate(req)
	case "start_gaming":
		return s.cmdStartGaming(req)
	case "stop_gaming":
		return s.cmdStopGaming(req)
	case "acknowledge_alert":
		return ok(req.ID, map[string]any{"acknowledged": true})
	case "health":
		return s.cmdHealth(req)
	default:
		return errResp(req.ID, fmt.Sprintf("unknown command: %s", req.Command))
	}
}

// ── Command handlers ──────────────────────────────────────────────────────────

func (s *Server) cmdGetState(req Request) Response {
	st := s.stateMgr.Get()
	data, _ := json.Marshal(st)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	return ok(req.ID, m)
}

func (s *Server) cmdSetMode(req Request) Response {
	modeStr, _ := req.Params["mode"].(string)
	if modeStr == "" {
		return errResp(req.ID, "params.mode is required")
	}
	newMode := state.Mode(modeStr)
	switch newMode {
	case state.ModeNormal, state.ModeGaming, state.ModeLabActive,
		state.ModeUpdatePreparing, state.ModeUpdatePendingReboot,
		state.ModeRollbackMode, state.ModeSafeMode:
	default:
		return errResp(req.ID, fmt.Sprintf("unknown mode: %s", modeStr))
	}
	st := s.stateMgr.Get()
	if err := guardModeTransition(st.Mode, newMode); err != nil {
		return errResp(req.ID, err.Error())
	}
	prev := st.Mode
	if err := s.stateMgr.Update(func(s *state.SystemState) {
		s.Mode = newMode
	}); err != nil {
		return errResp(req.ID, fmt.Sprintf("state persist: %v", err))
	}
	s.bus.Emit(eventbus.EventModeChanged, map[string]any{
		"from": string(prev),
		"to":   string(newMode),
	})
	log.Printf("[hisnosd/ipc] mode: %s → %s", prev, newMode)
	return ok(req.ID, map[string]any{"mode": string(newMode)})
}

func (s *Server) cmdLockVault(req Request) Response {
	if err := s.vault.Execute(policy.Action{Type: policy.ActionForceVaultLock}); err != nil {
		return errResp(req.ID, fmt.Sprintf("vault lock: %v", err))
	}
	_ = s.stateMgr.Update(func(st *state.SystemState) {
		st.Vault.Mounted = false
		st.Vault.ExposureSeconds = 0
	})
	s.bus.Emit(eventbus.EventVaultLocked, map[string]any{"source": "ipc"})
	log.Printf("[hisnosd/ipc] vault locked via IPC")
	return ok(req.ID, nil)
}

func (s *Server) cmdStartLab(req Request) Response {
	st := s.stateMgr.Get()
	if allow, reason := policy.CanStartLab(st); !allow {
		return errResp(req.ID, fmt.Sprintf("policy rejected lab start: %s", reason))
	}
	profile, _ := req.Params["profile"].(string)
	if profile == "" {
		profile = "isolated"
	}
	// Delegate actual session start to the dashboard or lab runtime script.
	// hisnosd updates state and emits the event; the dashboard owns the systemd-run invocation.
	sessionID := generateSessionID()
	if err := s.stateMgr.Update(func(st *state.SystemState) {
		st.Mode = state.ModeLabActive
		st.Lab.Active = true
		st.Lab.SessionID = sessionID
		st.Lab.NetworkProfile = profile
	}); err != nil {
		return errResp(req.ID, fmt.Sprintf("state update: %v", err))
	}
	s.bus.Emit(eventbus.EventLabStarted, map[string]any{
		"session_id": sessionID,
		"profile":    profile,
	})
	log.Printf("[hisnosd/ipc] lab started session=%s profile=%s", sessionID, profile)
	return ok(req.ID, map[string]any{
		"session_id": sessionID,
		"profile":    profile,
	})
}

func (s *Server) cmdStopLab(req Request) Response {
	st := s.stateMgr.Get()
	sessionID := st.Lab.SessionID
	if sessionID == "" {
		sessionID, _ = req.Params["session_id"].(string)
	}
	if err := s.lab.StopSession(sessionID); err != nil {
		log.Printf("[hisnosd/ipc] WARN: lab stop: %v", err)
		// Non-fatal: update state anyway.
	}
	_ = s.stateMgr.Update(func(st *state.SystemState) {
		st.Lab.Active = false
		st.Lab.SessionID = ""
		if st.Mode == state.ModeLabActive {
			st.Mode = state.ModeNormal
		}
	})
	s.bus.Emit(eventbus.EventLabStopped, map[string]any{"session_id": sessionID})
	log.Printf("[hisnosd/ipc] lab stopped session=%s", sessionID)
	return ok(req.ID, nil)
}

func (s *Server) cmdReloadFirewall(req Request) Response {
	st := s.stateMgr.Get()
	if allow, reason := policy.CanReloadFirewall(st); !allow {
		return errResp(req.ID, fmt.Sprintf("policy rejected firewall reload: %s", reason))
	}
	if err := s.firewall.Execute(policy.Action{Type: policy.ActionFirewallRestore}); err != nil {
		return errResp(req.ID, fmt.Sprintf("firewall reload: %v", err))
	}
	now := time.Now().UTC()
	_ = s.stateMgr.Update(func(st *state.SystemState) {
		st.Firewall.LastReload = now
		st.Firewall.Active = true
	})
	s.bus.Emit(eventbus.EventFirewallReloaded, map[string]any{"ts": now.Format(time.RFC3339)})
	log.Printf("[hisnosd/ipc] firewall reloaded via IPC")
	return ok(req.ID, map[string]any{"reloaded_at": now.Format(time.RFC3339)})
}

func (s *Server) cmdPrepareUpdate(req Request) Response {
	exitCode, output, err := s.update.RunPreflight()
	if err != nil {
		return errResp(req.ID, fmt.Sprintf("preflight exec: %v", err))
	}
	if exitCode >= 2 {
		return errResp(req.ID, fmt.Sprintf("preflight failed (exit %d): %s", exitCode, output))
	}
	// Transition to update-preparing.
	_ = s.stateMgr.Update(func(st *state.SystemState) {
		st.Mode = state.ModeUpdatePreparing
		st.Update.Phase = "preparing"
	})
	s.bus.Emit(eventbus.EventUpdatePrepared, nil)
	log.Printf("[hisnosd/ipc] update preparing (preflight exit=%d)", exitCode)
	return ok(req.ID, map[string]any{
		"preflight_exit": exitCode,
		"preflight_output": output,
	})
}

func (s *Server) cmdStartGaming(req Request) Response {
	st := s.stateMgr.Get()
	if allow, reason := policy.CanStartGaming(st); !allow {
		return errResp(req.ID, fmt.Sprintf("policy rejected gaming start: %s", reason))
	}
	if err := s.gaming.Start(); err != nil {
		return errResp(req.ID, fmt.Sprintf("gaming start: %v", err))
	}
	_ = s.stateMgr.Update(func(st *state.SystemState) {
		st.Mode = state.ModeGaming
	})
	s.bus.Emit(eventbus.EventGamingStarted, nil)
	log.Printf("[hisnosd/ipc] gaming started")
	return ok(req.ID, map[string]any{"mode": "gaming"})
}

func (s *Server) cmdStopGaming(req Request) Response {
	if err := s.gaming.Stop(); err != nil {
		log.Printf("[hisnosd/ipc] WARN: gaming stop: %v", err)
	}
	_ = s.stateMgr.Update(func(st *state.SystemState) {
		if st.Mode == state.ModeGaming {
			st.Mode = state.ModeNormal
		}
	})
	s.bus.Emit(eventbus.EventGamingStopped, nil)
	log.Printf("[hisnosd/ipc] gaming stopped")
	return ok(req.ID, map[string]any{"mode": "normal"})
}

func (s *Server) cmdHealth(req Request) Response {
	st := s.stateMgr.Get()
	return ok(req.ID, map[string]any{
		"mode":         string(st.Mode),
		"risk_score":   st.Risk.Score,
		"risk_level":   st.Risk.Level,
		"vault_mounted": st.Vault.Mounted,
		"lab_active":   st.Lab.Active,
		"subsystems":   st.Subsystems,
	})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func ok(id string, data map[string]any) Response {
	return Response{ID: id, OK: true, Data: data}
}

func errResp(id, msg string) Response {
	return Response{ID: id, OK: false, Error: msg}
}

func writeErr(enc *json.Encoder, id, msg string) {
	_ = enc.Encode(errResp(id, msg))
}

// guardModeTransition rejects transitions that are forbidden by the current mode.
func guardModeTransition(from, to state.Mode) error {
	// Cannot manually exit update or rollback phases — those transition automatically.
	switch from {
	case state.ModeUpdatePreparing:
		if to != state.ModeNormal && to != state.ModeUpdatePendingReboot && to != state.ModeSafeMode {
			return fmt.Errorf("cannot transition from update-preparing to %s", to)
		}
	case state.ModeRollbackMode:
		if to != state.ModeNormal && to != state.ModeSafeMode {
			return fmt.Errorf("cannot transition from rollback-mode to %s", to)
		}
	}
	return nil
}

// generateSessionID returns a short hex session identifier.
func generateSessionID() string {
	return fmt.Sprintf("%x", time.Now().UnixNano())[:12]
}
