package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/est7/herdr-amq-adapter/internal/bridge"
)

type peerView struct {
	InventoryUpdatedAt time.Time `json:"inventory_updated_at,omitempty"`
	Host               string    `json:"host"`
	Label              string    `json:"label"`
	SSH                string    `json:"ssh"`
	RendezvousPort     int       `json:"rendezvous_port"`
	Agents             []string  `json:"agents"`
}

// bridgeView is shared by the existing text/JSON command and the popup.
// Nil queue maps mean unavailable; empty maps mean successfully read and empty.
type bridgeView struct {
	Adapter      string         `json:"adapter"`
	Configured   bool           `json:"configured"`
	BridgeBin    string         `json:"amq_bridge"`
	Log          string         `json:"log"`
	Host         string         `json:"host,omitempty"`
	Port         int            `json:"rendezvous_port_here"`
	Peers        []peerView     `json:"peers"`
	Running      bool           `json:"running"`
	Runner       *runnerStatus  `json:"runner,omitempty"`
	Probe        string         `json:"rendezvous_probe"`
	Pending      map[string]int `json:"pending_spool"`
	AliasPending map[string]int `json:"pending_alias"`
	Quarantined  map[string]int `json:"quarantined"`
	Errors       []string       `json:"errors,omitempty"`
}

func inspectBridge(ctx context.Context, e env, snap bridge.Snapshot) bridgeView {
	v := bridgeView{Adapter: version, Configured: snap.Configured, BridgeBin: e.bridgeBin, Log: filepath.Join(e.logs, "bridge.log"), Host: snap.Local.Host, Port: snap.Local.RendezvousPort, Probe: "not configured"}
	if !snap.Configured {
		return v
	}
	for _, p := range snap.Peers {
		v.Peers = append(v.Peers, peerView{p.InventoryUpdatedAt, p.Host, p.Label, p.SSHTarget, p.RendezvousPort, p.Agents})
	}
	var err error
	v.Running, err = bridgeRunningChecked(e)
	if err != nil {
		v.Errors = append(v.Errors, err.Error())
	}
	var rs runnerStatus
	if b, err := os.ReadFile(statusPath(e)); err == nil {
		if err := json.Unmarshal(b, &rs); err != nil {
			v.Errors = append(v.Errors, "decode runner status: "+err.Error())
		} else {
			v.Runner = &rs
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		v.Errors = append(v.Errors, "read runner status: "+err.Error())
	}
	// Probe the configured topology, not a potentially stale runner's URL.
	port := snap.Local.RendezvousPort
	if port == 0 {
		for _, p := range snap.Peers {
			if p.RendezvousPort > 0 {
				port = p.RendezvousPort
				break
			}
		}
	}
	v.Probe = "no rendezvous configured"
	if port > 0 {
		client := &http.Client{Timeout: 3 * time.Second}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v1/transfers?dest_alias=%s&limit=1", port, url.QueryEscape(bridge.DestAlias(snap.Local.Host, "probe"))), nil)
		if err != nil {
			v.Errors = append(v.Errors, err.Error())
			return v
		}
		resp, err := client.Do(req)
		if err != nil {
			v.Probe = "unreachable: " + err.Error()
		} else {
			resp.Body.Close()
			v.Probe = resp.Status
		}
	}
	v.Pending, err = bridge.PendingSpools(e.root)
	if err != nil {
		v.Errors = append(v.Errors, err.Error())
	}
	v.AliasPending = map[string]int{}
	v.Quarantined = map[string]int{}
	for _, p := range snap.Peers {
		for _, a := range p.Agents {
			alias := bridge.AliasHandle(p.Host, a)
			for _, q := range []struct {
				box    string
				counts map[string]int
			}{{"new", v.AliasPending}, {"quarantine", v.Quarantined}} {
				n, err := bridge.CountMessages(filepath.Join(e.root, "agents", alias, "inbox", q.box))
				if err != nil {
					v.Errors = append(v.Errors, err.Error())
					q.counts[alias] = -1
				} else if n > 0 {
					q.counts[alias] = n
				}
			}
		}
	}
	return v
}

func renderBridge(v bridgeView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "adapter %s; host %s; amq-bridge: %s\n", v.Adapter, v.Host, v.BridgeBin)
	if !v.Configured {
		b.WriteString("bridge: not configured (run peer add)\n")
		return b.String()
	}
	fmt.Fprintf(&b, "runner: running=%v; rendezvous: %s\n", v.Running, v.Probe)
	if r := v.Runner; r != nil {
		fmt.Fprintf(&b, "runner build: %s; last tick: %s; inventory attempted: %s; forwarded=%d pushed=%d applied=%d refused=%d\n", r.Version, ageOf(r.LastTick), ageOf(r.LastInventory), r.Forwarded, r.Pushed, r.Applied, r.Refused)
		if r.LastError != "" {
			fmt.Fprintf(&b, "last error (%s): %s\n", ageOf(r.LastErrorAt), r.LastError)
		}
	}
	for _, p := range v.Peers {
		fmt.Fprintf(&b, "peer %s: %s (last known inventory)\n", p.Host, strings.Join(p.Agents, ", "))
	}
	for _, q := range []struct {
		name string
		m    map[string]int
	}{{"pending spool", v.Pending}, {"pending alias", v.AliasPending}, {"quarantined", v.Quarantined}} {
		if q.m == nil {
			fmt.Fprintf(&b, "%s: unavailable\n", q.name)
			continue
		}
		keys := make([]string, 0, len(q.m))
		for k := range q.m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) == 0 {
			fmt.Fprintf(&b, "%s: 0\n", q.name)
		}
		for _, k := range keys {
			fmt.Fprintf(&b, "%s %s: %d\n", q.name, k, q.m[k])
		}
	}
	for _, err := range v.Errors {
		fmt.Fprintf(&b, "ERROR: %s\n", err)
	}
	fmt.Fprintf(&b, "log: %s\n", v.Log)
	return b.String()
}
