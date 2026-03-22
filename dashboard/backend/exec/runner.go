// exec/runner.go — HisnOS safe subprocess execution wrapper
//
// Security contract:
//   - args[0] MUST be an absolute path (rejects PATH-based injection)
//   - Never uses shell interpolation (exec.Command, not "bash -c ...")
//   - Output is capped at MaxOutputBytes to prevent OOM from runaway scripts
//   - All subprocesses inherit the calling user's environment
//   - Non-zero exit codes are returned in Result.ExitCode, NOT as Go errors
//     (Go errors indicate I/O failure or timeout, not script logic failures)
//
// Two execution modes:
//   Run()    — buffered: collects all output, returns when command exits
//   Stream() — streaming: returns a channel of output lines for SSE forwarding

package exec

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	osExec "os/exec" // alias required: package name "exec" would shadow the stdlib import
	"path/filepath"
	"time"
)

const (
	DefaultTimeout = 30 * time.Second
	MaxOutputBytes = 1 << 20 // 1 MiB cap on buffered output
)

// Options configures a Run call.
type Options struct {
	Timeout time.Duration // 0 → DefaultTimeout
	Stdin   io.Reader     // optional stdin (e.g., passphrase for vault mount)
	Env     []string      // additional env vars in KEY=VALUE format
}

// Result holds captured output from a completed command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int // 0 on success; non-zero is NOT a Go error
}

// Run executes a command and returns its captured output.
//
// A Go error is returned only for I/O failures (bad path, pipe error) or timeout.
// A non-zero exit code is reflected in Result.ExitCode with no Go error.
func Run(args []string, opts Options) (*Result, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("exec: no command specified")
	}
	if !filepath.IsAbs(args[0]) {
		return nil, fmt.Errorf("exec: command must be an absolute path: %q", args[0])
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := osExec.CommandContext(ctx, args[0], args[1:]...)

	var stdout, stderr limitedBuffer
	stdout.limit = MaxOutputBytes
	stderr.limit = MaxOutputBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if opts.Stdin != nil {
		cmd.Stdin = opts.Stdin
	}
	if len(opts.Env) > 0 {
		cmd.Env = append(os.Environ(), opts.Env...)
	}

	runErr := cmd.Run()
	res := &Result{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if runErr != nil {
		if exitErr, ok := runErr.(*osExec.ExitError); ok {
			res.ExitCode = exitErr.ExitCode()
			return res, nil // non-zero exit is caller's concern, not a Go error
		}
		if ctx.Err() == context.DeadlineExceeded {
			return res, fmt.Errorf("exec: timed out after %s: %s", timeout, args[0])
		}
		return res, fmt.Errorf("exec: %w", runErr)
	}

	return res, nil
}

// Stream starts a command and returns a channel of output lines (stdout+stderr merged).
//
// The channel is closed when the command exits or the context is cancelled.
// The caller must drain the channel (or cancel the context) to avoid blocking.
//
// args[0] must be an absolute path.
func Stream(ctx context.Context, args []string) (<-chan string, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("exec: no command specified")
	}
	if !filepath.IsAbs(args[0]) {
		return nil, fmt.Errorf("exec: command must be an absolute path: %q", args[0])
	}

	cmd := osExec.CommandContext(ctx, args[0], args[1:]...)

	// io.Pipe merges stdout+stderr into a single stream the scanner can read.
	// When the process exits, cmd.Wait() returns and we close pw, which causes
	// the scanner to see io.EOF and exit cleanly.
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("exec: start %q: %w", args[0], err)
	}

	ch := make(chan string, 64)

	// Scanner goroutine: reads lines from pr and sends them on ch.
	go func() {
		defer close(ch)
		defer pr.Close()

		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			select {
			case ch <- scanner.Text():
			case <-ctx.Done():
				// Context cancelled: stop reading and let the wait goroutine
				// kill the process and close pw (which drains the scanner).
				return
			}
		}
	}()

	// Wait goroutine: waits for the process to exit, then closes pw.
	// Closing pw sends io.EOF to the scanner, which causes it to exit.
	// exec.CommandContext kills the process when ctx is cancelled, so
	// cmd.Wait() returns promptly on cancellation.
	go func() {
		cmd.Wait()
		pw.Close()
	}()

	return ch, nil
}

// StreamWithExitCode streams merged stdout+stderr lines like Stream, but also
// provides the final exit code once the process exits.
//
// The returned exitCode channel will always be closed after a single value
// is sent (unless ctx is cancelled before cmd.Start succeeds).
func StreamWithExitCode(ctx context.Context, args []string) (<-chan string, <-chan int, error) {
	if len(args) == 0 {
		return nil, nil, fmt.Errorf("exec: no command specified")
	}
	if !filepath.IsAbs(args[0]) {
		return nil, nil, fmt.Errorf("exec: command must be an absolute path: %q", args[0])
	}

	cmd := osExec.CommandContext(ctx, args[0], args[1:]...)

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, nil, fmt.Errorf("exec: start %q: %w", args[0], err)
	}

	ch := make(chan string, 64)
	exitCh := make(chan int, 1)

	// Scanner goroutine.
	go func() {
		defer close(ch)
		defer pr.Close()
		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			select {
			case ch <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
	}()

	// Wait goroutine with exit code.
	go func() {
		err := cmd.Wait()
		// Ensure scanner sees EOF.
		pw.Close()

		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*osExec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				// I/O error or ctx cancellation before process exit: use 1 as unknown.
				exitCode = 1
			}
		}

		exitCh <- exitCode
		close(exitCh)
	}()

	return ch, exitCh, nil
}

// ── limitedBuffer ─────────────────────────────────────────────────────────────

// limitedBuffer is a bytes.Buffer that silently discards writes beyond limit.
type limitedBuffer struct {
	bytes.Buffer
	limit int
	total int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.total >= b.limit {
		return len(p), nil // silently discard; report success to avoid breaking cmd
	}
	remaining := b.limit - b.total
	if len(p) > remaining {
		p = p[:remaining]
	}
	n, err := b.Buffer.Write(p)
	b.total += n
	return n, err
}
