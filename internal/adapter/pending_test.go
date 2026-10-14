package adapter

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPendingMail(t *testing.T) {
	root := t.TempDir()
	if n, err := PendingMail(root, "codex"); err != nil || n != 0 {
		t.Fatalf("missing inbox: n=%d err=%v, want 0 nil", n, err)
	}
	dir := filepath.Join(root, "agents", "codex", "inbox", "new")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.md", "b.md", "tmp.partial"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "a.md"), filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	n, err := PendingMail(root, "codex")
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v, want 2 regular .md files", n, err)
	}
}

func TestDecideKick(t *testing.T) {
	const sockA, sockB = "/tmp/server-a.sock", "/tmp/server-b.sock"
	live := WakerRecord{PaneID: "w1:p2", Handle: "codex", Root: "/r", PID: 42, ServerSocket: sockA}
	deferred := InjectEntry{Progress: ProgressDeferred, Code: "agent_blocked"}
	accepted := InjectEntry{Progress: ProgressAccepted}
	parked := live
	parked.PID = 0
	unbound := live
	unbound.ServerSocket = ""
	other := WakerRecord{PaneID: "w1:p9", Handle: "claude", PID: 7, ServerSocket: sockA}
	cases := []struct {
		name     string
		recs     []WakerRecord
		socket   string
		last     InjectEntry
		haveLast bool
		pending  int
		want     bool
	}{
		{"kick", []WakerRecord{live, other}, sockA, deferred, true, 2, true},
		{"foreign server event", []WakerRecord{live}, sockB, deferred, true, 2, false},
		{"no socket", []WakerRecord{live}, "", deferred, true, 2, false},
		{"unbound record", []WakerRecord{unbound}, sockA, deferred, true, 2, false},
		{"parked", []WakerRecord{parked}, sockA, deferred, true, 2, false},
		{"no record", []WakerRecord{other}, sockA, deferred, true, 2, false},
		{"last accepted: already announced", []WakerRecord{live}, sockA, accepted, true, 2, false},
		{"no inject history", []WakerRecord{live}, sockA, InjectEntry{}, false, 2, false},
		{"inbox empty", []WakerRecord{live}, sockA, deferred, true, 0, false},
	}
	for _, c := range cases {
		v := DecideKick(c.recs, c.socket, "w1:p2", c.last, c.haveLast, c.pending)
		if v.Kick != c.want {
			t.Errorf("%s: kick=%v (%s), want %v", c.name, v.Kick, v.Reason, c.want)
		}
	}
}

// TestKickOnceConcurrent is the self-induced race: a delivered doorbell flips
// the agent to working, Herdr fires another status hook while the first kick
// has not yet recorded "accepted". Under the per-handle kick lock exactly one
// of the concurrent kicks delivers.
func TestKickOnceConcurrent(t *testing.T) {
	stateDir, root := t.TempDir(), t.TempDir()
	const sock = "/tmp/server-a.sock"
	inbox := filepath.Join(root, "agents", "codex", "inbox", "new")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RecordInject(stateDir, "codex", Outcome{Progress: ProgressDeferred, Code: "agent_blocked"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec := WakerRecord{PaneID: "w1:p2", Handle: "codex", Root: root, PID: 42, ServerSocket: sock}
	load := func() ([]WakerRecord, error) { return []WakerRecord{rec}, nil }
	var delivered atomic.Int32
	deliver := func(WakerRecord) Outcome {
		delivered.Add(1)
		time.Sleep(100 * time.Millisecond) // the window where the second hook arrives
		return Outcome{Progress: ProgressAccepted}
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := KickOnce(stateDir, "codex", sock, "w1:p2", load, deliver, time.Now); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := delivered.Load(); n != 1 {
		t.Fatalf("delivered %d doorbells for one deferred cohort, want 1", n)
	}
	last, ok, err := LastInject(stateDir, "codex")
	if err != nil || !ok || last.Progress != ProgressAccepted {
		t.Fatalf("last inject = %+v ok=%v err=%v, want accepted", last, ok, err)
	}
}
