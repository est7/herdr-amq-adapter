package adapter

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Spawn must produce a detached, terminable process whose stdio is not the
// test's own pipes; a fake amq that ignores its argv stands in for the real one.
func TestSpawnDetachedAndTerminate(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-amq")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec, err := Spawn(WakerSpec{
		AmqBin: fake, SelfBin: "/nonexistent/self", LogDir: filepath.Join(dir, "logs"),
		Agent: AgentInfo{PaneID: "w1:p1", Cwd: dir}, Handle: "reviewer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !Alive(rec.PID) {
		t.Fatalf("pid %d not alive after spawn", rec.PID)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", "w1_p1.log")); err != nil {
		t.Errorf("log file missing: %v", err)
	}
	if err := Terminate(rec.PID); err != nil {
		t.Fatal(err)
	}
	// The test process is the spawner, so the exited child lingers as a
	// zombie until reaped (in production the hook exits and init reaps).
	// Reap it here so Alive reflects the real outcome.
	var status syscall.WaitStatus
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		wpid, err := syscall.Wait4(rec.PID, &status, syscall.WNOHANG, nil)
		if err != nil || wpid == rec.PID {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if Alive(rec.PID) {
		t.Fatalf("pid %d still alive after SIGTERM to its process group", rec.PID)
	}
	if !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Errorf("expected death by SIGTERM, got %v", status)
	}
}
