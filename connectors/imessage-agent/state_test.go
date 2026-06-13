package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadStateMissingFile(t *testing.T) {
	s, err := loadState(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("loadState on missing file should not error: %v", err)
	}
	if s.LastRowID != 0 {
		t.Errorf("LastRowID = %d, want 0 for missing file", s.LastRowID)
	}
}

func TestSaveAndLoadStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	must(t, saveState(path, state{LastRowID: 12345}))

	s, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if s.LastRowID != 12345 {
		t.Errorf("LastRowID = %d, want 12345", s.LastRowID)
	}
}

func TestSaveStateCreatesDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "state.json")
	must(t, saveState(path, state{LastRowID: 1}))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not created: %v", err)
	}
}

func TestLoadStateCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(path); err == nil {
		t.Error("loadState accepted corrupt file, want error")
	}
}

func TestSaveStateAtomicOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	must(t, saveState(path, state{LastRowID: 1}))
	must(t, saveState(path, state{LastRowID: 99}))
	s, err := loadState(path)
	must(t, err)
	if s.LastRowID != 99 {
		t.Errorf("LastRowID = %d, want 99 after overwrite", s.LastRowID)
	}
	// No leftover temp files in the directory.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Errorf("leftover file in state dir: %s", e.Name())
		}
	}
}
