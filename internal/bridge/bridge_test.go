package bridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const sample = "---json\n{\n  \"schema\": 1,\n  \"id\": \"2026-09-22T06-08-16.891Z_pid28972_b53cc11f\",\n  \"from\": \"claude\",\n  \"to\": [\n    \"heping-codex\"\n  ],\n  \"thread\": \"p2p/claude__heping-codex\",\n  \"subject\": \"x-host ping\",\n  \"created\": \"2026-09-22T06:08:16.891122Z\"\n}\n---\nping from mac\n"

func TestReaddressKeepsEverythingButFromTo(t *testing.T) {
	out, err := Readdress([]byte(sample), "mac-claude", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "---json\n") || !strings.HasSuffix(string(out), "\n---\nping from mac\n") {
		t.Fatalf("shape lost:\n%s", out)
	}
	m := frontMatter.FindSubmatch(out)
	var h map[string]any
	if err := json.Unmarshal(m[1], &h); err != nil {
		t.Fatal(err)
	}
	if h["from"] != "mac-claude" || !reflect.DeepEqual(h["to"], []any{"codex"}) {
		t.Errorf("from/to: %v %v", h["from"], h["to"])
	}
	for _, k := range []string{"id", "thread", "subject", "created"} {
		if h[k] == "" || h[k] == nil {
			t.Errorf("%s dropped", k)
		}
	}
	if h["id"] != "2026-09-22T06-08-16.891Z_pid28972_b53cc11f" || h["thread"] != "p2p/claude__heping-codex" {
		t.Errorf("id/thread changed: %v %v", h["id"], h["thread"])
	}
	if _, err := Readdress([]byte("hello"), "a", "b"); err == nil {
		t.Error("non-message must be rejected")
	}
	id, from, err := headerIDFrom([]byte(sample))
	if err != nil || id != "2026-09-22T06-08-16.891Z_pid28972_b53cc11f" || from != "claude" {
		t.Errorf("headerIDFrom: %q %q %v", id, from, err)
	}
}

// The argv contract with amq-bridge 0.80.1: the receive alias must be in
// --allow-dest too, and --once bounds the cycle.
func TestCourierArgs(t *testing.T) {
	got := CourierArgs(CourierSpec{
		Root: "/r", RendezvousURL: "http://127.0.0.1:1", LocalHost: "mac", LocalAgent: "claude",
		SourceHandle: "mac-claude", DestAliases: []string{"heping/codex", "heping/pi"}, PeerHosts: []string{"heping"}, Mode: "poll",
	})
	want := []string{"--root", "/r", "--rendezvous", "http://127.0.0.1:1", "--source-host", "mac", "--source-handle", "mac-claude",
		"--dest-alias", "heping/codex", "--receive-alias", "mac/claude", "--allow-dest", "heping/codex,heping/pi,mac/claude",
		"--allow-source-host", "heping", "--mode", "poll", "--once"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestNaming(t *testing.T) {
	if AliasHandle("heping", "codex") != "heping-codex" || DestAlias("heping", "codex") != "heping/codex" {
		t.Error("naming")
	}
	if ValidHost("Heping") == nil || ValidHost("a b") == nil || ValidHost("heping") != nil {
		t.Error("host grammar")
	}
}

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok, _ := LoadLocal(dir); ok {
		t.Fatal("fresh dir must have no local config")
	}
	if err := SaveLocal(dir, Local{Host: "mac"}); err != nil {
		t.Fatal(err)
	}
	if err := SavePeer(dir, Peer{Host: "heping", SSHTarget: "remote_heping", RendezvousPort: 18790, Agents: []string{"pi", "codex"}}); err != nil {
		t.Fatal(err)
	}
	l, ok, err := LoadLocal(dir)
	if err != nil || !ok || l.Host != "mac" {
		t.Fatalf("local %+v %v %v", l, ok, err)
	}
	peers, err := ListPeers(dir)
	if err != nil || len(peers) != 1 || peers[0].Host != "heping" || !reflect.DeepEqual(peers[0].Agents, []string{"codex", "pi"}) {
		t.Fatalf("peers %+v %v", peers, err)
	}
	if err := SavePeer(dir, Peer{Host: "Bad Host"}); err == nil {
		t.Error("invalid host must be rejected")
	}
}

func TestValidRemotePath(t *testing.T) {
	for _, ok := range []string{"~/.local/bin/herdr-amq-adapter", "/Users/ada/herdr-amq-adapter/bin/x", "~/x_y.z-1"} {
		if err := ValidRemotePath(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"bin/x", "~/a b", "/tmp/x;rm -rf ~", "~/$(id)", "/a/../b", "~/x`y`", "~/x\ny", ""} {
		if err := ValidRemotePath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A fake amq-bridge whose enqueue writes the spool file amq-bridge would;
// one malformed message must not starve the rest of the alias mailbox.
func TestTickIsolatesMessagesAndQuarantinesGarbage(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	fake := filepath.Join(dir, "fake-amq-bridge")
	script := `#!/bin/sh
# enqueue --config CFG --dest-alias DEST ; stdin = message
if [ "$1" = "enqueue" ]; then
  cfg=$3; msg=$(cat)
  root=$(sed -E 's/.*"root":"([^"]*)".*/\1/' "$cfg"); src=$(sed -E 's/.*"source_handle":"([^"]*)".*/\1/' "$cfg")
  id=$(printf '%s' "$msg" | sed -nE 's/^  "id": "([^"]*)",?$/\1/p' | head -1)
  mkdir -p "$root/bridge/outbox/$src/new"; printf '%s' "$msg" > "$root/bridge/outbox/$src/new/$id.md"; exit 0
fi
# courier cycles: print nothing, succeed
exit 0
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	newDir := filepath.Join(root, "agents", "heping-codex", "inbox", "new")
	if err := os.MkdirAll(newDir, 0o700); err != nil {
		t.Fatal(err)
	}
	good := strings.Replace(sample, "2026-09-22T06-08-16.891Z_pid28972_b53cc11f", "id-good", 1)
	os.WriteFile(filepath.Join(newDir, "0-garbage.md"), []byte("not a message"), 0o600)
	os.WriteFile(filepath.Join(newDir, "1-good.md"), []byte(good), 0o600)
	env := Env{BridgeBin: fake, Root: root, StateDir: filepath.Join(dir, "state"), Local: Local{Host: "mac"},
		Peers: []Peer{{Host: "heping", Agents: []string{"codex"}}}, LocalAgents: []string{"claude"}, RendezvousURL: "http://127.0.0.1:1"}
	rep := Tick(context.Background(), env)
	if len(rep.Forwarded) != 1 || !strings.HasPrefix(rep.Forwarded[0], "claude -> heping/codex id-good") {
		t.Fatalf("forwarded %v errors %v", rep.Forwarded, rep.Errors)
	}
	if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0].Error(), "quarantined") {
		t.Fatalf("errors %v", rep.Errors)
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "heping-codex", "inbox", "quarantine", "0-garbage.md")); err != nil {
		t.Error("garbage not quarantined")
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "heping-codex", "inbox", "cur", "1-good.md")); err != nil {
		t.Error("good message not moved to cur")
	}
	spool := filepath.Join(root, "bridge", "outbox", "mac-claude", "new", "id-good.md")
	b, err := os.ReadFile(spool)
	if err != nil {
		t.Fatalf("spool file missing: %v", err)
	}
	if !strings.Contains(string(b), `"from": "mac-claude"`) || !strings.Contains(string(b), `"codex"`) {
		t.Errorf("spooled message not readdressed:\n%s", b)
	}
	// second tick: nothing new, spool already holds the id -> no duplicate
	rep = Tick(context.Background(), env)
	if len(rep.Forwarded) != 0 || len(rep.Errors) != 0 {
		t.Errorf("second tick: %v %v", rep.Forwarded, rep.Errors)
	}
}

func TestSpoolsWithWorkSurfacesIOErrors(t *testing.T) {
	root := t.TempDir()
	spool := filepath.Join(root, "bridge", "outbox", "mac-claude", "new")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(spool, "m.md"), []byte("x"), 0o600)
	got, err := spoolsWithWork(root)
	if err != nil || len(got) != 1 || got[0] != "mac-claude" {
		t.Fatalf("got %v %v", got, err)
	}
	if err := os.Chmod(spool, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(spool, 0o700) })
	if _, err := spoolsWithWork(root); err == nil {
		t.Error("unreadable spool must be an error, not empty")
	}
}
