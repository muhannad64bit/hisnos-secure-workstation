// ipc/server.go — Unix socket JSON-RPC server for searchd
//
// Socket: /run/user/$UID/hisnos-search.sock (0600, owner-only)
// Protocol: line-delimited JSON (one request per line, one response per line)
// Commands: search, execute, preview, status
//
// Request:  {"id":1,"cmd":"search","query":"vault","limit":20}
// Response: {"id":1,"ok":true,"results":[...]}
//
// Idle connections are closed after 30 seconds of inactivity.

package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"hisnos.local/searchd/index"
)

const (
	idleTimeout = 30 * time.Second
)

// Server serves search queries over a Unix socket.
type Server struct {
	socketPath string
	idx        *index.Index
}

// NewServer constructs a Server.
func NewServer(socketPath string, idx *index.Index) *Server {
	return &Server{socketPath: socketPath, idx: idx}
}

// DefaultSocketPath returns /run/user/$UID/hisnos-search.sock.
func DefaultSocketPath() string {
	if s := os.Getenv("HISNOS_SEARCH_SOCKET"); s != "" {
		return s
	}
	uid := os.Getenv("UID")
	if uid == "" {
		uid = fmt.Sprintf("%d", os.Getuid())
	}
	return filepath.Join("/run/user", uid, "hisnos-search.sock")
}

// Run starts listening and serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	_ = os.Remove(s.socketPath)
	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.socketPath, err)
	}
	if err := os.Chmod(s.socketPath, 0600); err != nil {
		ln.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		go s.handleConn(conn)
	}
}

// request is a single IPC request line.
type request struct {
	ID      int64  `json:"id"`
	Cmd     string `json:"cmd"`
	Query   string `json:"query,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Action  string `json:"action,omitempty"`
	Preview string `json:"preview,omitempty"`
}

// response is a single IPC response line.
type response struct {
	ID      int64         `json:"id"`
	OK      bool          `json:"ok"`
	Error   string        `json:"error,omitempty"`
	Results []index.Result `json:"results,omitempty"`
	Data    any           `json:"data,omitempty"`
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	enc := json.NewEncoder(conn)

	for {
		conn.SetDeadline(time.Now().Add(idleTimeout))
		if !scanner.Scan() {
			return
		}
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}

		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = enc.Encode(response{OK: false, Error: "invalid JSON: " + err.Error()})
			continue
		}

		resp := s.dispatch(req)
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

func (s *Server) dispatch(req request) response {
	switch req.Cmd {
	case "search":
		return s.cmdSearch(req)
	case "execute":
		return s.cmdExecute(req)
	case "preview":
		return s.cmdPreview(req)
	case "status":
		return s.cmdStatus(req)
	default:
		return response{ID: req.ID, OK: false, Error: "unknown command: " + req.Cmd}
	}
}

func (s *Server) cmdSearch(req request) response {
	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	results := s.idx.Search(req.Query, limit)
	return response{ID: req.ID, OK: true, Results: results}
}

func (s *Server) cmdExecute(req request) response {
	action := req.Action
	if action == "" {
		return response{ID: req.ID, OK: false, Error: "action required"}
	}

	switch {
	case strings.HasPrefix(action, "open:"):
		path := strings.TrimPrefix(action, "open:")
		cmd := exec.Command("xdg-open", path)
		cmd.Start()
		return response{ID: req.ID, OK: true}

	case strings.HasPrefix(action, "browse:"):
		path := strings.TrimPrefix(action, "browse:")
		cmd := exec.Command("xdg-open", path)
		cmd.Start()
		return response{ID: req.ID, OK: true}

	case strings.HasPrefix(action, "ipc:"):
		// Delegate to hisnosd via its socket.
		ipcCmd := strings.TrimPrefix(action, "ipc:")
		result, err := delegateToHisnosd(ipcCmd)
		if err != nil {
			return response{ID: req.ID, OK: false, Error: err.Error()}
		}
		return response{ID: req.ID, OK: true, Data: result}

	case strings.HasPrefix(action, "shell:"):
		shellCmd := strings.TrimPrefix(action, "shell:")
		out, err := exec.Command("/bin/sh", "-c", shellCmd).CombinedOutput()
		if err != nil {
			return response{ID: req.ID, OK: false, Error: strings.TrimSpace(string(out))}
		}
		return response{ID: req.ID, OK: true, Data: strings.TrimSpace(string(out))}

	case strings.HasPrefix(action, "event:"):
		return response{ID: req.ID, OK: true, Data: map[string]string{"action": action}}

	default:
		return response{ID: req.ID, OK: false, Error: "unknown action prefix: " + action}
	}
}

func (s *Server) cmdPreview(req request) response {
	if req.Preview == "" {
		return response{ID: req.ID, OK: false, Error: "preview path required"}
	}
	snip := readFilePrev(req.Preview, 1000)
	return response{ID: req.ID, OK: true, Data: snip}
}

func (s *Server) cmdStatus(req request) response {
	return response{
		ID: req.ID,
		OK: true,
		Data: map[string]any{
			"files":     s.idx.FileCount(),
			"telemetry": s.idx.TelemetryCount(),
			"socket":    s.socketPath,
		},
	}
}

// delegateToHisnosd sends a single command to hisnosd IPC and returns the response data.
func delegateToHisnosd(command string) (any, error) {
	uid := fmt.Sprintf("%d", os.Getuid())
	sockPath := filepath.Join("/run/user", uid, "hisnosd.sock")
	if s := os.Getenv("HISNOS_IPC_SOCKET"); s != "" {
		sockPath = s
	}

	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("hisnosd unavailable: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	req := map[string]any{"id": 1, "command": command}
	enc := json.NewEncoder(conn)
	if err := enc.Encode(req); err != nil {
		return nil, err
	}

	var resp map[string]any
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&resp); err != nil {
		return nil, err
	}
	if ok, _ := resp["ok"].(bool); !ok {
		errMsg, _ := resp["error"].(string)
		return nil, fmt.Errorf("hisnosd: %s", errMsg)
	}
	return resp["data"], nil
}

// readFilePrev reads up to n bytes of a file for the preview pane.
func readFilePrev(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, n)
	nr, _ := f.Read(buf)
	return string(buf[:nr])
}
