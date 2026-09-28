package adapter

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	testGrace = 24 * time.Hour
	testKeep  = 30 * 24 * time.Hour
)

func TestPlanGC(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	tombs := []Tombstone{
		{Handle: "fresh", DepartedUnix: ago(time.Hour)},            // within grace: kept, reserved
		{Handle: "gone", DepartedUnix: ago(25 * time.Hour)},        // expired, unused: archive
		{Handle: "back-live", DepartedUnix: ago(25 * time.Hour)},   // expired, a live agent has the name: drop tombstone only
		{Handle: "back-record", DepartedUnix: ago(48 * time.Hour)}, // expired, a record has it: drop tombstone only
	}
	live := []AgentInfo{{PaneID: "w1:p1", Name: str("back-live")}}
	recs := []WakerRecord{{PaneID: "w1:p2", Handle: "back-record"}}
	archives := []Archive{
		{Dir: "old-1790000000", ArchivedUnix: ago(31 * 24 * time.Hour)}, // past retention: purge
		{Dir: "new-1790500000", ArchivedUnix: ago(24 * time.Hour)},      // kept
	}
	got := PlanGC(tombs, live, recs, archives, now, testGrace, testKeep)
	want := GCPlan{
		Archive:  []string{"gone"},
		Forget:   []string{"back-live", "back-record"},
		Purge:    []string{"old-1790000000"},
		Reserved: []string{"fresh"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

// A handle with a tombstone is reserved: an unnamed newcomer of the same
// kind gets another name, so the departed agent's backlog stays unread.
func TestAdoptHandleSkipsReservedHandles(t *testing.T) {
	a := AgentInfo{PaneID: "w1:p3", Agent: str("claude"), Cwd: "/repo/c"}
	if got := AdoptHandle(a, []AgentInfo{a}, WakerRecord{}, false, []string{"claude"}); got != "claude-2" {
		t.Fatalf("got %q", got)
	}
}

func TestTombstoneStoreRoundTrip(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutTombstone(Tombstone{Handle: "claude", DepartedUnix: 42, Cwd: "/r", Agent: "claude"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Tombstones()
	if err != nil || len(got) != 1 || got[0].Handle != "claude" || got[0].DepartedUnix != 42 {
		t.Fatalf("got %+v err %v", got, err)
	}
	if err := s.DeleteTombstone("claude"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTombstone("claude"); err != nil {
		t.Fatal("deleting a missing tombstone must be a no-op:", err)
	}
	if got, _ := s.Tombstones(); len(got) != 0 {
		t.Fatalf("left %+v", got)
	}
	if err := s.PutTombstone(Tombstone{Handle: "../x"}); err == nil {
		t.Fatal("a handle outside Herdr's name grammar must be refused")
	}
}

// RunGC moves an expired mailbox into the archive, rewrites the amq agent
// list without it, forgets reused tombstones and purges old archives.
func TestRunGC(t *testing.T) {
	d := t.TempDir()
	state := filepath.Join(d, "state")
	root := filepath.Join(d, "root")
	s, err := NewStore(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"gone", "keep"} {
		if err := os.MkdirAll(filepath.Join(root, "agents", h, "inbox", "new"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "gone", "inbox", "new", "m1.md"), []byte("backlog"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "meta"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "meta", "config.json"), []byte(`{"agents":["gone","keep"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"gone", "back"} {
		if err := s.PutTombstone(Tombstone{Handle: h, DepartedUnix: 1}); err != nil {
			t.Fatal(err)
		}
	}
	oldArchive := filepath.Join(state, "archive", "older-1")
	if err := os.MkdirAll(oldArchive, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldArchive, archiveMarker), []byte(`{"handle":"older","archived_unix":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(d, "amq-args")
	amq := filepath.Join(d, "amq")
	if err := os.WriteFile(amq, []byte("#!/bin/sh\necho \"$@\" > '"+argsFile+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1790600000, 0)
	plan := GCPlan{Archive: []string{"gone"}, Forget: []string{"back"}, Purge: []string{"older-1"}}
	if err := RunGC(context.Background(), amq, root, s, plan, now); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(state, "archive", "gone-1790600000", "inbox", "new", "m1.md")
	if b, err := os.ReadFile(moved); err != nil || string(b) != "backlog" {
		t.Fatalf("backlog not archived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "gone")); !os.IsNotExist(err) {
		t.Fatal("mailbox still in the root")
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "keep")); err != nil {
		t.Fatal("another mailbox was touched")
	}
	args, _ := os.ReadFile(argsFile)
	if got := strings.TrimSpace(string(args)); got != "init --root "+root+" --agents keep --force" {
		t.Fatalf("amq args %q", got)
	}
	if _, err := os.Stat(oldArchive); !os.IsNotExist(err) {
		t.Fatal("old archive not purged")
	}
	if left, _ := s.Tombstones(); len(left) != 0 {
		t.Fatalf("tombstones left: %+v", left)
	}
	archives, err := s.Archives()
	if err != nil || len(archives) != 1 || archives[0].Dir != "gone-1790600000" || archives[0].ArchivedUnix != 1790600000 {
		t.Fatalf("archives %+v err %v", archives, err)
	}
}

// G1: a handle another pane's record still holds is reserved even before
// that pane's stop has run (hooks arrive in any order).
func TestReservedHandlesIncludeOtherPanesRecords(t *testing.T) {
	tombs := []Tombstone{{Handle: "gone"}}
	recs := []WakerRecord{{PaneID: "w1:p1", Handle: "claude"}, {PaneID: "w1:p2", Handle: "claude-2"}}
	got := ReservedHandles(tombs, recs, "w1:p1")
	if !reflect.DeepEqual(got, []string{"gone", "claude-2"}) {
		t.Fatalf("got %v", got)
	}
}

func setupGC(t *testing.T) (state, root string, s *Store) {
	t.Helper()
	d := t.TempDir()
	state, root = filepath.Join(d, "state"), filepath.Join(d, "root")
	s, err := NewStore(state)
	if err != nil {
		t.Fatal(err)
	}
	return state, root, s
}

// G2: an archive dir that is a symlink is never read or purged through.
func TestArchivesRefuseASymlinkedArchiveDir(t *testing.T) {
	state, _, s := setupGC(t)
	outside := filepath.Join(t.TempDir(), "notes-1")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, archiveMarker), []byte(`{"handle":"notes","archived_unix":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(state, "archive")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Archives(); err == nil || len(got) != 0 {
		t.Fatalf("symlinked archive dir listed: %+v err=%v", got, err)
	}
	if err := RunGC(context.Background(), "/nonexistent/amq", "/r", s, GCPlan{Purge: []string{"notes-1"}}, time.Unix(1790600000, 0)); err == nil {
		t.Fatal("purge through a symlinked archive dir must fail")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("data behind the symlink was removed")
	}
}

// G5: only dirs the adapter archived (with its marker) are archives; a
// look-alike name is left alone, and the time comes from the marker.
func TestArchivesAreOnlyMarkedDirs(t *testing.T) {
	state, _, s := setupGC(t)
	for _, d := range []string{"notes-1", "notes-1.backup", "claude-1790000000"} {
		if err := os.MkdirAll(filepath.Join(state, "archive", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(state, "archive", "claude-1790000000", archiveMarker), []byte(`{"handle":"claude","archived_unix":1790000123}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.Archives()
	if err != nil || len(got) != 1 || got[0].Dir != "claude-1790000000" || got[0].ArchivedUnix != 1790000123 {
		t.Fatalf("got %+v err %v", got, err)
	}
	if err := RunGC(context.Background(), "/nonexistent/amq", "/r", s, GCPlan{Purge: []string{"notes-1"}}, time.Unix(1790600000, 0)); err == nil {
		t.Fatal("purging an unmarked dir must fail")
	}
	if _, err := os.Stat(filepath.Join(state, "archive", "notes-1")); err != nil {
		t.Fatal("unmarked dir was removed")
	}
}

// G4: amq refuses an empty agent list, so archiving the last agent keeps
// its tombstone (and the handle reserved) until the config can drop it.
func TestArchivingTheLastAgentKeepsItsTombstone(t *testing.T) {
	state, root, s := setupGC(t)
	if err := os.MkdirAll(filepath.Join(root, "agents", "solo", "inbox", "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "meta"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "meta", "config.json"), []byte(`{"agents":["solo"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.PutTombstone(Tombstone{Handle: "solo", DepartedUnix: 1}); err != nil {
		t.Fatal(err)
	}
	plan := GCPlan{Archive: []string{"solo"}}
	for i := 0; i < 2; i++ { // a second pass is harmless
		if err := RunGC(context.Background(), "/nonexistent/amq", root, s, plan, time.Unix(1790600000, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if left, _ := s.Tombstones(); len(left) != 1 {
		t.Fatalf("tombstone forgotten while the config still lists the handle: %+v", left)
	}
	if got, _ := s.Archives(); len(got) != 1 {
		t.Fatalf("archives %+v", got)
	}
	_ = state
}

func mailboxRoot(t *testing.T, root string, agents ...string) {
	t.Helper()
	for _, h := range agents {
		if err := os.MkdirAll(filepath.Join(root, "agents", h, "inbox", "new"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "meta"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"agents":["` + strings.Join(agents, `","`) + `"]}`
	if err := os.WriteFile(filepath.Join(root, "meta", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fakeAmq(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// G6: writing the marker never follows a link planted under its name.
func TestArchiveMarkerNeverFollowsALink(t *testing.T) {
	state, root, s := setupGC(t)
	mailboxRoot(t, root, "gone", "keep")
	victim := filepath.Join(t.TempDir(), "user-file")
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "agents", "gone", archiveMarker)); err != nil {
		t.Fatal(err)
	}
	if err := RunGC(context.Background(), fakeAmq(t), root, s, GCPlan{Archive: []string{"gone"}}, time.Unix(1790600000, 0)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "precious" {
		t.Fatalf("file behind the link was overwritten: %q", b)
	}
	if got, _ := s.Archives(); len(got) != 1 || got[0].ArchivedUnix != 1790600000 {
		t.Fatalf("archives %+v", got)
	}
	_ = state
}

// G9: a pass that died after marking but before moving is finished by the
// next one, and the archive is dated by that pass.
func TestInterruptedArchiveIsCompleted(t *testing.T) {
	_, root, s := setupGC(t)
	mailboxRoot(t, root, "gone", "keep")
	stale := filepath.Join(root, "agents", "gone", archiveMarker)
	if err := os.WriteFile(stale, []byte(`{"handle":"gone","archived_unix":5}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunGC(context.Background(), fakeAmq(t), root, s, GCPlan{Archive: []string{"gone"}}, time.Unix(1790600000, 0)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Archives()
	if len(got) != 1 || got[0].ArchivedUnix != 1790600000 {
		t.Fatalf("archives %+v", got)
	}
}

// G8: an archive dir that cannot be searched fails the pass at once
// instead of looping on candidate names.
func TestArchiveTargetErrorsAreReturned(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	state, root, s := setupGC(t)
	mailboxRoot(t, root, "gone", "keep")
	base := filepath.Join(state, "archive")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o200); err != nil { // write, no search
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(base, 0o755) })
	done := make(chan error, 1)
	go func() {
		done <- RunGC(context.Background(), fakeAmq(t), root, s, GCPlan{Archive: []string{"gone"}}, time.Unix(1790600000, 0))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an unsearchable archive dir must be an error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("gc looped on an archive target error")
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "gone")); err != nil {
		t.Fatal("mailbox moved despite the error")
	}
}

// G7: stop died between the tombstone and the record; the same agent
// resuming unnamed keeps its handle.
func TestInheritedHandleWinsOverItsOwnTombstone(t *testing.T) {
	rec := WakerRecord{PaneID: "w1:p1", Handle: "claude-7", Cwd: "/repo/a", Agent: "claude"}
	a := AgentInfo{PaneID: "w1:p1", Agent: str("claude"), Cwd: "/repo/a"}
	reserved := ReservedHandles([]Tombstone{{Handle: "claude-7"}}, []WakerRecord{rec}, "w1:p1")
	if got := AdoptHandle(a, []AgentInfo{a}, rec, true, reserved); got != "claude-7" {
		t.Fatalf("got %q", got)
	}
}

// G10: a link planted at a temp-file name is never written through.
func TestPutTombstoneNeverWritesThroughALink(t *testing.T) {
	state, _, s := setupGC(t)
	victim := filepath.Join(t.TempDir(), "user-file")
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(state, "tombstones")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "claude.json.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutTombstone(Tombstone{Handle: "claude", DepartedUnix: 1}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "precious" {
		t.Fatalf("overwritten: %q", b)
	}
}

// G11: a tombstone counts only as a regular file named after its handle.
func TestTombstonesFailClosedOnMismatch(t *testing.T) {
	state, _, s := setupGC(t)
	dir := filepath.Join(state, "tombstones")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{"handle":"b","departed_unix":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Tombstones(); err == nil {
		t.Fatalf("mismatched tombstone accepted: %+v", got)
	}
	if err := os.Remove(filepath.Join(dir, "a.json")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "x.json")
	if err := os.WriteFile(target, []byte(`{"handle":"c","departed_unix":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "c.json")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Tombstones(); err == nil {
		t.Fatalf("linked tombstone accepted: %+v", got)
	}
}

// G12: an existing file under the marker name that is not this adapter's
// marker for the same handle is kept, and archiving that mailbox fails.
func TestForeignFileUnderTheMarkerNameIsKept(t *testing.T) {
	_, root, s := setupGC(t)
	mailboxRoot(t, root, "gone", "keep")
	foreign := filepath.Join(root, "agents", "gone", archiveMarker)
	if err := os.WriteFile(foreign, []byte("user notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunGC(context.Background(), fakeAmq(t), root, s, GCPlan{Archive: []string{"gone"}}, time.Unix(1790600000, 0)); err == nil {
		t.Fatal("archiving over a foreign file must fail")
	}
	if b, _ := os.ReadFile(foreign); string(b) != "user notes" {
		t.Fatalf("foreign file changed: %q", b)
	}
}
