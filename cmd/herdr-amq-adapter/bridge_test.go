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
