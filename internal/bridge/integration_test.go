package bridge

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/est7/herdr-amq-adapter/internal/rendezvous"
)

// Two roots on one machine, the real amq and amq-bridge binaries, and the
// plugin's own rendezvous behind httptest: the full data plane as it runs
// between machines, minus SSH. Covers signed enqueue/push/apply, thread and
// address preservation, a reply through the symmetric alias, idempotent
// replay, and the crash window between enqueue and the alias-mailbox move.
func TestTwoHostsExchangeOverRealBridge(t *testing.T) {
	amq, err := exec.LookPath("amq")
	if err != nil {
		t.Skip("amq not on PATH")
	}
	bridgeBin, err := exec.LookPath("amq-bridge")
	if err != nil {
		if p := filepath.Join(os.Getenv("HOME"), ".local", "bin", "amq-bridge"); fileExists(p) {
			bridgeBin = p
		} else {
			t.Skip("amq-bridge not installed")
		}
	}
	ctx := context.Background()
	dir := t.TempDir()
	store, err := rendezvous.Open(filepath.Join(dir, "rv"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(store.Handler())
	defer srv.Close()

	type host struct {
		name  string
		root  string
		state string
		agent string
		peer  string
		env   Env
	}
	mk := func(name, agent, peer, peerAgent string) *host {
		h := &host{name: name, root: filepath.Join(dir, name, "root"), state: filepath.Join(dir, name, "state"), agent: agent, peer: peer}
		run(t, amq, "--no-update-check", "init", "--root", h.root, "--agents", agent+","+AliasHandle(peer, peerAgent))
		if err := EnsureIdentity(ctx, bridgeBin, h.root, name); err != nil {
			t.Fatal(err)
		}
		h.env = Env{BridgeBin: bridgeBin, Root: h.root, StateDir: h.state, Local: Local{Host: name},
			Peers: []Peer{{Host: peer, Agents: []string{peerAgent}}}, LocalAgents: []string{agent}, RendezvousURL: srv.URL}
		return h
	}
	mac := mk("mac", "claude", "heping", "codex")
	heping := mk("heping", "codex", "mac", "claude")
	for _, pair := range [][2]*host{{mac, heping}, {heping, mac}} {
		pub, err := PublicKey(ctx, bridgeBin, pair[0].root)
		if err != nil {
			t.Fatal(err)
		}
		if err := Trust(pair[1].root, pair[0].name, pub); err != nil {
			t.Fatal(err)
		}
	}

	// 1. claude on mac writes to the alias mailbox heping-codex.
	out := run(t, amq, "--no-update-check", "send", "--root", mac.root, "--me", "claude", "--to", "heping-codex",
		"--subject", "x-host", "--body", "ping from mac")
	sentID := regexp.MustCompile(`Sent (\S+) to`).FindStringSubmatch(out)[1]

	// 2. mac tick forwards + pushes; heping tick polls + applies.
	rep := Tick(ctx, mac.env)
	if len(rep.Errors) != 0 || len(rep.Forwarded) != 1 || len(rep.Pushed) != 1 || rep.Pushed[0].Stage != "transport_accepted" {
		t.Fatalf("mac tick: %+v", rep)
	}
	rep = Tick(ctx, heping.env)
	if len(rep.Errors) != 0 || len(rep.Applied) != 1 || rep.Applied[0].Stage != "destination_maildir_committed" {
		t.Fatalf("heping tick: %+v", rep)
	}
	applied := rep.Applied[0].CommittedPath
	if !strings.Contains(applied, "/agents/codex/inbox/new/xfer-mac-") {
		t.Fatalf("applied path %q", applied)
	}
	body, _ := os.ReadFile(applied)
	for _, want := range []string{`"from": "mac-claude"`, `"codex"`, `"id": "` + sentID + `"`, `"thread": "p2p/claude__heping-codex"`, "ping from mac"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("applied message lacks %s:\n%s", want, body)
		}
	}

	// 3. idempotent replay: nothing new on either side.
	if rep = Tick(ctx, mac.env); len(rep.Forwarded)+len(rep.Pushed)+len(rep.Errors) != 0 {
		t.Fatalf("mac replay tick did work: %+v", rep)
	}
	if rep = Tick(ctx, heping.env); len(rep.Applied)+len(rep.Errors) != 0 {
		t.Fatalf("heping replay tick did work: %+v", rep)
	}

	// 4. codex replies in the same thread through the symmetric alias.
	run(t, amq, "--no-update-check", "send", "--root", heping.root, "--me", "codex", "--to", "mac-claude",
		"--thread", "p2p/claude__heping-codex", "--body", "pong from heping")
	if rep = Tick(ctx, heping.env); len(rep.Errors) != 0 || len(rep.Pushed) != 1 {
		t.Fatalf("heping reply tick: %+v", rep)
	}
	if rep = Tick(ctx, mac.env); len(rep.Errors) != 0 || len(rep.Applied) != 1 {
		t.Fatalf("mac reply tick: %+v", rep)
	}
	reply, _ := os.ReadFile(rep.Applied[0].CommittedPath)
	if !strings.Contains(string(reply), `"from": "heping-codex"`) || !strings.Contains(string(reply), `"thread": "p2p/claude__heping-codex"`) {
		t.Errorf("reply not readdressed/threaded:\n%s", reply)
	}
	thread := run(t, amq, "--no-update-check", "thread", "--root", mac.root, "--me", "claude", "--id", "p2p/claude__heping-codex")
	if !strings.Contains(thread, "heping-codex") || !strings.Contains(thread, "x-host") {
		t.Errorf("thread view on mac:\n%s", thread)
	}

	// 5. crash window: enqueue succeeded but the alias message was not yet
	// moved to cur. Put the original back in new next to its spooled copy;
	// the next tick must not enqueue again (a second push would duplicate)
	// and must complete the move.
	aliasNew := filepath.Join(mac.root, "agents", "heping-codex", "inbox", "new")
	aliasCur := filepath.Join(mac.root, "agents", "heping-codex", "inbox", "cur")
	if err := os.Rename(filepath.Join(aliasCur, sentID+".md"), filepath.Join(aliasNew, sentID+".md")); err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(mac.root, "bridge", "outbox", "mac-claude", "sent", sentID+".md")) {
		t.Fatal("precondition: spool copy should be in sent after a successful push")
	}
	rep = Tick(ctx, mac.env)
	if len(rep.Errors) != 0 || len(rep.Pushed) != 0 || len(rep.Forwarded) != 1 {
		t.Fatalf("crash-window tick: %+v", rep)
	}
	if fileExists(filepath.Join(aliasNew, sentID+".md")) || !fileExists(filepath.Join(aliasCur, sentID+".md")) {
		t.Error("crash-window tick did not complete the new->cur move")
	}
	if fileExists(filepath.Join(mac.root, "bridge", "outbox", "mac-claude", "new", sentID+".md")) {
		t.Error("crash-window tick re-enqueued an already sent message")
	}
}

func run(t *testing.T, bin string, args ...string) string {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(bin), strings.Join(args, " "), err, out)
	}
	return string(out)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
