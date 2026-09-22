package adapter

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Spawn must produce a detached process whose stdio is not the test's own
// pipes; a fake amq stands in for the real one.
func TestSpawnDetached(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-amq")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pid, err := Spawn(WakerSpec{
		AmqBin: fake, SelfBin: "/nonexistent/self", LogDir: filepath.Join(dir, "logs"),
		Agent: AgentInfo{PaneID: "w1:p1", Cwd: dir}, Handle: "reviewer", ArgvPane: "w1:p1", Root: "/state/amq-root",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("pid %d not alive after spawn: %v", pid, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", "w1_p1.log")); err != nil {
		t.Errorf("log file missing: %v", err)
	}
	// Detached: the child is its own session/process-group leader.
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		t.Errorf("pgid=%d err=%v; want own process group", pgid, err)
	}
}

// Real amq lifecycle: spawn, prove live with the wanted target, refuse a
// foreign target, retire by generation, then stale + repair.
func TestWakeLifecycleWithRealAmq(t *testing.T) {
	amq, err := exec.LookPath("amq")
	if err != nil {
		t.Skip("amq not on PATH")
	}
	// amq refuses injectors under group/world-writable parents (e.g. /tmp);
	// t.TempDir lives under the user's private cache on macOS/Linux.
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if out, err := exec.Command(amq, "--no-update-check", "init", "--root", root, "--agents", "bob").CombinedOutput(); err != nil {
		t.Fatalf("amq init: %v: %s", err, out)
	}
	inj := filepath.Join(dir, "inj.sh")
	if err := os.WriteFile(inj, []byte("#!/bin/sh\necho AMQ_INJECT_PROGRESS=accepted >&2\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	want := ExpectedTarget(inj, "bob", "w1:p1", root)

	st, err := WakeCheck(ctx, amq, root, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if d := DecideWake(st, want); d != DecisionStart {
		t.Fatalf("fresh root: decision %s, want start (%+v)", d, st)
	}
	pid, err := Spawn(WakerSpec{AmqBin: amq, SelfBin: inj, LogDir: filepath.Join(dir, "logs"),
		Agent: AgentInfo{PaneID: "w1:p1", Cwd: dir}, Handle: "bob", ArgvPane: "w1:p1", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	liveCtx, liveCancel := context.WithTimeout(ctx, 10*time.Second)
	defer liveCancel()
	st, err = AwaitLive(liveCtx, amq, root, "bob", want)
	if err != nil {
		t.Fatal(err)
	}
	if st.PID != pid || st.Generation == "" {
		t.Fatalf("live state %+v, want pid %d with a generation", st, pid)
	}
	// A different wanted target (other pane, other binary) is not this waker.
	if d := DecideWake(st, ExpectedTarget(inj, "bob", "w9:p9", root)); d != DecisionReplace {
		t.Errorf("foreign pane: decision %s, want replace", d)
	}
	if d := DecideWake(st, ExpectedTarget("/other/adapter", "bob", "w1:p1", root)); d != DecisionReplace {
		t.Errorf("foreign binary: decision %s, want replace", d)
	}
	// Retire with a stale generation must be refused by amq.
	stale := st
	stale.Generation = "00000000000000000000000000000000"
	if err := WakeRetire(ctx, amq, root, "bob", stale); err == nil {
		t.Fatal("retire with a wrong generation must be refused")
	}
	if err := WakeRetire(ctx, amq, root, "bob", st); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ = WakeCheck(ctx, amq, root, "bob"); st.Status == "missing" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st.Status != "missing" {
		t.Fatalf("after retire: status %q", st.Status)
	}
	if err := WakeRetire(ctx, amq, root, "bob", st); err != nil {
		t.Fatalf("retire with nothing to retire must be a no-op: %v", err)
	}

	// Stale lock (waker killed hard) is repaired from its saved target.
	pid2, err := Spawn(WakerSpec{AmqBin: amq, SelfBin: inj, LogDir: filepath.Join(dir, "logs"),
		Agent: AgentInfo{PaneID: "w1:p1", Cwd: dir}, Handle: "bob", ArgvPane: "w1:p1", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid2, syscall.SIGKILL) })
	liveCtx2, liveCancel2 := context.WithTimeout(ctx, 10*time.Second)
	defer liveCancel2()
	if _, err := AwaitLive(liveCtx2, amq, root, "bob", want); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(pid2, syscall.SIGKILL)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ = WakeCheck(ctx, amq, root, "bob"); st.Status == "stale" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if d := DecideWake(st, want); d != DecisionRepair {
		t.Fatalf("after SIGKILL: status %q decision %s, want repair", st.Status, d)
	}
	if err := WakeRepair(ctx, amq, root, "bob"); err != nil {
		t.Fatal(err)
	}
	liveCtx3, liveCancel3 := context.WithTimeout(ctx, 10*time.Second)
	defer liveCancel3()
	st, err = AwaitLive(liveCtx3, amq, root, "bob", want)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-st.PID, syscall.SIGKILL) })
	if err := WakeRetire(ctx, amq, root, "bob", st); err != nil {
		t.Fatal(err)
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
