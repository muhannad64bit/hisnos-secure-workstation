// core/runtime/leader_guard.go
//
// LeaderGuard ensures exactly one hisnosd instance is active at any time.
//
// Mechanism:
//   - File lock: LOCK_EX|LOCK_NB on /run/user/<uid>/hisnosd.lock.
//     The kernel releases this lock on process death, even on SIGKILL.
//   - PID file: after acquiring the lock, writes own PID to the lock file.
//   - Socket validation: checks whether the IPC socket exists and is owned
//     by a live process.  If the socket exists but its owning PID is dead,
//     removes the stale socket.
//   - Re-election: on startup, if a stale lock is found (PID dead), removes
//     the lock file and re-tries acquisition once.
//
// The lock fd must be kept open for the lifetime of the process.
// Call Release() at shutdown to cleanly remove the PID record (not strictly
// required — kernel will release the flock on fd close).

package runtime

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	lockDirMode = 0700
)

// LeaderGuard holds the exclusive file lock for the hisnosd process.
type LeaderGuard struct {
	lockPath   string
	socketPath string
	fd         int // file descriptor holding the flock (must stay open)
}

// AcquireLeadership tries to become the single active hisnosd instance.
// Returns error if another live instance holds the lock.
func AcquireLeadership(uid int, socketPath string) (*LeaderGuard, error) {
	lockDir := fmt.Sprintf("/run/user/%d", uid)
	if err := os.MkdirAll(lockDir, lockDirMode); err != nil {
		// /run/user/<uid> should be created by systemd-logind; warn but proceed.
		log.Printf("[leader] WARN: cannot ensure lock dir %s: %v", lockDir, err)
	}

	lockPath := filepath.Join(lockDir, "hisnosd.lock")

	g := &LeaderGuard{
		lockPath:   lockPath,
		socketPath: socketPath,
		fd:         -1,
	}

	if err := g.acquire(); err != nil {
		return nil, err
	}

	// Validate / clean up the IPC socket.
	if err := g.validateSocket(); err != nil {
		log.Printf("[leader] WARN: socket validation: %v", err)
	}

	return g, nil
}

func (g *LeaderGuard) acquire() error {
	// O_CREAT|O_RDWR: create if missing, open for read+write.
	fd, err := syscall.Open(g.lockPath,
		syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return fmt.Errorf("open lock file %s: %w", g.lockPath, err)
	}

	// Non-blocking exclusive lock.
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		syscall.Close(fd)

		if err == syscall.EWOULDBLOCK {
			// Lock held by another process.  Read its PID for diagnostics.
			pid := readPIDFromLockFile(g.lockPath)
			if pid > 0 && processAlive(pid) {
				return fmt.Errorf("another hisnosd instance is running (PID %d)", pid)
			}

			// Stale lock: previous instance died without releasing.
			log.Printf("[leader] stale lock detected (PID %d dead) — clearing and retrying", pid)
			if rerr := syscall.Unlink(g.lockPath); rerr != nil && rerr != syscall.ENOENT {
				return fmt.Errorf("remove stale lock: %w", rerr)
			}

			// Retry once.
			fd2, err2 := syscall.Open(g.lockPath,
				syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC, 0600)
			if err2 != nil {
				return fmt.Errorf("open lock file (retry): %w", err2)
			}
			if err2 = syscall.Flock(fd2, syscall.LOCK_EX|syscall.LOCK_NB); err2 != nil {
				syscall.Close(fd2)
				return fmt.Errorf("flock (retry): %w", err2)
			}
			fd = fd2
		} else {
			return fmt.Errorf("flock: %w", err)
		}
	}

	// Write own PID to the lock file.
	pid := os.Getpid()
	pidStr := strconv.Itoa(pid)
	if terr := syscall.Ftruncate(fd, 0); terr == nil {
		syscall.Write(fd, []byte(pidStr+"\n"))
	}

	g.fd = fd
	log.Printf("[leader] leadership acquired (PID %d, lock %s)", pid, g.lockPath)
	return nil
}

// Release releases the file lock and removes the PID record.
// It is safe to call Release more than once.
func (g *LeaderGuard) Release() {
	if g.fd >= 0 {
		syscall.Ftruncate(g.fd, 0) // clear PID
		syscall.Flock(g.fd, syscall.LOCK_UN)
		syscall.Close(g.fd)
		g.fd = -1
	}
}

// validateSocket ensures the IPC socket is not a stale artefact from a dead process.
func (g *LeaderGuard) validateSocket() error {
	if g.socketPath == "" {
		return nil
	}

	info, err := os.Lstat(g.socketPath)
	if os.IsNotExist(err) {
		return nil // socket does not exist yet — expected on first start
	}
	if err != nil {
		return err
	}

	// Check it is a socket.
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("path %s exists but is not a socket (mode %s)",
			g.socketPath, info.Mode())
	}

	// The socket is present.  Since we now hold the lock, any previous instance
	// is dead.  Remove the stale socket so net.Listen can bind it fresh.
	log.Printf("[leader] removing stale IPC socket: %s", g.socketPath)
	return os.Remove(g.socketPath)
}

// ── helpers ───────────────────────────────────────────────────────────────

func readPIDFromLockFile(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return -1
	}
	return pid
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// Signal 0: checks existence + permission without delivering a signal.
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
