package adapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Store persists WakerRecords as one JSON file per pane under
// $HERDR_PLUGIN_STATE_DIR/wakers/. Pane IDs contain ':' which is filesystem
// safe on Unix; the file name is the pane id with ':' replaced by '_'.
type Store struct{ dir string }

func NewStore(stateDir string) (*Store, error) {
	if stateDir == "" {
		return nil, errors.New("HERDR_PLUGIN_STATE_DIR is not set")
	}
	dir := filepath.Join(stateDir, "wakers")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(paneID string) string {
	return filepath.Join(s.dir, strings.ReplaceAll(paneID, ":", "_")+".json")
}

func (s *Store) Get(paneID string) (WakerRecord, bool, error) {
	b, err := os.ReadFile(s.path(paneID))
	if errors.Is(err, os.ErrNotExist) {
		return WakerRecord{}, false, nil
	}
	if err != nil {
		return WakerRecord{}, false, err
	}
	var w WakerRecord
	if err := json.Unmarshal(b, &w); err != nil {
		return WakerRecord{}, false, fmt.Errorf("corrupt record %s: %w", s.path(paneID), err)
	}
	return w, true, nil
}

func (s *Store) Put(w WakerRecord) error {
	b, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(w.PaneID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(w.PaneID))
}

func (s *Store) Delete(paneID string) error {
	err := os.Remove(s.path(paneID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Store) List() ([]WakerRecord, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []WakerRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var w WakerRecord
		if err := json.Unmarshal(b, &w); err != nil {
			return nil, fmt.Errorf("corrupt record %s: %w", e.Name(), err)
		}
		out = append(out, w)
	}
	return out, nil
}

// Alive reports whether pid still exists (signal 0 probe).
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Terminate sends SIGTERM to the waker's own process group (it was spawned
// with setsid, so pid == pgid).
func Terminate(pid int) error {
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(-pid, syscall.SIGTERM)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
