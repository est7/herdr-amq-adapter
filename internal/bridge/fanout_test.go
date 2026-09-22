package bridge

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/est7/herdr-amq-adapter/internal/rendezvous"
)

func TestBroadcastPreservesMessageAcrossDestinations(t *testing.T) {
	amq, err := exec.LookPath("amq")
	if err != nil {
		t.Skip(err)
	}
	bin := filepath.Join(os.Getenv("HOME"), ".local/bin/amq-bridge")
	if _, err := os.Stat(bin); err != nil {
		t.Skip(err)
	}
	d := t.TempDir()
	root, other := filepath.Join(d, "mac"), filepath.Join(d, "peer")
	run(t, amq, "--no-update-check", "init", "--root", root, "--agents", "claude,peer-codex,peer-pi")
	run(t, amq, "--no-update-check", "init", "--root", other, "--agents", "codex,pi")
	ctx := context.Background()
	for _, h := range []struct{ root, name string }{{root, "mac"}, {other, "peer"}} {
		if err := EnsureIdentity(ctx, bin, h.root, h.name); err != nil {
			t.Fatal(err)
		}
	}
	pub, err := PublicKey(ctx, bin, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := Trust(other, "mac", pub); err != nil {
		t.Fatal(err)
	}
	s, err := rendezvous.Open(filepath.Join(d, "relay"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	run(t, amq, "--no-update-check", "send", "--root", root, "--me", "claude", "--to", "peer-codex,peer-pi", "--thread", "broadcast-test", "--refs", "prior-message", "--body", "both must receive")
	originals, _ := filepath.Glob(filepath.Join(root, "agents/peer-codex/inbox/new/*.md"))
	original, err := os.ReadFile(originals[0])
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := headerIDFrom(original)
	if err != nil {
		t.Fatal(err)
	}
	e := Env{Root: root, StateDir: filepath.Join(d, "state"), BridgeBin: bin, Local: Local{Host: "mac"}, Peers: []Peer{{Host: "peer", Agents: []string{"codex", "pi"}}}, RendezvousURL: srv.URL}
	for i := 0; i < 3; i++ {
		if rep := Tick(ctx, e); len(rep.Errors) > 0 {
			t.Fatal(rep.Errors)
		}
		if i == 0 {
			// Crash window in upstream moveToSent: .md was archived and
			// unlinked from new, but the old destination sidecar remains.
			p := filepath.Join(root, "bridge/outbox/mac-claude/new", id+".dest")
			if err := os.WriteFile(p, []byte("peer/codex\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, agent := range []string{"codex", "pi"} {
		spec := CourierSpec{Root: other, RendezvousURL: srv.URL, LocalHost: "peer", LocalAgent: agent, SourceHandle: "peer-" + agent, PeerHosts: []string{"mac"}, Mode: "poll"}
		out, err := RunCourier(ctx, bin, spec)
		if err != nil || len(out.Receipts) != 1 {
			t.Fatalf("%s: want one delivery, got %+v, %v", agent, out, err)
		}
		b, err := os.ReadFile(out.Receipts[0].CommittedPath)
		if err != nil {
			t.Fatal(err)
		}
		gotID, _, err := headerIDFrom(b)
		if err != nil || gotID != id || !strings.Contains(string(b), `"thread": "broadcast-test"`) || !strings.Contains(string(b), `"prior-message"`) {
			t.Fatalf("identity changed: %s", b)
		}
	}
	// Replay the original alias copy after both destination-specific sends.
	if err := os.WriteFile(originals[0], original, 0600); err != nil {
		t.Fatal(err)
	}
	if rep := Tick(ctx, e); len(rep.Errors) > 0 || len(rep.Pushed) > 0 {
		t.Fatalf("replayed broadcast: %+v", rep)
	}
	// Upgrade recovery: destination-bound upstream receipts remain sufficient
	// even if the old installation never wrote our new forwarding marker.
	if err := os.Remove(forwardedPath(root, "mac-claude", id, "peer/codex")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(originals[0], original, 0600); err != nil {
		t.Fatal(err)
	}
	if rep := Tick(ctx, e); len(rep.Errors) > 0 || len(rep.Pushed) > 0 {
		t.Fatalf("receipt recovery: %+v", rep)
	}
}
