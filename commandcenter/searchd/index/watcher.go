// index/watcher.go — inotify-based incremental index updater
//
// Uses Linux syscall.InotifyInit1 + InotifyAddWatch.
// Watches all indexed directories for: IN_CREATE, IN_DELETE, IN_MOVED_FROM,
// IN_MOVED_TO, IN_CLOSE_WRITE, IN_ATTRIB.
// Polls with 50ms sleep on EAGAIN; non-blocking FD (IN_NONBLOCK).
// Symlinks are never followed.

package index

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	inotifyMask = syscall.IN_CREATE |
		syscall.IN_DELETE |
		syscall.IN_MOVED_FROM |
		syscall.IN_MOVED_TO |
		syscall.IN_CLOSE_WRITE |
		syscall.IN_ATTRIB |
		syscall.IN_DONT_FOLLOW
)

// Watcher monitors directories and updates the index on filesystem changes.
type Watcher struct {
	idx     *Index
	cfg     ScanConfig
	fd      int
	wdToDir map[int32]string // watch descriptor → directory path
}

// NewWatcher creates a Watcher. Call Run(ctx) in a goroutine.
func NewWatcher(idx *Index, cfg ScanConfig) (*Watcher, error) {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}
	w := &Watcher{
		idx:     idx,
		cfg:     cfg,
		fd:      fd,
		wdToDir: make(map[int32]string),
	}
	return w, nil
}

// WatchDir adds an inotify watch for a single directory.
func (w *Watcher) WatchDir(dir string) error {
	wd, err := syscall.InotifyAddWatch(w.fd, dir, inotifyMask)
	if err != nil {
		return err
	}
	w.wdToDir[int32(wd)] = dir
	return nil
}

// Run polls the inotify FD and processes events until done is closed.
func (w *Watcher) Run(done <-chan struct{}) {
	defer syscall.Close(w.fd)

	buf := make([]byte, 4096)
	for {
		select {
		case <-done:
			return
		default:
		}

		n, err := syscall.Read(w.fd, buf)
		if err != nil {
			if err == syscall.EAGAIN {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			// Unrecoverable read error — log and exit loop.
			return
		}

		offset := 0
		for offset+syscall.SizeofInotifyEvent <= n {
			ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[offset]))
			nameLen := int(ev.Len)
			nameBytes := buf[offset+syscall.SizeofInotifyEvent : offset+syscall.SizeofInotifyEvent+nameLen]
			name := nullTerminated(nameBytes)
			w.handleEvent(ev, name)
			offset += syscall.SizeofInotifyEvent + nameLen
		}
	}
}

func (w *Watcher) handleEvent(ev *syscall.InotifyEvent, name string) {
	dir, ok := w.wdToDir[int32(ev.Wd)]
	if !ok {
		return
	}
	if name == "" {
		return
	}

	fullPath := filepath.Join(dir, name)
	isDir := ev.Mask&syscall.IN_ISDIR != 0

	switch {
	case ev.Mask&syscall.IN_DELETE != 0, ev.Mask&syscall.IN_MOVED_FROM != 0:
		w.idx.RemoveFile(fullPath)

	case ev.Mask&syscall.IN_CREATE != 0, ev.Mask&syscall.IN_MOVED_TO != 0, ev.Mask&syscall.IN_CLOSE_WRITE != 0, ev.Mask&syscall.IN_ATTRIB != 0:
		if isDir {
			// Watch the new directory and quick-scan it.
			if !w.isExcluded(name) {
				_ = w.WatchDir(fullPath)
				w.idx.AddFile(fullPath, 0, time.Now(), true, "")
			}
			return
		}
		info, err := os.Lstat(fullPath)
		if err != nil {
			return
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return
		}
		snip := ""
		if isTextExt(fullPath) && info.Size() <= 1<<20 {
			snip = readSnip(fullPath, 200)
		}
		w.idx.AddFile(fullPath, info.Size(), info.ModTime(), false, snip)
	}
}

func (w *Watcher) isExcluded(name string) bool {
	for _, ex := range w.cfg.ExcludeDirs {
		if strings.EqualFold(name, filepath.Base(ex)) {
			return true
		}
	}
	return false
}

// nullTerminated returns the string up to the first NUL byte.
func nullTerminated(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// isTextExt returns true for extensions the watcher should snip.
func isTextExt(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	textExts := map[string]bool{
		".go": true, ".sh": true, ".py": true, ".toml": true,
		".yaml": true, ".yml": true, ".json": true, ".conf": true,
		".md": true, ".txt": true, ".service": true, ".nft": true,
	}
	return textExts[ext]
}

// Ensure binary import used (needed for potential future big-endian struct reads).
var _ = binary.LittleEndian
