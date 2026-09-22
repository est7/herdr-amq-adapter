package adapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// WakerAlive reports whether the record's pid is still exactly the waker it
// was recorded as. A bare pid probe is not enough: after a reboot (the
// startup reconcile path) the pid is routinely reused by an unrelated
// process, which would leave the pane without a waker for good. The whole
// `amq wake` argv the record implies must match the live command line, so a
// stale waker for the same handle but another pane, root, or adapter binary
// is not mistaken for the current one either.
func WakerAlive(w WakerRecord) bool {
	if !Alive(w.PID) {
		return false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(w.PID), "-o", "args=").Output()
	if err != nil {
		return false
	}
	want := AmqWakeArgs(w.SelfBin, w.Handle, w.PaneID, w.Root)
	got := strings.Fields(string(out))
	if len(got) < len(want)+1 {
		return false
	}
	got = got[len(got)-len(want):]
	for i := range want {
		// A record written before self_bin existed cannot name its binary;
		// accept whatever --inject-via it runs so it can be identified and
		// then replaced (its SelfBin never equals the current binary).
		if want[i] == "" && w.SelfBin == "" && i > 0 && want[i-1] == "--inject-via" {
			continue
		}
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TerminateWaker stops the record's waker only after WakerAlive positively
// identifies it. A pid that fails identification is never signalled: after
// a reboot it may belong to an unrelated process group.
func TerminateWaker(w WakerRecord) error {
	if !WakerAlive(w) {
		return nil
	}
	return Terminate(w.PID)
}

// Lock serialises lifecycle transitions (hook, reconcile) across processes.
// Every transition is a multi-step read/spawn/write sequence over the same
// records, identity files, and amq config; Herdr may run hooks concurrently.
// The returned func releases the lock.
func (s *Store) Lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(s.dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lifecycle lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("acquire lifecycle lock: %w", err)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// Terminate sends SIGTERM to the process group of a pid this process just
// spawned (setsid, so pid == pgid). For a recorded pid use TerminateWaker.
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
