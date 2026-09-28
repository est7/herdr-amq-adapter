package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInjectLogKeepsTheLastOutcomePerHandle(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC)
	if _, ok, err := LastInject(dir, "claude"); ok || err != nil {
		t.Fatalf("empty log: ok=%v err=%v", ok, err)
	}
	for i, o := range []Outcome{
		{Progress: ProgressDeferred, Code: "draft_in_box", Note: "the input box holds unsent text"},
		{Progress: ProgressAccepted, Note: `{"result":"stdout is not kept"}`},
		{Progress: ProgressDeferred, Code: "trust_screen", Note: "Trust this folder?"},
	} {
		if err := RecordInject(dir, "claude", o, t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := RecordInject(dir, "codex", Outcome{Progress: ProgressFailed, Code: "agent_not_found"}, t0); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LastInject(dir, "claude")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	want := InjectEntry{At: t0.Add(2 * time.Minute), Progress: ProgressDeferred, Code: "trust_screen", Note: "Trust this folder?"}
	if !got.At.Equal(want.At) || got.Progress != want.Progress || got.Code != want.Code || got.Note != want.Note {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if got, _, _ := LastInject(dir, "codex"); got.Code != "agent_not_found" {
		t.Fatalf("codex: got %+v", got)
	}
	b, err := os.ReadFile(filepath.Join(dir, "inject", "claude.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "stdout is not kept") {
		t.Fatal("an accepted prompt's stdout was logged")
	}
}

// The log is bounded: past the cap it rotates to .1 and keeps working.
func TestInjectLogRotates(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", 1000)
	for i := 0; i < 400; i++ {
		if err := RecordInject(dir, "claude", Outcome{Progress: ProgressDeferred, Code: "draft_in_box", Note: long}, time.Unix(int64(i), 0)); err != nil {
			t.Fatal(err)
		}
	}
	st, err := os.Stat(filepath.Join(dir, "inject", "claude.jsonl"))
	if err != nil || st.Size() > injectLogCap {
		t.Fatalf("log size %v err %v", st.Size(), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "inject", "claude.jsonl.1")); err != nil {
		t.Fatalf("no rotated file: %v", err)
	}
	if got, ok, _ := LastInject(dir, "claude"); !ok || got.At.Unix() != 399 {
		t.Fatalf("last after rotation: %+v", got)
	}
}

// G3: logging runs inside the injector's time budget: a stuck write is
// abandoned rather than delaying the exit amq is waiting for.
func TestWithinGivesUpOnAStuckStep(t *testing.T) {
	start := time.Now()
	err := Within(50*time.Millisecond, func() error { time.Sleep(2 * time.Second); return nil })
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
	if err := Within(time.Second, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}
