package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestConcurrentMailboxRegistration(t *testing.T) {
	bin, err := exec.LookPath("amq")
	if err != nil {
		t.Skip(err)
	}
	root := filepath.Join(t.TempDir(), "root")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for _, h := range []string{"alice", "bob", "peer-codex", "peer-pi", "claude", "codex", "pi", "reviewer"} {
		wg.Add(1)
		go func(h string) { defer wg.Done(); errs <- EnsureMailbox(context.Background(), bin, root, h) }(h)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(filepath.Join(root, "meta/config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct{ Agents []string }
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 8 {
		t.Fatalf("lost registrations: %v", cfg.Agents)
	}
}

func TestMailboxLockWaitHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	unlock, err := lockMailboxRegistry(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := EnsureMailbox(ctx, "/must-not-run", root, "alice"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait ignored deadline: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "meta/config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled waiter changed registry")
	}
}
