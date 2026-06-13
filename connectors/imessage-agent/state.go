package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// state is the small persisted high-water mark for incremental runs: the
// largest message ROWID already uploaded. The next run reads it and queries
// only newer rows. It is JSON so a human can inspect or reset it.
type state struct {
	// LastRowID is the highest message.ROWID uploaded so far. The next
	// incremental query asks for ROWID > LastRowID.
	LastRowID int64 `json:"last_rowid"`
}

// loadState reads the state file at path. A missing file is not an error: it
// returns the zero state (LastRowID 0), meaning "start from the beginning".
// A present-but-corrupt file is an error so a typo does not silently re-upload
// the entire history.
func loadState(path string) (state, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return state{}, nil
		}
		return state{}, fmt.Errorf("read state file %s: %w", path, err)
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return state{}, fmt.Errorf("state file %s is corrupt: %w", path, err)
	}
	return s, nil
}

// saveState writes s to path atomically (write to a temp file in the same
// directory, then rename) so an interrupted write cannot corrupt the
// high-water mark. The parent directory is created if absent.
func saveState(path string, s state) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create state dir %s: %w", dir, err)
		}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".imessage-state-*")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("commit state file %s: %w", path, err)
	}
	return nil
}
