package spool

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Record is the envelope stored on disk for a deferred ClickHouse write.
type Record struct {
	Kind string          `json:"kind"` // subscription|payment|transaction|acu|chargeback
	Data json.RawMessage `json:"data"`
}

type Spool struct {
	dir        string
	maxBytes   int64
	maxFiles   int
	lowWMScale float64 // trim down to maxBytes*lowWMScale when over cap (e.g., 0.9)
}

const (
	defaultSpoolDir = "/var/lib/openrails/spool"
)

func defaultDir() string {
	return defaultSpoolDir
}

// New creates a spool directory with safe defaults and built-in caps.
// Defaults: 1 GiB total size, unlimited files, 90% low watermark trim target.
func New(dir string) (*Spool, error) {
	if dir == "" {
		dir = defaultDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create spool dir: %w", err)
	}
	// Safe defaults: 1 GiB, unlimited files, trim to 90%
	return &Spool{dir: dir, maxBytes: 1 << 30, maxFiles: 0, lowWMScale: 0.9}, nil
}

func (s *Spool) Dir() string { return s.dir }

// Enqueue writes a record as a JSON file with a timestamped filename.
func (s *Spool) Enqueue(rec *Record) error {
	ts := time.Now().UTC().Format("20060102T150405.000000000Z07:00")
	name := fmt.Sprintf("%s_%d.json", ts, time.Now().UTC().UnixNano())
	path := filepath.Join(s.dir, name)
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	// Enforce capacity before creating a temp file so temp files are not counted
	// as committed records or accidentally deleted as old records.
	if err := s.enforceCapacity(int64(len(data)), 1); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// List returns up to limit records (oldest first) as file paths.
func (s *Spool) List(limit int) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !isRecordName(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if limit > 0 && len(names) > limit {
		names = names[:limit]
	}
	paths := make([]string, len(names))
	for i, n := range names {
		paths[i] = filepath.Join(s.dir, n)
	}
	return paths, nil
}

func (s *Spool) Read(path string) (*Record, []byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, nil, err
	}
	return &rec, b, nil
}

func (s *Spool) Remove(path string) error {
	// Best-effort: ignore if already gone
	err := os.Remove(path)
	if err != nil && !errorsIs(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// enforceCapacity ensures adding newSize bytes and newFiles files will not exceed caps.
// If caps would be exceeded, it deletes oldest files until usage is below the low-watermark.
func (s *Spool) enforceCapacity(newSize int64, newFiles int) error {
	// Quick exit if no caps
	hasByteCap := s.maxBytes > 0
	hasFileCap := s.maxFiles > 0
	if !hasByteCap && !hasFileCap {
		return nil
	}

	// Gather files sorted oldest->newest and compute size
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && isRecordName(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var used int64
	for _, n := range names {
		info, err := os.Stat(filepath.Join(s.dir, n))
		if err == nil {
			used += info.Size()
		}
	}
	files := len(names)
	// Check if over
	overBytes := hasByteCap && used+newSize > s.maxBytes
	overFiles := hasFileCap && files+newFiles > s.maxFiles
	if !overBytes && !overFiles {
		return nil
	}
	// Target thresholds
	targetBytes := s.maxBytes
	if hasByteCap {
		targetBytes = int64(float64(s.maxBytes) * s.lowWMScale)
	}
	targetFiles := s.maxFiles
	if hasFileCap {
		targetFiles = int(float64(s.maxFiles) * s.lowWMScale)
	}
	// Delete oldest until under thresholds
	for _, n := range names {
		p := filepath.Join(s.dir, n)
		info, err := os.Stat(p)
		if err == nil {
			used -= info.Size()
		}
		_ = os.Remove(p)
		files--
		// If both caps satisfied, stop
		if (!hasByteCap || used+newSize <= targetBytes) && (!hasFileCap || files+newFiles <= targetFiles) {
			return nil
		}
	}
	// If we deleted everything and still cannot fit, reject
	if hasByteCap && newSize > s.maxBytes {
		return fmt.Errorf("spool record exceeds max bytes cap")
	}
	if hasFileCap && newFiles > s.maxFiles {
		return fmt.Errorf("spool record exceeds max files cap")
	}
	return nil
}

func isRecordName(name string) bool {
	return strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".tmp")
}

func errorsIs(err error, target error) bool {
	// local helper to avoid importing errors in many files
	if err == nil {
		return false
	}
	return os.IsNotExist(err) || err == target
}

// DiskUsage returns bytes used by spool (approximate).
func (s *Spool) DiskUsage() (int64, error) {
	var size int64
	err := filepath.WalkDir(s.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		size += info.Size()
		return nil
	})
	return size, err
}
