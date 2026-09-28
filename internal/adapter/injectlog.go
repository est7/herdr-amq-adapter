package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// injectLogCap bounds one handle's log; past it the log moves to .1 once.
const injectLogCap = 64 << 10

// InjectEntry is one delivery attempt as the injector saw it: the marker
// it gave amq and why. amq's own notification ledger (`amq trace`) keeps
// per-message states but records a deferral only as "exit status 1"; this
// log keeps the reason (draft_in_box, trust_screen, …) per handle.
type InjectEntry struct {
	At       time.Time `json:"at"`
	Progress Progress  `json:"progress"`
	Code     string    `json:"code,omitempty"`
	Note     string    `json:"note,omitempty"`
}

func injectLogPath(stateDir, handle string) (string, error) {
	if !herdrNameRe.MatchString(handle) {
		return "", fmt.Errorf("inject log: invalid handle %q", handle)
	}
	return filepath.Join(stateDir, "inject", handle+".jsonl"), nil
}

// RecordInject appends one attempt to handle's log. One waker per handle
// runs its injector sequentially, so appends and rotation do not race.
func RecordInject(stateDir, handle string, o Outcome, at time.Time) error {
	path, err := injectLogPath(stateDir, handle)
	if err != nil {
		return err
	}
	e := InjectEntry{At: at.UTC(), Progress: o.Progress, Code: o.Code}
	if o.Progress != ProgressAccepted { // an accepted prompt's note is herdr's stdout
		e.Note = truncate(o.Note, 200)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil && st.Size()+int64(len(line)) > injectLogCap {
		if err := os.Rename(path, path+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(line)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// LastInject returns handle's most recent attempt, and false when none was
// recorded.
func LastInject(stateDir, handle string) (InjectEntry, bool, error) {
	path, err := injectLogPath(stateDir, handle)
	if err != nil {
		return InjectEntry{}, false, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return InjectEntry{}, false, nil
	}
	if err != nil {
		return InjectEntry{}, false, err
	}
	b = bytes.TrimRight(b, "\n")
	if len(b) == 0 {
		return InjectEntry{}, false, nil
	}
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		b = b[i+1:]
	}
	var e InjectEntry
	if err := json.Unmarshal(b, &e); err != nil {
		return InjectEntry{}, false, fmt.Errorf("inject log %s: last line: %w", path, err)
	}
	return e, true, nil
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
