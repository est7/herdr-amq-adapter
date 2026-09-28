package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	claudeRule   = "──────────────────────────────────────────"
	claudeEmpty  = "some output\n" + claudeRule + "\n❯ \x1b[2mTry \"fix lint\"\x1b[0m\n" + claudeRule + "\n"
	claudeDraft  = "some output\n" + claudeRule + "\n❯ half typed by the user\n" + claudeRule + "\n"
	claudeTrust  = "│ Quick safety check: Is this a project you created or one you trust? (Like your own code)\n│ \x1b[1m❯\x1b[0m 1. Yes, I trust this folder\n│   2. No, exit\n"
	agentJSON    = `{"result":{"agent":{"agent":"%KIND%","name":"claude","pane_id":"w1:p1","cwd":"/x","agent_status":"idle"}}}`
	notFoundJSON = `{"error":{"code":"agent_not_found","message":"agent target claude not found"}}`
)

// fakeHerdr answers `agent get`, `agent read` and `agent prompt`; each
// step's script can be overridden. A prompt that runs leaves a marker file,
// so a test can tell "refused before typing" from "typed".
type fakeHerdr struct {
	kind, screen          string
	get, read, promptStep string
}

func (f fakeHerdr) install(t *testing.T) (bin, typed string) {
	t.Helper()
	dir := t.TempDir()
	typed = filepath.Join(dir, "typed")
	screen := filepath.Join(dir, "screen")
	if err := os.WriteFile(screen, []byte(f.screen), 0o600); err != nil {
		t.Fatal(err)
	}
	orDefault := func(s, def string) string {
		if s != "" {
			return s
		}
		return def
	}
	get := orDefault(f.get, "echo '"+strings.ReplaceAll(agentJSON, "%KIND%", f.kind)+"'")
	read := orDefault(f.read, `[ "$4 $5 $6 $7" = "--source visible --format ansi" ] || exit 9; cat '`+screen+`'`)
	prompt := orDefault(f.promptStep, "exit 0")
	script := "#!/bin/sh\n[ \"$1\" = agent ] || exit 9\ncase \"$2\" in\n" +
		"get) " + get + ";;\n" +
		"read) " + read + ";;\n" +
		"prompt) touch '" + typed + "'; " + prompt + ";;\n" +
		"*) exit 9;;\nesac\n"
	bin = filepath.Join(dir, "herdr")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, typed
}

func TestDeliverChecksTheScreenBeforeTyping(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fake      fakeHerdr
		want      Progress
		code      string
		wantTyped bool
	}{
		{"empty box", fakeHerdr{kind: "claude", screen: claudeEmpty}, ProgressAccepted, "", true},
		{"draft in box", fakeHerdr{kind: "claude", screen: claudeDraft}, ProgressDeferred, "draft_in_box", false},
		{"trust screen", fakeHerdr{kind: "claude", screen: claudeTrust}, ProgressDeferred, "trust_screen", false},
		// A kind whose box this adapter cannot read is sent to as before.
		{"unknown kind", fakeHerdr{kind: "copilot", screen: "anything\n"}, ProgressAccepted, "", true},
		{"agent gone", fakeHerdr{kind: "claude", get: "echo '" + notFoundJSON + "' >&2; exit 1"}, ProgressFailed, "agent_not_found", false},
		{"server down", fakeHerdr{kind: "claude", get: `echo '{"error":{"code":"server_not_running","message":"down"}}' >&2; exit 1`}, ProgressDeferred, "server_not_running", false},
		{"screen unreadable", fakeHerdr{kind: "claude", read: `echo '{"error":{"code":"internal","message":"x"}}' >&2; exit 1`}, ProgressDeferred, "screen_unreadable", false},
		{"blocked at prompt", fakeHerdr{kind: "claude", screen: claudeEmpty, promptStep: `echo '{"error":{"code":"agent_blocked","message":"approval"}}' >&2; exit 1`}, ProgressDeferred, "agent_blocked", true},
		{"missing at prompt", fakeHerdr{kind: "claude", screen: claudeEmpty, promptStep: "echo '" + notFoundJSON + "' >&2; exit 1"}, ProgressFailed, "agent_not_found", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, typed := tc.fake.install(t)
			out, err := (Herdr{Bin: bin}).Deliver("claude", "doorbell", 2*time.Second)
			if err != nil || out.Progress != tc.want || out.Code != tc.code {
				t.Fatalf("got %+v %v, want %s %q", out, err, tc.want, tc.code)
			}
			if _, statErr := os.Stat(typed); (statErr == nil) != tc.wantTyped {
				t.Fatalf("prompt ran = %v, want %v", statErr == nil, tc.wantTyped)
			}
		})
	}
}

// Looking at the pane types nothing, so running out of time there is safe
// to retry; running out of time in `agent prompt` may have typed and is not.
func TestDeliverTimeoutBeforeAndAfterTyping(t *testing.T) {
	bin, typed := fakeHerdr{kind: "claude", screen: claudeEmpty, read: "sleep 3"}.install(t)
	out, _ := (Herdr{Bin: bin}).Deliver("claude", "doorbell", 1500*time.Millisecond)
	if out.Progress != ProgressDeferred || out.Code != "timeout" {
		t.Fatalf("look timeout: got %+v", out)
	}
	if _, err := os.Stat(typed); err == nil {
		t.Fatal("prompt ran after the look timed out")
	}
	bin, _ = fakeHerdr{kind: "claude", screen: claudeEmpty, promptStep: "sleep 3"}.install(t)
	out, _ = (Herdr{Bin: bin}).Deliver("claude", "doorbell", 1500*time.Millisecond)
	if out.Progress != ProgressFailed || out.Code != "timeout" {
		t.Fatalf("prompt timeout: got %+v", out)
	}
}
