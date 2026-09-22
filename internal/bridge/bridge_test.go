package bridge

import (
	"encoding/json"
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
