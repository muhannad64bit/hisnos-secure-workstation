// index/scanner.go — Directory walker for initial index population
//
// Walks configured root directories, skips excluded paths, reads text file snippets.
// Called once at startup; incremental updates come from watcher.go.

package index

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ScanConfig controls what the scanner traverses.
type ScanConfig struct {
	Roots        []string // directories to index
	ExcludeDirs  []string // directory names to skip entirely
	MaxFileSize  int64    // files larger than this get no content snip (0 = no snip)
	MaxDepth     int      // 0 = unlimited
	TextExtTypes []string // extensions to attempt content snip (e.g. ".go", ".sh")
}

// DefaultScanConfig returns a sane default for a Fedora Kinoite user home.
func DefaultScanConfig(home string) ScanConfig {
	return ScanConfig{
		Roots: []string{
			home,
			filepath.Join(home, ".local", "share", "hisnos"),
			"/etc/hisnos",
		},
		ExcludeDirs: []string{
			".git", ".cache", ".mozilla", ".config/google-chrome", ".config/chromium",
			"node_modules", "__pycache__", ".venv", "vendor",
			"proc", "sys", "dev", "run",
			".local/share/Steam", ".steam", "Proton",
		},
		MaxFileSize:  1 << 20, // 1 MiB
		MaxDepth:     8,
		TextExtTypes: []string{".go", ".sh", ".py", ".toml", ".yaml", ".yml", ".json", ".conf", ".md", ".txt", ".service", ".nft"},
	}
}

// Scanner walks the filesystem and populates an Index.
type Scanner struct {
	cfg ScanConfig
	idx *Index
}

// NewScanner constructs a Scanner.
func NewScanner(cfg ScanConfig, idx *Index) *Scanner {
	return &Scanner{cfg: cfg, idx: idx}
}

// Run performs a full scan of all configured roots.
func (sc *Scanner) Run() {
	for _, root := range sc.cfg.Roots {
		if _, err := os.Lstat(root); err != nil {
			continue // skip missing roots silently
		}
		sc.walk(root, 0)
	}
}

func (sc *Scanner) walk(dir string, depth int) {
	if sc.cfg.MaxDepth > 0 && depth > sc.cfg.MaxDepth {
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		name := entry.Name()
		fullPath := filepath.Join(dir, name)

		if entry.IsDir() {
			if sc.excludeDir(name) {
				continue
			}
			sc.idx.AddFile(fullPath, 0, entryModTime(entry), true, "")
			sc.walk(fullPath, depth+1)
			continue
		}

		if entry.Type()&fs.ModeSymlink != 0 {
			continue // skip symlinks to avoid cycles
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}
		size := info.Size()
		snip := ""
		if sc.cfg.MaxFileSize > 0 && size <= sc.cfg.MaxFileSize && sc.isTextExt(name) {
			snip = readSnip(fullPath, 200)
		}
		sc.idx.AddFile(fullPath, size, info.ModTime(), false, snip)
	}
}

func (sc *Scanner) excludeDir(name string) bool {
	for _, ex := range sc.cfg.ExcludeDirs {
		if strings.EqualFold(name, ex) {
			return true
		}
		// Also handle path-based exclusions like ".local/share/Steam"
		if strings.HasSuffix(ex, "/"+name) || ex == name {
			return true
		}
	}
	return false
}

func (sc *Scanner) isTextExt(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	for _, allowed := range sc.cfg.TextExtTypes {
		if ext == allowed {
			return true
		}
	}
	return false
}

// readSnip reads up to n bytes from a file and returns printable ASCII only.
func readSnip(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	buf := make([]byte, n)
	nr, err := io.ReadFull(f, buf)
	if err != nil && nr == 0 {
		return ""
	}
	buf = buf[:nr]

	// Strip non-printable bytes.
	out := make([]byte, 0, nr)
	for _, b := range buf {
		if b == '\n' || b == '\t' || (b >= 0x20 && b < 0x7F) {
			out = append(out, b)
		}
	}
	result := strings.TrimSpace(string(out))
	if len(result) > n {
		result = result[:n]
	}
	return result
}

func entryModTime(entry fs.DirEntry) (t interface{ IsZero() bool }) {
	// Return a zero time on error — callers use time.Time{}
	info, err := entry.Info()
	if err != nil {
		return zeroTime{}
	}
	return info.ModTime()
}

type zeroTime struct{}

func (z zeroTime) IsZero() bool { return true }
