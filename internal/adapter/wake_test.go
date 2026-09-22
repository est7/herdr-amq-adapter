package adapter

import (
	"context"
	"testing"
)

const checkValid = `{"schema":2,"agent":"claude-2","wake":{"status":"valid","live":true,"pid":88168,"mode":"inject-via","owner_bound":false,"generation":"565a19d9fb55b5fef7564769ebc161d7","target_digest":"sha256:b0b8"}}`
const checkStale = `{"schema":2,"agent":"bob","wake":{"status":"stale","live":false,"pid":21119,"generation":"595690c624217b8bdb2e631c44db7411","target_digest":"sha256:x"}}`
const checkMissing = `{"schema":2,"agent":"bob","wake":{"status":"missing","live":false,"pid":null,"mode":null,"owner_bound":false,"generation":null,"target_digest":null}}`

func TestParseWakeCheck(t *testing.T) {
	st, err := ParseWakeCheck([]byte(checkValid))
	if err != nil || st.Status != "valid" || !st.Live || st.PID != 88168 || st.Generation != "565a19d9fb55b5fef7564769ebc161d7" {
		t.Fatalf("valid: %+v err=%v", st, err)
	}
	st, err = ParseWakeCheck([]byte(checkMissing))
	if err != nil || st.Status != "missing" || st.Live || st.PID != 0 || st.Generation != "" {
		t.Fatalf("missing: %+v err=%v", st, err)
	}
	if _, err := ParseWakeCheck([]byte(`{"schema":1,"wake":{}}`)); err == nil {
		t.Error("schema 1 must be rejected")
	}
}

func TestDecideWake(t *testing.T) {
	want := ExpectedTarget("/plugins/cur/adapter", "claude-2", "w8:p7", "/root")
	other := ExpectedTarget("/plugins/old/adapter", "claude-2", "w8:p7", "/root")
	moved := ExpectedTarget("/plugins/cur/adapter", "claude-2", "wB:p2", "/root")
	cases := []struct {
		name string
		js   string
		tgt  WakeTarget
		has  bool
		want WakeDecision
	}{
		{"live same", checkValid, want, true, DecisionKeep},
		{"live old binary", checkValid, other, true, DecisionReplace},
		{"live but record keyed by moved pane", checkValid, moved, true, DecisionReplace},
		{"live no target file", checkValid, WakeTarget{}, false, DecisionReplace},
		{"stale same", checkStale, want, true, DecisionRepair},
		{"stale other", checkStale, other, true, DecisionReplace},
		{"missing", checkMissing, WakeTarget{}, false, DecisionStart},
	}
	for _, c := range cases {
		st, err := ParseWakeCheck([]byte(c.js))
		if err != nil {
			t.Fatal(err)
		}
		st.Target, st.HasTarget = c.tgt, c.has
		if got := DecideWake(st, want); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

// The retire identity must be the saved target verbatim, in amq's flag
// order, so a waker for another pane or binary is never matched.
func TestRetireArgsFromTarget(t *testing.T) {
	tgt := WakeTarget{InjectVia: "/a/adapter", InjectArgs: []string{"inject", "w1:p1", "bob", "/root"}, RetryUntil: "injected"}
	got := tgt.retireArgs()
	want := []string{"--inject-via", "/a/adapter", "--inject-arg", "inject", "--inject-arg", "w1:p1", "--inject-arg", "bob", "--inject-arg", "/root", "--retry-until", "injected"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d: got %q want %q", i, got[i], want[i])
		}
	}
	if !ExpectedTarget("/a/adapter", "bob", "w1:p1", "/root").Equal(tgt) {
		t.Error("ExpectedTarget must equal the target amq saves for AmqWakeArgs")
	}
}

// A lock that cannot be fenced is never retired: no generation or no saved
// target must be an error, not an unfenced destructive call.
func TestWakeRetireFailsClosedWithoutFence(t *testing.T) {
	ctx := context.Background()
	if err := WakeRetire(ctx, "/nonexistent/amq", "/r", "bob", WakeState{Status: "missing"}); err != nil {
		t.Errorf("missing lock must be a no-op: %v", err)
	}
	tgt := WakeTarget{InjectVia: "/a", InjectArgs: []string{"inject"}, RetryUntil: "injected"}
	if err := WakeRetire(ctx, "/nonexistent/amq", "/r", "bob", WakeState{Status: "valid", HasTarget: true, Target: tgt}); err == nil {
		t.Error("valid lock without generation must be refused before calling amq")
	}
	if err := WakeRetire(ctx, "/nonexistent/amq", "/r", "bob", WakeState{Status: "stale", Generation: "abc"}); err == nil {
		t.Error("lock without saved target must be refused before calling amq")
	}
}

// Cleanup retires only what the record can prove it owns; anything it
// cannot identify is an error so the record survives as the credential.
func TestDecideOwnership(t *testing.T) {
	want := ExpectedTarget("/a", "bob", "w1:p1", "/r")
	rec := WakerRecord{Handle: "bob", PaneID: "w1:p1", Generation: "g1"}
	cases := []struct {
		name string
		st   WakeState
		rec  WakerRecord
		want Ownership
		err  bool
	}{
		{"parked record owns nothing", WakeState{Status: "valid", Generation: "g1", HasTarget: true, Target: want}, WakerRecord{Handle: "bob"}, OwnsNothing, false},
		{"missing lock", WakeState{Status: "missing"}, rec, OwnsNothing, false},
		{"owned", WakeState{Status: "valid", Generation: "g1", HasTarget: true, Target: want}, rec, OwnsLock, false},
		{"stale but owned", WakeState{Status: "stale", Generation: "g1", HasTarget: true, Target: want}, rec, OwnsLock, false},
		{"foreign generation", WakeState{Status: "valid", Generation: "g2", HasTarget: true, Target: want}, rec, ForeignLock, false},
		{"no generation", WakeState{Status: "valid", HasTarget: true, Target: want}, rec, OwnsNothing, true},
		{"no target", WakeState{Status: "valid", Generation: "g1"}, rec, OwnsNothing, true},
		{"same generation other target", WakeState{Status: "valid", Generation: "g1", HasTarget: true, Target: ExpectedTarget("/b", "bob", "w1:p1", "/r")}, rec, OwnsNothing, true},
	}
	for _, c := range cases {
		got, err := DecideOwnership(c.st, c.rec, want)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("%s: got %v err=%v, want %v err=%v", c.name, got, err, c.want, c.err)
		}
	}
}

func TestLiveHandlesExcludesParked(t *testing.T) {
	got := LiveHandles([]WakerRecord{{Handle: "a", Generation: "g"}, {Handle: "parked"}, {Handle: "b", Generation: "h"}})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("got %v", got)
	}
}
