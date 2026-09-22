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
		{"released", `{"event":"pane.agent_detected","data":{"pane_id":"w1:p2","workspace_id":"w1","released":true,"final_status":"done"}}`, ActionPark},
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
	// working is deliverable: the agent queues the notice into its turn.
	for _, st := range []string{"idle", "done", "working", "unknown", ""} {
		if _, ready := GateOnStatus(st); !ready {
			t.Errorf("status %q must be ready", st)
		}
	}
	out, ready := GateOnStatus("blocked")
	if ready || out.Progress != ProgressDeferred || out.ExitCode() != 1 {
		t.Errorf("blocked: got ready=%v %+v; want deferred exit 1", ready, out)
	}
}

func TestChooseHandle(t *testing.T) {
	taken := map[string]bool{"claude": true, "claude-2": true}
	cases := []struct {
		a         AgentInfo
		preferred string
		want      string
	}{
		{AgentInfo{Name: str("reviewer"), Agent: str("claude")}, "", "reviewer"},    // live name wins
		{AgentInfo{Name: str("reviewer"), Agent: str("claude")}, "old", "reviewer"}, // even over a preference
		{AgentInfo{Agent: str("codex")}, "", "codex"},
		{AgentInfo{Agent: str("claude")}, "", "claude-3"},
		{AgentInfo{Agent: str("Antigravity CLI")}, "", "antigravity-cli"},
		{AgentInfo{}, "", "agent"},
		{AgentInfo{Agent: str("claude")}, "claude-7", "claude-7"}, // restarted agent keeps its old handle
		{AgentInfo{Agent: str("claude")}, "claude-2", "claude-3"}, // unless another live agent took it
	}
	for _, c := range cases {
		if got := ChooseHandle(c.a, taken, c.preferred); got != c.want {
			t.Errorf("%+v preferred=%q: got %q want %q", c.a, c.preferred, got, c.want)
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
		{PaneID: "w1:p1", Handle: "reviewer", PID: 100, Generation: "g"}, // current, matches: keep
		{PaneID: "w1:p2", Handle: "codex", PID: 101, Generation: "g"},    // current, unnamed agent: keep (name unknown, no mismatch)
		{PaneID: "w1:p3", Handle: "impl", PID: 102, Generation: "g"},     // waker not current: re-adopt (ensure keeps the record's handle)
		{PaneID: "w1:p4", Handle: "qa-old", PID: 103, Generation: "g"},   // renamed: re-adopt
		{PaneID: "w1:p9", Handle: "gone", PID: 104, Generation: "g"},     // pane vanished: retire
	}
	current := func(w WakerRecord) bool { return w.PID != 102 }
	plan := Plan(live, wakers, current)

	var stopped []string
	for _, w := range plan.Stop {
		stopped = append(stopped, w.PaneID)
	}
	var started []string
	for _, a := range plan.Start {
		started = append(started, a.PaneID)
	}
	if want := []string{"w1:p9"}; !reflect.DeepEqual(stopped, want) {
		t.Errorf("stop: got %v want %v", stopped, want)
	}
	if want := []string{"w1:p3", "w1:p4", "w1:p5"}; !reflect.DeepEqual(started, want) {
		t.Errorf("start: got %v want %v", started, want)
	}
}

// A parked record (pid 0, no generation) for a live pane is re-adopted
// rather than retired, and a live record that is not current (for example
// after a plugin update moved the --inject-via binary) is re-adopted too.
func TestPlanParkedAndStale(t *testing.T) {
	live := []AgentInfo{
		{PaneID: "w1:p1", Name: str("claude"), Agent: str("claude")},
		{PaneID: "w1:p2", Agent: str("codex")},
	}
	wakers := []WakerRecord{
		{PaneID: "w1:p1", Handle: "claude", PID: 100, Generation: "g"},
		{PaneID: "w1:p2", Handle: "codex", PID: 0},
	}
	plan := Plan(live, wakers, func(w WakerRecord) bool { return false })
	if len(plan.Stop) != 0 {
		t.Errorf("stop: got %v want none", plan.Stop)
	}
	var started []string
	for _, a := range plan.Start {
		started = append(started, a.PaneID)
	}
	if want := []string{"w1:p1", "w1:p2"}; !reflect.DeepEqual(started, want) {
		t.Errorf("start: got %v want %v", started, want)
	}
}

// After `herdr pane move` the record is keyed by the new pane while the
// waker's argv still names the old one; ArgvPane is what identity checks
// must use.
func TestArgvPaneSurvivesMove(t *testing.T) {
	w := WakerRecord{PaneID: "wB:p2", SpawnPaneID: "w8:p7", PaneAliases: []string{"w8:p7"}}
	if w.ArgvPane() != "w8:p7" {
		t.Errorf("argv pane %q", w.ArgvPane())
	}
	if (WakerRecord{PaneID: "w1:p1"}).ArgvPane() != "w1:p1" {
		t.Error("legacy record without spawn_pane_id must fall back to pane_id")
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

// A record with no generation is refreshed through ensure even when its
// waker is current, so it becomes provable and advertisable.
func TestPlanRefreshesRecordsWithoutGeneration(t *testing.T) {
	live := []AgentInfo{{PaneID: "w1:p1", Name: str("claude-2"), Agent: str("claude")}}
	wakers := []WakerRecord{{PaneID: "w1:p1", Handle: "claude-2", PID: 1}}
	plan := Plan(live, wakers, func(WakerRecord) bool { return true })
	if len(plan.Start) != 1 || len(plan.Stop) != 0 {
		t.Fatalf("plan %+v", plan)
	}
}
