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
		// wire form as herdr 0.9.1 actually emits it (snake_case EventKind)
		{"detected-wire", `{"event":"pane_agent_detected","data":{"pane_id":"w1:p2","workspace_id":"w1","agent":"claude"}}`, ActionEnsure},
		{"exited-wire", `{"event":"pane_exited","data":{"pane_id":"w1:p2","workspace_id":"w1"}}`, ActionStop},
		{"closed-wire", `{"event":"pane_closed","data":{"pane_id":"w1:p2","workspace_id":"w1"}}`, ActionStop},
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

func TestChooseHandle(t *testing.T) {
	taken := map[string]bool{"claude": true, "claude-2": true}
	cases := []struct {
		a    AgentInfo
		want string
	}{
		{AgentInfo{Name: str("reviewer"), Agent: str("claude")}, "reviewer"}, // live name wins
		{AgentInfo{Agent: str("codex")}, "codex"},
		{AgentInfo{Agent: str("claude")}, "claude-3"},
		{AgentInfo{Agent: str("Antigravity CLI")}, "antigravity-cli"},
		{AgentInfo{}, "agent"},
	}
	for _, c := range cases {
		if got := ChooseHandle(c.a, taken); got != c.want {
			t.Errorf("%+v: got %q want %q", c.a, got, c.want)
		}
	}
}

func TestAgentsWith(t *testing.T) {
	in := []byte(`{"version":1,"created_utc":"x","agents":["codex"]}`)
	got, changed, err := AgentsWith(in, "claude")
	if err != nil || !changed || !reflect.DeepEqual(got, []string{"claude", "codex"}) {
		t.Fatalf("got %v changed=%v err=%v", got, changed, err)
	}
	if _, changed, _ := AgentsWith(in, "codex"); changed {
		t.Error("existing handle must not report a change")
	}
}

func TestDecideMove(t *testing.T) {
	raw := `{"event":"pane_moved","data":{"previous_pane_id":"w8:p7","previous_workspace_id":"w8","previous_tab_id":"w8:t2","pane":{"pane_id":"wB:p2","workspace_id":"wB"}}}`
	ev, err := ParseEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	act := Decide(ev)
	if act.Kind != ActionMove || act.PaneID != "wB:p2" || act.PreviousPaneID != "w8:p7" {
		t.Errorf("got %+v", act)
	}
}

func TestIdentityRenderAndNotice(t *testing.T) {
	id := Identity{PaneID: "w1:p2", Handle: "claude", Root: "/tmp/it's here"}
	want := "export AM_ROOT='/tmp/it'\\''s here'\nexport AM_ME='claude'\nexport HERDR_AMQ_PANE='w1:p2'\n"
	if got := id.Render(); got != want {
		t.Errorf("render:\n%q\nwant\n%q", got, want)
	}
	if p := IdentityPath("/cfg", "w1:p2"); p != "/cfg/panes/w1_p2.env" {
		t.Errorf("path %q", p)
	}
	n := Notice("AMQ doorbell run amq drain --include-body then act on it\n", id, "/cfg/panes/w1_p2.env")
	if n != "AMQ doorbell run amq drain --include-body then act on it (you are claude in Herdr; first: source '/cfg/panes/w1_p2.env')" {
		t.Errorf("notice %q", n)
	}
}

func TestPlan(t *testing.T) {
	live := []AgentInfo{
		{PaneID: "w1:p1", Name: str("reviewer"), Agent: str("claude")},
		{PaneID: "w1:p2", Agent: str("codex")}, // unnamed but live: wanted (named at start)
		{PaneID: "w1:p3", Name: str("impl"), Agent: str("codex")},
		{PaneID: "w1:p4", Name: str("qa"), Agent: str("claude")},
		{PaneID: "w1:p5", Agent: str("codex")}, // unnamed, no record: start
	}
	wakers := []WakerRecord{
		{PaneID: "w1:p1", Handle: "reviewer", PID: 100}, // alive, matches: keep
		{PaneID: "w1:p2", Handle: "codex", PID: 101},    // alive, unnamed agent: keep (name unknown, no mismatch)
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
	if want := []string{"w1:p3", "w1:p4", "w1:p9"}; !reflect.DeepEqual(stopped, want) {
		t.Errorf("stop: got %v want %v", stopped, want)
	}
	if want := []string{"w1:p3", "w1:p4", "w1:p5"}; !reflect.DeepEqual(started, want) {
		t.Errorf("start: got %v want %v", started, want)
	}
}

func TestAmqWakeArgsContract(t *testing.T) {
	got := AmqWakeArgs("/opt/adapter", "reviewer", "w1:p1", "/state/amq-root")
	want := []string{"wake", "--root", "/state/amq-root", "--me", "reviewer", "--inject-via", "/opt/adapter",
		"--inject-arg", "inject", "--inject-arg", "w1:p1", "--inject-arg", "reviewer", "--inject-arg", "/state/amq-root",
		"--retry-until", "injected", "--interrupt-cmd", "none", "--no-self-upgrade"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}
