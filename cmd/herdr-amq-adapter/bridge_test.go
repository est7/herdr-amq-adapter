package main

import (
	"testing"

	"github.com/est7/herdr-amq-adapter/internal/bridge"
)

// The tunnel argv must end option parsing before the target so a target
// can never be read as an ssh option.
func TestTunnelArgsTerminateOptions(t *testing.T) {
	args := tunnelArgs(bridge.Peer{Host: "heping", SSHTarget: "remote_heping", RendezvousPort: 18790})
	n := len(args)
	if args[n-2] != "--" || args[n-1] != "remote_heping" {
		t.Fatalf("argv tail %v", args[n-3:])
	}
	if args[0] != "-N" || args[n-4] != "-L" || args[n-3] != "127.0.0.1:18790:127.0.0.1:18790" {
		t.Fatalf("argv %v", args)
	}
}

// Topology changes are planned in a fixed safe order; the plan is pure so
// the ordering can be checked without ssh or sockets.
func TestPlanTopologyOrdersStopRebindStart(t *testing.T) {
	heping := bridge.Peer{Host: "heping", SSHTarget: "remote_heping", RendezvousPort: 18790}
	other := bridge.Peer{Host: "other", SSHTarget: "remote_other", RendezvousPort: 18790}
	running := map[string]bool{tunnelKey(heping): true}

	// unchanged: nothing to do
	plan := planTopology(running, 0, bridge.Env{Peers: []bridge.Peer{heping}})
	if len(plan.StopTunnels) != 0 || len(plan.StartTunnels) != 0 || plan.Rebind {
		t.Fatalf("unchanged: %+v", plan)
	}
	// there -> here on the same port: the tunnel holding 18790 must stop and
	// the listener must bind; no tunnel starts (peer port is now 0).
	plan = planTopology(running, 0, bridge.Env{Local: bridge.Local{RendezvousPort: 18790}, Peers: []bridge.Peer{{Host: "heping", SSHTarget: "remote_heping"}}})
	if len(plan.StopTunnels) != 1 || !plan.Rebind || plan.ServePort != 18790 || len(plan.StartTunnels) != 0 {
		t.Fatalf("there->here: %+v", plan)
	}
	// here -> there: listener goes away (ServePort 0), tunnel starts.
	plan = planTopology(nil, 18790, bridge.Env{Peers: []bridge.Peer{heping}})
	if len(plan.StopTunnels) != 0 || !plan.Rebind || plan.ServePort != 0 || len(plan.StartTunnels) != 1 {
		t.Fatalf("here->there: %+v", plan)
	}
	// peer replaced: old tunnel stops, new one starts, listener untouched.
	plan = planTopology(running, 0, bridge.Env{Peers: []bridge.Peer{other}})
	if len(plan.StopTunnels) != 1 || plan.Rebind || len(plan.StartTunnels) != 1 || plan.StartTunnels[0].Host != "other" {
		t.Fatalf("replace: %+v", plan)
	}
	// a peer without ssh target or port never gets a tunnel
	plan = planTopology(nil, 0, bridge.Env{Peers: []bridge.Peer{{Host: "mac"}}})
	if len(plan.StartTunnels) != 0 {
		t.Fatalf("dial-less peer got a tunnel: %+v", plan)
	}
}

func TestRemoteInstallAction(t *testing.T) {
	if a, err := remoteInstallAction("Darwin arm64\n", "Darwin arm64\n", "abc", "abc\n"); err != nil || a != "keep" {
		t.Errorf("same build: %s %v", a, err)
	}
	if a, err := remoteInstallAction("Darwin arm64\n", "Darwin arm64\n", "abc", "old\n"); err != nil || a != "install" {
		t.Errorf("old build: %s %v", a, err)
	}
	if a, err := remoteInstallAction("Darwin arm64\n", "Darwin arm64\n", "abc", ""); err != nil || a != "install" {
		t.Errorf("missing: %s %v", a, err)
	}
	if _, err := remoteInstallAction("Darwin arm64\n", "Linux x86_64\n", "abc", ""); err == nil {
		t.Error("other platform must be refused")
	}
}
