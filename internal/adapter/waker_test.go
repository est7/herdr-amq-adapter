package adapter

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Spawn must produce a detached, terminable process whose stdio is not the
// test's own pipes; a fake amq that ignores its argv (but keeps it on its
// command line, as the real one does) stands in for the real one.
func TestSpawnDetachedAndTerminate(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-amq")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec, err := Spawn(WakerSpec{
		AmqBin: fake, SelfBin: "/nonexistent/self", LogDir: filepath.Join(dir, "logs"),
		Agent: AgentInfo{PaneID: "w1:p1", Cwd: dir}, Handle: "reviewer", Root: "/state/amq-root",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.SelfBin != "/nonexistent/self" {
		t.Errorf("record self_bin %q", rec.SelfBin)
	}
	if !Alive(rec.PID) {
		t.Fatalf("pid %d not alive after spawn", rec.PID)
	}
	if !WakerAlive(rec) {
		t.Fatalf("pid %d serving %q not recognised as the recorded waker", rec.PID, rec.Handle)
	}
	// Same pid, but the record disagrees on any part of the waker's argv:
	// a reused pid after reboot, or a stale waker for another pane/root/binary.
	for name, mutate := range map[string]func(*WakerRecord){
		"handle":  func(w *WakerRecord) { w.Handle = "someone-else" },
		"pane":    func(w *WakerRecord) { w.PaneID = "w9:p9" },
		"root":    func(w *WakerRecord) { w.Root = "/elsewhere" },
		"selfbin": func(w *WakerRecord) { w.SelfBin = "/plugins/old/adapter" },
	} {
		other := rec
		mutate(&other)
		if WakerAlive(other) {
			t.Errorf("pid %d accepted for a record with a different %s", rec.PID, name)
		}
	}
	if WakerAlive(WakerRecord{PID: 0, Handle: "reviewer"}) {
		t.Fatal("parked record (pid 0) must not be alive")
	}
	// A record from before self_bin existed still identifies its waker, so
	// reconcile can replace it instead of leaving a second waker behind.
	legacy := rec
	legacy.SelfBin = ""
	if !WakerAlive(legacy) {
		t.Fatal("legacy record without self_bin must still identify its own waker")
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", "w1_p1.log")); err != nil {
		t.Errorf("log file missing: %v", err)
	}
	if err := TerminateWaker(rec); err != nil {
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

// A recorded pid that is not positively the recorded waker must never be
// signalled: after a reboot it can belong to an unrelated process group.
func TestTerminateWakerRefusesUnidentifiedPid(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	rec := WakerRecord{PID: cmd.Process.Pid, Handle: "reviewer", PaneID: "w1:p1", Root: "/r", SelfBin: "/s"}
	if err := TerminateWaker(rec); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if !Alive(rec.PID) {
		t.Fatalf("unrelated pid %d was signalled", rec.PID)
	}
}

func TestStoreLockIsExclusiveAcrossDescriptors(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := st.Lock()
	if err != nil {
		t.Fatal(err)
	}
	probe, err := os.OpenFile(filepath.Join(st.dir, ".lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatal("second descriptor acquired the lock while it was held")
	}
	unlock()
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("lock not released: %v", err)
	}
}
