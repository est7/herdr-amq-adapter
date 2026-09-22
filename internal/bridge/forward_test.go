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
