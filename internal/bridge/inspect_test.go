package bridge

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPendingSpoolsCountsMessagesAndReportsIOFailure(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bridge/outbox/alice/new")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"one.md", "one.dest", "ignored.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := PendingSpools(root)
	if err != nil || got["alice"] != 1 {
		t.Fatalf("counts: %v %v", got, err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	got, err = PendingSpools(root)
	if err == nil || got != nil {
		t.Fatalf("unreadable queue reported as readable: %v %v", got, err)
	}
}
