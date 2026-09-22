package adapter

import (
	"reflect"
	"testing"
)

func str(s string) *string { return &s }

func TestDecide(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want ActionKind
	}{
		{"detected", `{"event":"pane.agent_detected","data":{"pane_id":"w1:p2","workspace_id":"w1","agent":"claude"}}`, ActionEnsure},
		{"released", `{"event":"pane.agent_detected","data":{"pane_id":"w1:p2","workspace_id":"w1","released":true,"final_status":"done"}}`, ActionStop},
		{"closed", `{"event":"pane.closed","data":{"pane_id":"w1:p2","workspace_id":"w1"}}`, ActionStop},
		{"exited", `{"event":"pane.exited","data":{"pane_id":"w1:p2","workspace_id":"w1"}}`, ActionStop},
		{"status", `{"event":"pane.agent_status_changed","data":{"pane_id":"w1:p2","workspace_id":"w1","agent_status":"idle"}}`, ActionNone},
	}
	for _, c := range cases {
		ev, err := ParseEvent(c.raw)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := Decide(ev); got.Kind != c.want || got.PaneID != "w1:p2" {
			t.Errorf("%s: got %+v want %v", c.name, got, c.want)
		}
	}
	if _, err := ParseEvent(""); err == nil {
		t.Error("empty event must be an error")
	}
	if _, err := ParseEvent(`{"event":"pane.closed","data":{}}`); err == nil {
		t.Error("missing pane_id must be an error")
	}
}

func TestClassifyPromptResult(t *testing.T) {
	cases := []struct {
		rc     int
		stderr string
		want   Progress
		code   string
		exit   int
	}{
		{0, "", ProgressAccepted, "", 0},
		// deferred MUST exit non-zero: amq treats deferred+exit0 as uncertain (terminal).
		{1, `{"error":{"code":"agent_blocked","message":"agent is blocked"},"id":"cli:agent:prompt"}`, ProgressDeferred, "agent_blocked", 1},
		{1, `{"error":{"code":"agent_not_found","message":"agent target w1:p9 not found"},"id":"cli:agent:prompt"}`, ProgressFailed, "agent_not_found", 1},
		{1, `{"error":{"code":"agent_prompt_stalled","message":"no activity"},"id":"x"}`, ProgressFailed, "agent_prompt_stalled", 1},
		{2, "unknown option: --bogus", ProgressFailed, "exit_2", 1},
	}
	for _, c := range cases {
		got := ClassifyPromptResult(c.rc, c.stderr)
		if got.Progress != c.want || got.Code != c.code || got.ExitCode() != c.exit {
			t.Errorf("rc=%d stderr=%q: got %+v want progress=%s code=%s exit=%d", c.rc, c.stderr, got, c.want, c.code, c.exit)
		}
	}
}

func TestGateOnStatus(t *testing.T) {
	for _, st := range []string{"idle", "done", "unknown", ""} {
		if _, ready := GateOnStatus(st); !ready {
			t.Errorf("status %q must be ready", st)
		}
	}
	for _, st := range []string{"working", "blocked"} {
		out, ready := GateOnStatus(st)
		if ready || out.Progress != ProgressDeferred || out.ExitCode() != 1 {
			t.Errorf("status %q: got ready=%v %+v; want deferred exit 1", st, ready, out)
		}
	}
}

func TestHandleRequiresName(t *testing.T) {
	if _, ok := Handle(AgentInfo{PaneID: "w1:p1", Agent: str("claude")}); ok {
		t.Error("unnamed agent must not be adopted")
	}
	if h, ok := Handle(AgentInfo{PaneID: "w1:p1", Agent: str("claude"), Name: str("reviewer")}); !ok || h != "reviewer" {
		t.Errorf("named agent handle: got %q %v", h, ok)
	}
}

func TestPlan(t *testing.T) {
	live := []AgentInfo{
		{PaneID: "w1:p1", Name: str("reviewer"), Agent: str("claude")},
		{PaneID: "w1:p2", Agent: str("codex")}, // unnamed: never wanted
		{PaneID: "w1:p3", Name: str("impl"), Agent: str("codex")},
		{PaneID: "w1:p4", Name: str("qa"), Agent: str("claude")},
	}
	wakers := []WakerRecord{
		{PaneID: "w1:p1", Handle: "reviewer", PID: 100}, // alive, matches: keep
		{PaneID: "w1:p2", Handle: "old", PID: 101},      // pane now unnamed: stop
		{PaneID: "w1:p3", Handle: "impl", PID: 102},     // dead: forget + restart
		{PaneID: "w1:p4", Handle: "qa-old", PID: 103},   // renamed: stop + restart
		{PaneID: "w1:p9", Handle: "gone", PID: 104},     // pane vanished: stop
	}
	alive := func(pid int) bool { return pid != 102 }
	plan := Plan(live, wakers, alive)

	var stopped []string
	for _, w := range plan.Stop {
		stopped = append(stopped, w.PaneID)
	}
	var started []string
	for _, a := range plan.Start {
		started = append(started, a.PaneID)
	}
	if want := []string{"w1:p2", "w1:p3", "w1:p4", "w1:p9"}; !reflect.DeepEqual(stopped, want) {
		t.Errorf("stop: got %v want %v", stopped, want)
	}
	if want := []string{"w1:p3", "w1:p4"}; !reflect.DeepEqual(started, want) {
		t.Errorf("start: got %v want %v", started, want)
	}
}

func TestAmqWakeArgsContract(t *testing.T) {
	got := AmqWakeArgs("/opt/adapter", "reviewer", "w1:p1")
	want := []string{"wake", "--me", "reviewer", "--inject-via", "/opt/adapter",
		"--inject-arg", "inject", "--inject-arg", "w1:p1",
		"--retry-until", "injected", "--interrupt-cmd", "none", "--no-self-upgrade"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}
