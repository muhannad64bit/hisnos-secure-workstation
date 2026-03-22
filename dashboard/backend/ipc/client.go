// dashboard/backend/ipc/client.go — hisnosd IPC client for the dashboard.
//
// The Client connects to the hisnosd Unix socket and sends JSON-RPC commands.
// Protocol: line-delimited JSON (one request per line, one response per line).
//
// Usage pattern in dashboard handlers:
//
//   if h.hisnosd != nil {
//       result, err := h.hisnosd.Execute("lock_vault", nil)
//       if err != nil { writeError(w, 503, err.Error()); return }
//       writeJSON(w, 200, result)
//       return
//   }
//   // fallback: direct exec ...
//
// Connection is established on first use (lazy connect). On any failure the
// client attempts reconnection on the next call. The dashboard continues to
// work via direct exec fallback if hisnosd is not available.

package ipc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	connectTimeout = 2 * time.Second
	callTimeout    = 10 * time.Second
)

// Request mirrors the hisnosd wire format.
type Request struct {
	ID      string         `json:"id"`
	Command string         `json:"command"`
	Params  map[string]any `json:"params,omitempty"`
}

// Response mirrors the hisnosd wire format.
type Response struct {
	ID    string         `json:"id"`
	OK    bool           `json:"ok"`
	Data  map[string]any `json:"data,omitempty"`
	Error string         `json:"error,omitempty"`
}

// Client is a lightweight, goroutine-safe hisnosd IPC client.
type Client struct {
	socketPath string

	mu   sync.Mutex
	conn net.Conn
	enc  *json.Encoder
	dec  *bufio.Scanner

	seq atomic.Int64
}

// DefaultSocketPath returns the expected hisnosd socket path for the current user.
func DefaultSocketPath() string {
	uid := strconv.Itoa(os.Getuid())
	if p := os.Getenv("HISNOS_IPC_SOCKET"); p != "" {
		return p
	}
	return fmt.Sprintf("/run/user/%s/hisnosd.sock", uid)
}

// NewClient creates a Client. The socket is not opened until the first call.
func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath}
}

// Available returns true if the hisnosd socket exists.
// This is a fast non-blocking check; it does not open a connection.
func (c *Client) Available() bool {
	_, err := os.Stat(c.socketPath)
	return err == nil
}

// Execute sends command with optional params and returns the response data.
// Returns (nil, err) on transport or server error.
func (c *Client) Execute(command string, params map[string]any) (map[string]any, error) {
	id := strconv.FormatInt(c.seq.Add(1), 10)
	req := Request{
		ID:      id,
		Command: command,
		Params:  params,
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.ensureConnected(); err != nil {
		return nil, fmt.Errorf("hisnosd unavailable: %w", err)
	}

	// Set deadline on the underlying connection.
	_ = c.conn.SetDeadline(time.Now().Add(callTimeout))

	if err := c.enc.Encode(req); err != nil {
		c.closeConn()
		return nil, fmt.Errorf("send: %w", err)
	}

	if !c.dec.Scan() {
		err := c.dec.Err()
		c.closeConn()
		if err == nil {
			return nil, fmt.Errorf("connection closed by hisnosd")
		}
		return nil, fmt.Errorf("recv: %w", err)
	}

	var resp Response
	if err := json.Unmarshal(c.dec.Bytes(), &resp); err != nil {
		c.closeConn()
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if !resp.OK {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	return resp.Data, nil
}

// GetState is a convenience wrapper for the get_state command.
func (c *Client) GetState() (map[string]any, error) {
	return c.Execute("get_state", nil)
}

// SetMode transitions hisnosd to the given mode.
func (c *Client) SetMode(mode string) error {
	_, err := c.Execute("set_mode", map[string]any{"mode": mode})
	return err
}

// LockVault sends the lock_vault command.
func (c *Client) LockVault() error {
	_, err := c.Execute("lock_vault", nil)
	return err
}

// StartLab sends the start_lab command with the given profile.
func (c *Client) StartLab(profile string) (map[string]any, error) {
	return c.Execute("start_lab", map[string]any{"profile": profile})
}

// StopLab sends the stop_lab command.
func (c *Client) StopLab(sessionID string) error {
	params := map[string]any{}
	if sessionID != "" {
		params["session_id"] = sessionID
	}
	_, err := c.Execute("stop_lab", params)
	return err
}

// ReloadFirewall sends the reload_firewall command.
func (c *Client) ReloadFirewall() (map[string]any, error) {
	return c.Execute("reload_firewall", nil)
}

// StartGaming sends the start_gaming command.
func (c *Client) StartGaming() error {
	_, err := c.Execute("start_gaming", nil)
	return err
}

// StopGaming sends the stop_gaming command.
func (c *Client) StopGaming() error {
	_, err := c.Execute("stop_gaming", nil)
	return err
}

// Health sends the health command.
func (c *Client) Health() (map[string]any, error) {
	return c.Execute("health", nil)
}

// Close closes the underlying connection.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeConn()
}

// ── Internal ──────────────────────────────────────────────────────────────────

func (c *Client) ensureConnected() error {
	if c.conn != nil {
		return nil
	}
	conn, err := net.DialTimeout("unix", c.socketPath, connectTimeout)
	if err != nil {
		return err
	}
	c.conn = conn
	c.enc = json.NewEncoder(conn)
	c.dec = bufio.NewScanner(conn)
	c.dec.Buffer(make([]byte, 64*1024), 64*1024)
	return nil
}

func (c *Client) closeConn() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
		c.enc = nil
		c.dec = nil
	}
}
