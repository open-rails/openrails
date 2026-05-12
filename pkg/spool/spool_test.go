package spool

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListIgnoresTempFiles(t *testing.T) {
	dir := t.TempDir()
	sp, err := New(dir)
	if err != nil {
		t.Fatalf("new spool: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "20260101T000000.000000000Z_1.json.tmp"), []byte("tmp"), 0o644); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "20260101T000000.000000000Z_2.json"), []byte(`{"kind":"payment","data":{}}`), 0o644); err != nil {
		t.Fatalf("write record: %v", err)
	}

	paths, err := sp.List(10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("paths = %d, want 1", len(paths))
	}
	if filepath.Base(paths[0]) != "20260101T000000.000000000Z_2.json" {
		t.Fatalf("listed %q", filepath.Base(paths[0]))
	}
}

func TestCapacityIgnoresTempFiles(t *testing.T) {
	dir := t.TempDir()
	sp := &Spool{dir: dir, maxBytes: 10, lowWMScale: 0.9}

	if err := os.WriteFile(filepath.Join(dir, "old.json.tmp"), []byte("this temp file is larger than the cap"), 0o644); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	if err := sp.enforceCapacity(5, 1); err != nil {
		t.Fatalf("enforce capacity: %v", err)
	}
}
