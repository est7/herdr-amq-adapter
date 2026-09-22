package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForwardRecoveryAndConflict(t *testing.T) {
	bin := filepath.Join(os.Getenv("HOME"), ".local/bin/amq-bridge")
	if _, err := os.Stat(bin); err != nil {
		t.Skip(err)
	}
	d := t.TempDir()
	e := Env{Root: filepath.Join(d, "root"), StateDir: filepath.Join(d, "state"), BridgeBin: bin, Local: Local{Host: "mac"}}
	id, _, _ := headerIDFrom([]byte(sample))
	msg, err := Readdress([]byte(sample), "mac-claude", "codex")
	if err != nil {
		t.Fatal(err)
	}
	marker := forwardedPath(e.Root, "mac-claude", id, "heping/codex")
	// Enqueue succeeds but publishing the completion marker fails.
	if err := os.MkdirAll(marker, 0700); err != nil {
		t.Fatal(err)
	}
	// A read error must fail closed before enqueue.
	if err := enqueueDestination(context.Background(), e, "mac-claude", id, "heping/codex", msg); err == nil {
		t.Fatal("unreadable marker ignored")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	cfg, err := WriteEnqueueConfig(e.StateDir, EnqueueConfig{Root: e.Root, SourceHost: "mac", SourceHandle: "mac-claude", AllowedDestAliases: []string{"heping/codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(context.Background(), bin, cfg, "heping/codex", msg); err != nil {
		t.Fatal(err)
	}
	// Simulate a restart after enqueue and before the marker or alias move.
	if err := enqueueDestination(context.Background(), e, "mac-claude", id, "heping/codex", msg); err != nil {
		t.Fatal(err)
	}
	if err := enqueueDestination(context.Background(), e, "mac-claude", id, "heping/codex", append(msg, []byte("changed")...)); err == nil {
		t.Fatal("same key accepted different payload")
	}
	if err := prepareSpool(e.Root, "mac-claude"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(e.Root, "bridge/outbox/mac-claude/new", id+".dest")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := prepareSpool(e.Root, "mac-claude"); err == nil || !strings.Contains(err.Error(), "refusing default destination") {
		t.Fatalf("incomplete enqueue may be misrouted: %v", err)
	}
}

// A sent copy with a leftover .dest sidecar and no transport receipt cannot
// be resolved by retrying: it is reported as needing an operator, the
// alias message stays in new, and nothing is enqueued.
func TestOrphanSidecarWithoutReceiptNeedsOperator(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	src, id := "mac-claude", "id-orphan"
	msg := strings.Replace(sample, "2026-09-22T06-08-16.891Z_pid28972_b53cc11f", id, 1)
	for _, d := range []string{"agents/heping-codex/inbox/new", "bridge/outbox/" + src + "/new", "bridge/outbox/" + src + "/sent"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "agents/heping-codex/inbox/new", id+".md"), []byte(msg), 0o600)
	os.WriteFile(filepath.Join(root, "bridge/outbox", src, "sent", id+".md"), []byte("earlier bytes"), 0o600)
	os.WriteFile(filepath.Join(root, "bridge/outbox", src, "new", id+".dest"), []byte("heping/pi\n"), 0o600)
	fake := filepath.Join(dir, "fake-amq-bridge")
	os.WriteFile(fake, []byte("#!/bin/sh\necho should-not-run >&2; exit 1\n"), 0o755)
	env := Env{BridgeBin: fake, Root: root, StateDir: filepath.Join(dir, "state"), Local: Local{Host: "mac"},
		Peers: []Peer{{Host: "heping", Agents: []string{"codex"}}}}
	rep := Tick(context.Background(), env)
	if len(rep.Stuck) != 1 || len(rep.Errors) != 0 || len(rep.Forwarded) != 0 {
		t.Fatalf("report %+v errors %v", rep.Stuck, rep.Errors)
	}
	if !strings.Contains(rep.Stuck[0].Action, "heping/pi") || rep.Stuck[0].Key == "" {
		t.Errorf("operator item lacks action/key: %+v", rep.Stuck[0])
	}
	if _, err := os.Stat(filepath.Join(root, "agents/heping-codex/inbox/new", id+".md")); err != nil {
		t.Error("alias message must stay in new while stuck")
	}
}
