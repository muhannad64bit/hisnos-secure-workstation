// index/index.go — In-memory inverted index for HisnOS Command Center searchd
//
// Architecture:
//   - All records kept in memory; JSON snapshot persisted to disk on shutdown
//   - Inverted index: token → []uint64 (packed fileID or telemetryID + type bit)
//   - FileRecords indexed by path; TelemetryRecords indexed by sequential ID
//   - CommandRecords loaded from static commands.json at startup
//   - Score: exactNameMatch×100 + prefixName×50 + containsName×20 + pathContains×10
//             + tokenOverlap×5, multiplied by recencyWeight = 1/(1+daysSince/30)
//   - Max 10,000 telemetry entries; ring-wraps on overflow
//
// ID encoding:
//   - Bit 63 = 0 → fileID, bit 63 = 1 → telemetry, bit 62 = 1 → command

package index

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	maxTelemetryEntries = 10_000
	telemetryBit        = uint64(1) << 63
	commandBit          = uint64(1) << 62
)

// ResultType categorises a search result for the UI grouping headers.
type ResultType string

const (
	ResultTypeFile    ResultType = "FILE"
	ResultTypeCommand ResultType = "COMMAND"
	ResultTypeEvent   ResultType = "SECURITY_EVENT"
	ResultTypeApp     ResultType = "APP"
)

// FileRecord represents an indexed filesystem entry.
type FileRecord struct {
	ID          uint64    `json:"id"`
	Path        string    `json:"path"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"mod_time"`
	IsDir       bool      `json:"is_dir"`
	ContentSnip string    `json:"snip,omitempty"` // first 200 bytes if text
}

// TelemetryRecord represents one audit or threat event.
type TelemetryRecord struct {
	ID        uint64    `json:"id"`
	Source    string    `json:"source"`    // "audit" | "threat"
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`   // "info" | "warn" | "critical"
	Message   string    `json:"message"`
	Fields    map[string]string `json:"fields,omitempty"`
}

// CommandRecord represents a static HisnOS command from commands.json.
type CommandRecord struct {
	ID          uint64   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Keywords    []string `json:"keywords"`
	Action      string   `json:"action"` // "ipc:lock_vault" | "shell:..." | "url:..."
	Icon        string   `json:"icon,omitempty"`
}

// Result is a single search result returned to the UI.
type Result struct {
	Type        ResultType `json:"type"`
	Score       float64    `json:"score"`
	Title       string     `json:"title"`
	Subtitle    string     `json:"subtitle"`
	Action      string     `json:"action"`
	Preview     string     `json:"preview,omitempty"`
	Timestamp   string     `json:"timestamp,omitempty"`
	RiskLevel   string     `json:"risk_level,omitempty"`
}

// Index is the central search index. All exported methods are goroutine-safe.
type Index struct {
	mu sync.RWMutex

	// Primary stores
	files     map[uint64]*FileRecord
	telemetry []*TelemetryRecord // ring buffer, length <= maxTelemetryEntries
	commands  []*CommandRecord

	// Inverted index: normalised token → encoded IDs
	inv map[string][]uint64

	// Counters
	nextFileID uint64
	nextTelID  uint64
	telHead    int // next write position in ring buffer
	telCount   int
}

// New creates an empty Index.
func New() *Index {
	return &Index{
		files:    make(map[uint64]*FileRecord),
		telemetry: make([]*TelemetryRecord, maxTelemetryEntries),
		inv:      make(map[string][]uint64),
	}
}

// LoadCommands replaces the command registry from parsed JSON.
func (idx *Index) LoadCommands(cmds []*CommandRecord) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.commands = cmds
	for i, c := range cmds {
		c.ID = commandBit | uint64(i)
		for _, tok := range tokenise(c.Name + " " + c.Description + " " + strings.Join(c.Keywords, " ")) {
			idx.inv[tok] = appendUnique(idx.inv[tok], c.ID)
		}
	}
}

// AddFile inserts or updates a file record. Returns the assigned ID.
func (idx *Index) AddFile(path string, size int64, modTime time.Time, isDir bool, snip string) uint64 {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	id := idx.nextFileID
	idx.nextFileID++

	rec := &FileRecord{
		ID:          id,
		Path:        path,
		Name:        filepath.Base(path),
		Size:        size,
		ModTime:     modTime,
		IsDir:       isDir,
		ContentSnip: snip,
	}
	idx.files[id] = rec

	for _, tok := range tokenise(rec.Name + " " + path) {
		idx.inv[tok] = appendUnique(idx.inv[tok], id)
	}
	return id
}

// RemoveFile removes a file record by path. O(n) scan; called only on delete events.
func (idx *Index) RemoveFile(path string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	for id, rec := range idx.files {
		if rec.Path == path {
			delete(idx.files, id)
			// Lazy removal: entries in inv will simply miss on lookup — acceptable for search.
			return
		}
	}
}

// AddTelemetry appends a telemetry record to the ring buffer.
func (idx *Index) AddTelemetry(source, level, message string, ts time.Time, fields map[string]string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	id := telemetryBit | idx.nextTelID
	idx.nextTelID++

	rec := &TelemetryRecord{
		ID:        id,
		Source:    source,
		Timestamp: ts,
		Level:     level,
		Message:   message,
		Fields:    fields,
	}

	idx.telemetry[idx.telHead] = rec
	idx.telHead = (idx.telHead + 1) % maxTelemetryEntries
	if idx.telCount < maxTelemetryEntries {
		idx.telCount++
	}

	for _, tok := range tokenise(message + " " + level + " " + source) {
		idx.inv[tok] = appendUnique(idx.inv[tok], id)
	}
	for _, v := range fields {
		for _, tok := range tokenise(v) {
			idx.inv[tok] = appendUnique(idx.inv[tok], id)
		}
	}
}

// Search returns up to limit results ranked by score.
func (idx *Index) Search(query string, limit int) []Result {
	if limit <= 0 {
		limit = 20
	}
	if strings.TrimSpace(query) == "" {
		return nil
	}

	idx.mu.RLock()
	defer idx.mu.RUnlock()

	queryLower := strings.ToLower(strings.TrimSpace(query))
	tokens := tokenise(query)
	now := time.Now()

	// Collect candidate IDs from inverted index.
	seen := make(map[uint64]float64) // id → raw score
	for _, tok := range tokens {
		for _, id := range idx.inv[tok] {
			seen[id] += 5
		}
	}

	// Score each candidate.
	scored := make([]Result, 0, len(seen))
	for id, base := range seen {
		var res Result
		var ok bool

		switch {
		case id&commandBit != 0:
			res, ok = idx.scoreCommand(id, queryLower, base)
		case id&telemetryBit != 0:
			res, ok = idx.scoreTelemetry(id, queryLower, tokens, base, now)
		default:
			res, ok = idx.scoreFile(id, queryLower, base, now)
		}
		if ok {
			scored = append(scored, res)
		}
	}

	// Also do direct substring scan for names not in inverted index yet.
	for _, rec := range idx.files {
		if _, alreadyScored := seen[rec.ID]; alreadyScored {
			continue
		}
		nameLower := strings.ToLower(rec.Name)
		if strings.Contains(nameLower, queryLower) || strings.Contains(strings.ToLower(rec.Path), queryLower) {
			res, ok := idx.scoreFile(rec.ID, queryLower, 0, now)
			if ok {
				scored = append(scored, res)
			}
		}
	}
	for _, cmd := range idx.commands {
		if _, alreadyScored := seen[cmd.ID]; alreadyScored {
			continue
		}
		if strings.Contains(strings.ToLower(cmd.Name), queryLower) ||
			strings.Contains(strings.ToLower(cmd.Description), queryLower) {
			res, ok := idx.scoreCommand(cmd.ID, queryLower, 0)
			if ok {
				scored = append(scored, res)
			}
		}
	}

	// Sort descending by score (simple insertion sort for small N).
	for i := 1; i < len(scored); i++ {
		for j := i; j > 0 && scored[j].Score > scored[j-1].Score; j-- {
			scored[j], scored[j-1] = scored[j-1], scored[j]
		}
	}
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored
}

func (idx *Index) scoreFile(id uint64, queryLower string, base float64, now time.Time) (Result, bool) {
	rec, ok := idx.files[id]
	if !ok {
		return Result{}, false
	}
	nameLower := strings.ToLower(rec.Name)
	pathLower := strings.ToLower(rec.Path)

	score := base
	if nameLower == queryLower {
		score += 100
	} else if strings.HasPrefix(nameLower, queryLower) {
		score += 50
	} else if strings.Contains(nameLower, queryLower) {
		score += 20
	}
	if strings.Contains(pathLower, queryLower) {
		score += 10
	}

	score *= recencyWeight(rec.ModTime, now)

	rtype := ResultTypeFile
	subtitle := rec.Path
	action := "open:" + rec.Path
	if rec.IsDir {
		action = "browse:" + rec.Path
	}

	return Result{
		Type:     rtype,
		Score:    score,
		Title:    rec.Name,
		Subtitle: subtitle,
		Action:   action,
		Preview:  rec.ContentSnip,
	}, true
}

func (idx *Index) scoreTelemetry(id uint64, queryLower string, tokens []string, base float64, now time.Time) (Result, bool) {
	// Find in ring buffer by ID.
	var rec *TelemetryRecord
	for i := 0; i < idx.telCount; i++ {
		r := idx.telemetry[i]
		if r != nil && r.ID == id {
			rec = r
			break
		}
	}
	if rec == nil {
		return Result{}, false
	}

	msgLower := strings.ToLower(rec.Message)
	score := base
	if strings.Contains(msgLower, queryLower) {
		score += 20
	}
	score *= recencyWeight(rec.Timestamp, now)

	riskLevel := ""
	if rec.Level == "critical" {
		score += 30
		riskLevel = "critical"
	} else if rec.Level == "warn" {
		score += 10
		riskLevel = "warn"
	}

	return Result{
		Type:      ResultTypeEvent,
		Score:     score,
		Title:     rec.Message,
		Subtitle:  rec.Source + " · " + rec.Level,
		Action:    "event:" + rec.Source,
		Timestamp: rec.Timestamp.Format(time.RFC3339),
		RiskLevel: riskLevel,
	}, true
}

func (idx *Index) scoreCommand(id uint64, queryLower string, base float64) (Result, bool) {
	cmdIdx := int(id &^ commandBit)
	if cmdIdx >= len(idx.commands) {
		return Result{}, false
	}
	cmd := idx.commands[cmdIdx]

	nameLower := strings.ToLower(cmd.Name)
	score := base
	if nameLower == queryLower {
		score += 100
	} else if strings.HasPrefix(nameLower, queryLower) {
		score += 50
	} else if strings.Contains(nameLower, queryLower) {
		score += 20
	}
	if strings.Contains(strings.ToLower(cmd.Description), queryLower) {
		score += 10
	}

	return Result{
		Type:     ResultTypeCommand,
		Score:    score,
		Title:    cmd.Name,
		Subtitle: cmd.Description,
		Action:   cmd.Action,
	}, true
}

// FileCount returns the number of indexed files.
func (idx *Index) FileCount() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.files)
}

// TelemetryCount returns the number of indexed telemetry events.
func (idx *Index) TelemetryCount() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.telCount
}

// SaveSnapshot persists the file index to a JSON file.
func (idx *Index) SaveSnapshot(path string) error {
	idx.mu.RLock()
	files := make([]*FileRecord, 0, len(idx.files))
	for _, r := range idx.files {
		files = append(files, r)
	}
	idx.mu.RUnlock()

	data, err := json.Marshal(files)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadSnapshot restores a file index from a JSON snapshot (best-effort at startup).
func (idx *Index) LoadSnapshot(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var files []*FileRecord
	if err := json.Unmarshal(data, &files); err != nil {
		return err
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, rec := range files {
		if rec.ID >= idx.nextFileID {
			idx.nextFileID = rec.ID + 1
		}
		idx.files[rec.ID] = rec
		for _, tok := range tokenise(rec.Name + " " + rec.Path) {
			idx.inv[tok] = appendUnique(idx.inv[tok], rec.ID)
		}
	}
	return nil
}

// tokenise splits text into normalised lowercase tokens, stripping punctuation.
func tokenise(text string) []string {
	text = strings.ToLower(text)
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	// Deduplicate within the token set.
	seen := make(map[string]struct{}, len(fields))
	out := fields[:0]
	for _, f := range fields {
		if len(f) < 2 {
			continue
		}
		if _, dup := seen[f]; !dup {
			seen[f] = struct{}{}
			out = append(out, f)
		}
	}
	return out
}

// recencyWeight returns 1/(1 + daysSince/30), clamped to [0.1, 1.0].
func recencyWeight(t, now time.Time) float64 {
	days := now.Sub(t).Hours() / 24
	if days < 0 {
		days = 0
	}
	w := 1.0 / (1.0 + days/30.0)
	return math.Max(0.1, w)
}

// appendUnique appends v to s only if not already present (linear scan; lists are small).
func appendUnique(s []uint64, v uint64) []uint64 {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}
