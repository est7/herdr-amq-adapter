package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/est7/herdr-amq-adapter/internal/adapter"
	"github.com/est7/herdr-amq-adapter/internal/bridge"
	"github.com/est7/herdr-amq-adapter/internal/rendezvous"
)

// topologyPlan is the ordered set of changes that moves the runner from
// one configuration to the next: tunnels to stop (they may hold the port a
// local rendezvous is about to bind), the listener port to serve (0 for
// none), tunnels to start last.
type topologyPlan struct {
	StopTunnels  []string
	ServePort    int
	StartTunnels []bridge.Peer
	Rebind       bool
}

func tunnelKey(p bridge.Peer) string {
	return p.Host + "|" + p.SSHTarget + "|" + fmt.Sprint(p.RendezvousPort)
}

// planTopology is pure so the ordering rules can be tested without
// processes: stop first, rebind second (bind-before-close is the
// executor's job), start last; unchanged tunnels are untouched.
func planTopology(runningTunnels map[string]bool, servingPort int, next bridge.Env) topologyPlan {
	want := map[string]bridge.Peer{}
	for _, p := range next.Peers {
		if p.SSHTarget != "" && p.RendezvousPort > 0 {
			want[tunnelKey(p)] = p
		}
	}
	var plan topologyPlan
	for key := range runningTunnels {
		if _, still := want[key]; !still {
			plan.StopTunnels = append(plan.StopTunnels, key)
		}
	}
	sort.Strings(plan.StopTunnels)
	plan.ServePort = next.Local.RendezvousPort
	plan.Rebind = next.Local.RendezvousPort != servingPort
	keys := make([]string, 0, len(want))
	for key := range want {
		if !runningTunnels[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		plan.StartTunnels = append(plan.StartTunnels, want[key])
	}
	return plan
}

// Cross-machine federation. Every machine keeps its own root; the plugin on
// each side runs `bridge run`, which forwards alias-mailbox mail into
// amq-bridge spools, pushes and polls the rendezvous, and (on the dialing
// side) keeps the SSH tunnel and the agent inventories in sync. Pairing is
// `peer add` on the machine that can SSH to the other; it drives the other
// side's `peer accept` over that same SSH connection.

const (
	tickEvery      = 3 * time.Second
	inventoryEvery = 30 * time.Second
	sshTimeout     = 20 * time.Second
)

func runBridge(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: herdr-amq-adapter bridge run|ensure|status [--json]")
	}
	switch args[0] {
	case "run":
		return bridgeRun()
	case "ensure":
		return bridgeEnsure()
	case "status":
		return bridgeStatus(args[1:])
	}
	return fmt.Errorf("unknown bridge command %q", args[0])
}

// runnerStatus is what the runner publishes every tick to
// <state>/bridge/status.json so an operator can see progress, not just
// that a process holds the lock.
type runnerStatus struct {
	Version       string    `json:"version"`
	PID           int       `json:"pid"`
	StartedAt     time.Time `json:"started_at"`
	LastTick      time.Time `json:"last_tick"`
	LastInventory time.Time `json:"last_inventory,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	LastErrorAt   time.Time `json:"last_error_at,omitempty"`
	RendezvousURL string    `json:"rendezvous_url"`
	ServingPort   int       `json:"serving_port,omitempty"`
	Tunnels       []string  `json:"tunnels,omitempty"`
	Forwarded     int       `json:"forwarded_total"`
	Pushed        int       `json:"pushed_total"`
	Applied       int       `json:"applied_total"`
	Refused       int       `json:"refused_total"`
}

func statusPath(e env) string { return filepath.Join(e.stateDir, "bridge", "status.json") }

func writeStatus(e env, st runnerStatus) {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	tmp := fmt.Sprintf("%s.%d.tmp", statusPath(e), os.Getpid())
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, statusPath(e))
	}
}

func runPeer(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: herdr-amq-adapter peer add|accept|agents|aliases")
	}
	switch args[0] {
	case "add":
		return peerAdd(args[1:])
	case "accept":
		return peerAccept(args[1:])
	case "agents":
		return peerAgents()
	case "aliases":
		return peerAliases(args[1:])
	}
	return fmt.Errorf("unknown peer command %q", args[0])
}

// localAgents is this plugin's own inventory: the handles whose wakers it
// currently owns. Parked records (agent released) are excluded so a peer
// never routes mail to a mailbox nobody reads. It needs no Herdr access, so
// it also works over a bare SSH session.
func localAgents(e env) ([]string, error) {
	recs, err := e.store.List()
	if err != nil {
		return nil, err
	}
	out := adapter.LiveHandles(recs)
	sort.Strings(out)
	return out, nil
}

func bridgeEnv(e env) (bridge.Env, bool, error) {
	snap, err := bridge.LoadSnapshot(e.configDir)
	if err != nil || !snap.Configured {
		return bridge.Env{}, false, err
	}
	agents, err := localAgents(e)
	if err != nil {
		return bridge.Env{}, false, err
	}
	url := ""
	if snap.Local.RendezvousPort > 0 {
		url = fmt.Sprintf("http://127.0.0.1:%d", snap.Local.RendezvousPort)
	} else {
		for _, p := range snap.Peers {
			if p.RendezvousPort > 0 {
				url = fmt.Sprintf("http://127.0.0.1:%d", p.RendezvousPort)
				break
			}
		}
	}
	return bridge.Env{
		BridgeBin: e.bridgeBin, Root: e.root, StateDir: e.stateDir, Local: snap.Local, Peers: snap.Peers,
		LocalAgents: agents, RendezvousURL: url,
	}, true, nil
}

// reloadMarker is how `bridge ensure` asks a running instance to reload:
// it touches this file and the runner compares its mtime every tick. No
// pid is ever read or signalled.
func reloadMarker(e env) string { return filepath.Join(e.stateDir, "bridge", "reload") }

// bridgeRun is the long-lived federation process: single instance per
// state dir, rendezvous server when this host serves it, SSH tunnel and
// inventory sync for dialable peers, and a courier tick every few seconds.
func bridgeRun() error {
	e, err := loadEnv()
	if err != nil {
		return err
	}
	if e.bridgeBin == "" {
		return errors.New("amq-bridge not found on PATH (set AMQ_BRIDGE_BIN)")
	}
	lockPath := filepath.Join(e.stateDir, "bridge", "run.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Println("bridge run: another instance holds the lock; exiting")
		return nil
	}
	defer lock.Close()
	benv, ok, err := bridgeEnv(e)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("bridge is not configured; run `peer add` first")
	}
	// SIGTERM/SIGINT cancel the context, which kills the ssh tunnel and any
	// courier in flight; without this a stopped runner leaves an orphan
	// tunnel holding the loopback port.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	store, err := rendezvous.Open(filepath.Join(e.stateDir, "bridge", "rendezvous"))
	if err != nil {
		return err
	}
	var srv *http.Server
	servingPort := 0
	// serveRendezvous switches the listener to port (0 = none). The new
	// listener is bound before the old server is closed, so a failed bind
	// leaves the old service running and the caller keeps the old topology.
	serveRendezvous := func(port int) error {
		if port == servingPort {
			return nil
		}
		var next *http.Server
		if port != 0 {
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				return fmt.Errorf("rendezvous listen: %w", err)
			}
			next = &http.Server{Handler: store.Handler(), ReadHeaderTimeout: 10 * time.Second}
			go func(s *http.Server) { _ = s.Serve(ln) }(next)
			fmt.Printf("rendezvous serving on %s\n", ln.Addr())
		}
		if srv != nil {
			_ = srv.Close()
			fmt.Printf("rendezvous on port %d stopped\n", servingPort)
		}
		srv, servingPort = next, port
		return nil
	}
	if err := serveRendezvous(benv.Local.RendezvousPort); err != nil {
		return err
	}
	defer func() {
		if srv != nil {
			_ = srv.Close()
		}
	}()
	type tunnel struct {
		cancel context.CancelFunc
		done   chan struct{}
	}
	tunnels := map[string]tunnel{}
	running := func() map[string]bool {
		m := map[string]bool{}
		for k := range tunnels {
			m[k] = true
		}
		return m
	}
	startTunnel := func(p bridge.Peer) {
		tctx, tcancel := context.WithCancel(ctx)
		done := make(chan struct{})
		tunnels[tunnelKey(p)] = tunnel{cancel: tcancel, done: done}
		go func() { defer close(done); keepTunnel(tctx, p) }()
	}
	// applyTopology executes a plan in its order: obsolete tunnels are
	// cancelled and awaited (they may hold the port), the listener is
	// rebound bind-before-close, new tunnels start last. A failed bind keeps
	// the old listener, restores the old tunnels and reports; the next
	// reload retries.
	applyTopology := func(next bridge.Env) error {
		plan := planTopology(running(), servingPort, next)
		for _, key := range plan.StopTunnels {
			t := tunnels[key]
			t.cancel()
			<-t.done
			delete(tunnels, key)
			fmt.Printf("tunnel %s stopped\n", key)
		}
		if plan.Rebind {
			if err := serveRendezvous(plan.ServePort); err != nil {
				for _, p := range benv.Peers {
					if _, up := tunnels[tunnelKey(p)]; p.SSHTarget != "" && p.RendezvousPort > 0 && !up {
						startTunnel(p)
					}
				}
				return err
			}
		}
		for _, p := range plan.StartTunnels {
			startTunnel(p)
		}
		return nil
	}
	for _, p := range planTopology(nil, servingPort, benv).StartTunnels {
		startTunnel(p)
	}
	lastInventory := time.Time{}
	markerSeen := markerTime(reloadMarker(e))
	rs := runnerStatus{Version: version, PID: os.Getpid(), StartedAt: time.Now()}
	noteErr := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		fmt.Println(msg)
		rs.LastError, rs.LastErrorAt = msg, time.Now()
	}
	for {
		if now := markerTime(reloadMarker(e)); now.After(markerSeen) {
			markerSeen = now
			lastInventory = time.Time{}
			fmt.Println("bridge run: reload requested")
		}
		if time.Since(lastInventory) >= inventoryEvery {
			for _, p := range benv.Peers {
				if p.SSHTarget == "" {
					continue
				}
				if err := syncInventory(ctx, e, benv.Local, p); err != nil {
					noteErr("inventory %s: %v", p.Host, err)
				}
			}
			lastInventory = time.Now()
			rs.LastInventory = lastInventory
			fresh, ok, err := bridgeEnv(e)
			if err != nil || !ok {
				return fmt.Errorf("reload bridge config: %v", err)
			}
			if err := applyTopology(fresh); err != nil {
				noteErr("topology: %v (keeping previous)", err)
			} else {
				benv = fresh
			}
			if err := ensureAliasMailboxes(ctx, e, benv.Peers); err != nil {
				noteErr("alias mailboxes: %v", err)
			}
		}
		tctx, tcancel := context.WithTimeout(ctx, tickEvery*4)
		rep := bridge.Tick(tctx, benv)
		tcancel()
		for _, f := range rep.Forwarded {
			fmt.Printf("forwarded %s\n", f)
		}
		for _, r := range rep.Pushed {
			fmt.Printf("pushed %s transfer=%s\n", r.SourceMessageID, r.TransferID)
		}
		for _, r := range rep.Applied {
			fmt.Printf("applied %s -> %s\n", r.SourceMessageID, r.CommittedPath)
		}
		for _, r := range rep.Refused {
			kind := "uncertain (redelivered later)"
			if r.Conflict {
				kind = "conflict (operator action: clear it at the rendezvous)"
			}
			fmt.Printf("refused transfer=%s %s: %s\n", r.TransferID, kind, r.Reason)
		}
		for _, d := range rep.Diagnostics {
			fmt.Printf("diagnostic %s\n", d)
		}
		for _, err := range rep.Errors {
			noteErr("tick: %v", err)
		}
		rs.LastTick = time.Now()
		rs.RendezvousURL, rs.ServingPort = benv.RendezvousURL, servingPort
		rs.Tunnels = rs.Tunnels[:0]
		for key := range tunnels {
			rs.Tunnels = append(rs.Tunnels, key)
		}
		sort.Strings(rs.Tunnels)
		rs.Forwarded += len(rep.Forwarded)
		rs.Pushed += len(rep.Pushed)
		rs.Applied += len(rep.Applied)
		rs.Refused += len(rep.Refused)
		writeStatus(e, rs)
		select {
		case <-ctx.Done():
			fmt.Println("bridge run: stopping")
			return nil
		case <-time.After(tickEvery):
		}
	}
}

func markerTime(path string) time.Time {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// keepTunnel forwards the peer's rendezvous port to the same local loopback
// port for as long as the runner lives, restarting ssh with backoff.
func keepTunnel(ctx context.Context, p bridge.Peer) {
	backoff := time.Second
	for {
		if err := bridge.ValidSSHTarget(p.SSHTarget); err != nil {
			fmt.Printf("tunnel %s: %v\n", p.Host, err)
			return
		}
		cmd := exec.CommandContext(ctx, "ssh", tunnelArgs(p)...)
		var errb bytes.Buffer
		cmd.Stderr = &errb
		started := time.Now()
		err := cmd.Run()
		if ctx.Err() != nil {
			return
		}
		fmt.Printf("tunnel %s exited after %s: %v %s\n", p.Host, time.Since(started).Round(time.Second), err, strings.TrimSpace(errb.String()))
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// sshAdapter runs the peer's adapter over SSH. OpenSSH joins the remote
// command into one shell string, so every operand must satisfy a grammar
// with no whitespace or metacharacters: the adapter path is validated by
// ValidRemotePath, subcommand words are plugin constants, and values are
// validated at their source (host aliases by ValidHost, handles by amq's
// own grammar before they ever reach a record). Data travels on stdin.
// tunnelArgs is the pure argv for the rendezvous tunnel; options end at
// "--" so the target can never be read as one.
func tunnelArgs(p bridge.Peer) []string {
	return []string{"-N", "-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-o", "ConnectTimeout=10",
		"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", p.RendezvousPort, p.RendezvousPort), "--", p.SSHTarget}
}

func sshAdapter(ctx context.Context, p bridge.Peer, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, sshTimeout)
	defer cancel()
	remote := p.RemoteAdapter
	if remote == "" {
		remote = "~/.local/bin/herdr-amq-adapter"
	}
	if err := bridge.ValidRemotePath(remote); err != nil {
		return nil, err
	}
	if err := bridge.ValidSSHTarget(p.SSHTarget); err != nil {
		return nil, err
	}
	for _, a := range args {
		if strings.ContainsAny(a, " \t\n'\"`$;&|<>()*?[]{}\\") {
			return nil, fmt.Errorf("refusing remote argument %q", a)
		}
	}
	cmd := exec.CommandContext(ctx, "ssh", append([]string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "--", p.SSHTarget, remote}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ssh %s %s: %w: %s", p.SSHTarget, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// syncInventory exchanges agent lists with a dialable peer: pulls theirs
// (`peer agents`) into our peer file and pushes ours (`peer aliases`) so
// they provision alias mailboxes for us. Only the dialing side needs SSH.
func syncInventory(ctx context.Context, e env, local bridge.Local, p bridge.Peer) error {
	out, err := sshAdapter(ctx, p, nil, "peer", "agents")
	if err != nil {
		return err
	}
	var theirs []string
	if err := json.Unmarshal(out, &theirs); err != nil {
		return fmt.Errorf("decode peer agents: %w", err)
	}
	ours, err := localAgents(e)
	if err != nil {
		return err
	}
	if _, err := sshAdapter(ctx, p, nil, "peer", "aliases", "--host", local.Host, "--agents", strings.Join(ours, ",")); err != nil {
		return err
	}
	_, err = bridge.UpdatePeer(e.configDir, p.Host, func(cur *bridge.Peer) { cur.Agents = theirs; cur.InventoryUpdatedAt = time.Now() })
	return err
}

// ensureAliasMailboxes provisions <host>-<agent> for every known remote
// agent so local agents can `amq send --to` it.
func ensureAliasMailboxes(ctx context.Context, e env, peers []bridge.Peer) error {
	for _, p := range peers {
		for _, a := range p.Agents {
			if err := adapter.EnsureMailbox(ctx, e.amq, e.root, bridge.AliasHandle(p.Host, a)); err != nil {
				return err
			}
		}
	}
	return nil
}

// bridgeEnsure starts `bridge run` detached when the bridge is configured.
// When an instance already runs it is asked to reload by touching the
// reload marker (never by signalling a pid), so a pairing change takes
// effect on the next tick and no second instance is spawned only to exit
// on the lock.
func bridgeEnsure() error {
	e, err := loadEnv()
	if err != nil {
		return err
	}
	if _, ok, err := bridge.LoadLocal(e.configDir); err != nil || !ok {
		return err
	}
	running, err := bridgeRunningChecked(e)
	if err != nil {
		return err
	}
	if running {
		if err := touch(reloadMarker(e)); err != nil {
			return err
		}
		fmt.Println("bridge run: reload requested")
		return nil
	}
	if err := os.MkdirAll(e.logs, 0o755); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(e.logs, "bridge.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	fmt.Fprintf(logf, "\n=== %s bridge ensure\n", time.Now().Format(time.RFC3339))
	cmd := exec.Command(e.self, "bridge", "run")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Env = append(os.Environ(), "HERDR_PLUGIN_STATE_DIR="+e.stateDir, "HERDR_PLUGIN_CONFIG_DIR="+e.configDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	fmt.Printf("bridge run started pid=%d log=%s\n", pid, filepath.Join(e.logs, "bridge.log"))
	return nil
}

// bridgeStatus is the operator view: configuration, the runner's last
// published progress, a live probe of the rendezvous, and what is waiting
// in spools and quarantine. --json prints the same as one object.
func bridgeStatus(args []string) error {
	fs := flag.NewFlagSet("bridge status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := loadEnv()
	if err != nil {
		return err
	}
	snap, err := bridge.LoadSnapshot(e.configDir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	view := inspectBridge(ctx, e, snap)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(view); err != nil {
			return err
		}
	} else {
		fmt.Print(renderBridge(view))
	}
	if len(view.Errors) > 0 {
		return errors.New(strings.Join(view.Errors, "; "))
	}
	return nil
}

func ageOf(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return time.Since(t).Round(time.Second).String() + " ago"
}

// peerAdd pairs this machine with one it can SSH to. It installs the
// adapter binary there when missing, exchanges bridge identities, records
// both sides' peer files, and starts `bridge run` on both.
func peerAdd(args []string) error {
	fs := flag.NewFlagSet("peer add", flag.ContinueOnError)
	target := fs.String("ssh", "", "OpenSSH host alias of the peer (required)")
	label := fs.String("label", "", "Herdr saved-machine label of the peer (default: --host)")
	host := fs.String("host", "", "bridge host alias for the peer (default: --label)")
	me := fs.String("me", "", "bridge host alias for this machine (default: short hostname)")
	rendezvous := fs.String("rendezvous", "there", "where the rendezvous runs: there (peer serves it) or here")
	port := fs.Int("port", 18790, "loopback port for the rendezvous")
	remote := fs.String("remote-adapter", "~/.local/bin/herdr-amq-adapter", "adapter binary path on the peer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *target == "" {
		return errors.New("--ssh is required")
	}
	if err := bridge.ValidSSHTarget(*target); err != nil {
		return err
	}
	if *host == "" {
		*host = *label
	}
	if *host == "" {
		return errors.New("--host or --label is required")
	}
	if *label == "" {
		*label = *host
	}
	if *me == "" {
		hn, _ := os.Hostname()
		*me = strings.ToLower(strings.SplitN(hn, ".", 2)[0])
	}
	if err := bridge.ValidHost(*me); err != nil {
		return err
	}
	if err := bridge.ValidRemotePath(*remote); err != nil {
		return err
	}

	if err := bridge.ValidHost(*host); err != nil {
		return err
	}
	if *rendezvous != "there" && *rendezvous != "here" {
		return errors.New("--rendezvous must be there or here")
	}
	if err := bridge.ValidPort(*port); err != nil {
		return err
	}
	e, err := loadEnv()
	if err != nil {
		return err
	}
	if e.bridgeBin == "" {
		return errors.New("amq-bridge not found on PATH (set AMQ_BRIDGE_BIN)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := bridge.EnsureIdentity(ctx, e.bridgeBin, e.root, *me); err != nil {
		return err
	}
	pub, err := bridge.PublicKey(ctx, e.bridgeBin, e.root)
	if err != nil {
		return err
	}
	peer := bridge.Peer{Host: *host, Label: *label, SSHTarget: *target, RemoteAdapter: *remote}
	if err := installRemoteAdapter(ctx, e, peer); err != nil {
		return err
	}
	acceptArgs := []string{"peer", "accept", "--host", *host, "--peer", *me, "--port", fmt.Sprint(*port)}
	if *rendezvous == "there" {
		acceptArgs = append(acceptArgs, "--rendezvous-here")
		peer.RendezvousPort = *port
	}
	out, err := sshAdapter(ctx, peer, []byte(pub), acceptArgs...)
	if err != nil {
		return err
	}
	var accepted struct {
		Host   string   `json:"host"`
		Public string   `json:"public"`
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(out, &accepted); err != nil {
		return fmt.Errorf("decode peer accept: %w: %s", err, out)
	}
	if accepted.Host != *host {
		return fmt.Errorf("peer identifies as %q, expected %q", accepted.Host, *host)
	}
	if err := bridge.Trust(e.root, *host, accepted.Public); err != nil {
		return err
	}
	local, peer, err := bridge.UpdatePairing(e.configDir, *host, func(l *bridge.Local, p *bridge.Peer) error {
		l.Host = *me
		p.Label, p.SSHTarget, p.RemoteAdapter = peer.Label, peer.SSHTarget, peer.RemoteAdapter
		p.Agents = accepted.Agents
		return bridge.ApplyRendezvousRole(l, p, *rendezvous == "here", *port)
	})
	if err != nil {
		return err
	}
	if err := syncInventory(ctx, e, local, peer); err != nil {
		return err
	}
	if err := ensureAliasMailboxes(ctx, e, []bridge.Peer{peer}); err != nil {
		return err
	}
	if _, err := sshAdapter(ctx, peer, nil, "bridge", "ensure"); err != nil {
		return err
	}
	if err := bridgeEnsure(); err != nil {
		return err
	}
	fmt.Printf("paired with %s (%s): remote agents %s reachable as %s-<agent>; this host is %s\n",
		*host, *target, strings.Join(accepted.Agents, ","), *host, *me)
	return nil
}

// remoteInstallAction decides what installRemoteAdapter does from one
// probe of the peer: its `uname -sm`, and the adapter's `version` output
// there (empty when the binary is missing or not executable).
//
//	"keep"    the peer runs this build already
//	"install" nothing there, or a different build: copy this binary over
//	error     the peer is another OS/arch (this binary cannot run there)
func remoteInstallAction(localPlatform, remotePlatform, localVersion, remoteVersion string) (string, error) {
	if strings.TrimSpace(remotePlatform) != strings.TrimSpace(localPlatform) {
		return "", fmt.Errorf("peer is %q, this build is for %q; install the plugin there from source and pass --remote-adapter", strings.TrimSpace(remotePlatform), strings.TrimSpace(localPlatform))
	}
	if remoteVersion != "" && strings.TrimSpace(remoteVersion) == strings.TrimSpace(localVersion) {
		return "keep", nil
	}
	return "install", nil
}

// installRemoteAdapter puts this exact build at the peer's adapter path:
// missing or different build gets a copy (written to a temp name and
// renamed, so a running peer process is never half-overwritten); an SSH
// failure is reported as such, never mistaken for "missing".
func installRemoteAdapter(ctx context.Context, e env, p bridge.Peer) error {
	if err := bridge.ValidRemotePath(p.RemoteAdapter); err != nil {
		return err
	}
	ssh := func(args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "ssh", append([]string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "--", p.SSHTarget}, args...)...).CombinedOutput()
	}
	platform, err := ssh("uname", "-sm")
	if err != nil {
		return fmt.Errorf("ssh %s: %w: %s", p.SSHTarget, err, strings.TrimSpace(string(platform)))
	}
	local, _ := exec.Command("uname", "-sm").Output()
	remoteVersion := ""
	if out, err := ssh(p.RemoteAdapter, "version"); err == nil {
		remoteVersion = string(out)
	}
	action, err := remoteInstallAction(string(local), string(platform), version, remoteVersion)
	if err != nil {
		return err
	}
	if action == "keep" {
		return nil
	}
	dir := filepath.Dir(p.RemoteAdapter)
	if out, err := ssh("mkdir", "-p", dir); err != nil {
		return fmt.Errorf("ssh mkdir: %w: %s", err, out)
	}
	tmp := p.RemoteAdapter + ".new"
	if out, err := exec.CommandContext(ctx, "scp", "-q", "--", e.self, p.SSHTarget+":"+tmp).CombinedOutput(); err != nil {
		return fmt.Errorf("scp adapter: %w: %s", err, out)
	}
	if out, err := ssh("chmod", "755", tmp); err != nil {
		return fmt.Errorf("ssh chmod: %w: %s", err, out)
	}
	if out, err := ssh("mv", "-f", tmp, p.RemoteAdapter); err != nil {
		return fmt.Errorf("ssh mv: %w: %s", err, out)
	}
	fmt.Printf("installed adapter %s at %s:%s (was %q)\n", version, p.SSHTarget, p.RemoteAdapter, strings.TrimSpace(remoteVersion))
	return nil
}

// peerAccept runs on the peer, driven over SSH by peerAdd: it becomes
// bridge host --host, trusts the caller (--peer, public record on stdin),
// records the caller as a peer, and answers with its own public record and
// agent inventory.
func peerAccept(args []string) error {
	fs := flag.NewFlagSet("peer accept", flag.ContinueOnError)
	host := fs.String("host", "", "this machine's bridge host alias")
	peerHost := fs.String("peer", "", "the caller's bridge host alias")
	port := fs.Int("port", 18790, "rendezvous loopback port")
	here := fs.Bool("rendezvous-here", false, "serve the rendezvous on this machine")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *host == "" || *peerHost == "" {
		return errors.New("--host and --peer are required")
	}
	if err := bridge.ValidPort(*port); err != nil {
		return err
	}
	e, err := loadEnv()
	if err != nil {
		return err
	}
	if e.bridgeBin == "" {
		return errors.New("amq-bridge not found on this machine (install it to ~/.local/bin)")
	}
	record, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := bridge.EnsureIdentity(ctx, e.bridgeBin, e.root, *host); err != nil {
		return err
	}
	if err := bridge.Trust(e.root, *peerHost, string(record)); err != nil {
		return err
	}
	if _, _, err := bridge.UpdatePairing(e.configDir, *peerHost, func(l *bridge.Local, p *bridge.Peer) error {
		l.Host = *host
		return bridge.ApplyRendezvousRole(l, p, *here, *port)
	}); err != nil {
		return err
	}
	pub, err := bridge.PublicKey(ctx, e.bridgeBin, e.root)
	if err != nil {
		return err
	}
	agents, err := localAgents(e)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"host": *host, "public": pub, "agents": agents})
}

func peerAgents() error {
	e, err := loadEnv()
	if err != nil {
		return err
	}
	agents, err := localAgents(e)
	if err != nil {
		return err
	}
	if agents == nil {
		agents = []string{}
	}
	return json.NewEncoder(os.Stdout).Encode(agents)
}

// peerAliases records a peer's agent inventory and provisions the alias
// mailboxes for it; the dialing side calls this over SSH.
func peerAliases(args []string) error {
	fs := flag.NewFlagSet("peer aliases", flag.ContinueOnError)
	host := fs.String("host", "", "peer bridge host alias")
	agents := fs.String("agents", "", "comma-separated live agent handles at the peer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *host == "" {
		return errors.New("--host is required")
	}
	e, err := loadEnv()
	if err != nil {
		return err
	}
	if _, ok, err := bridge.LoadPeer(e.configDir, *host); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("unknown peer %q; pair first", *host)
	}
	var list []string
	for _, a := range strings.Split(*agents, ",") {
		if a = strings.TrimSpace(a); a != "" {
			list = append(list, a)
		}
	}
	p, err := bridge.UpdatePeer(e.configDir, *host, func(cur *bridge.Peer) { cur.Agents = list; cur.InventoryUpdatedAt = time.Now() })
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return ensureAliasMailboxes(ctx, e, []bridge.Peer{p})
}

// bridgeRunning reports whether an instance holds the run lock.
func bridgeRunningChecked(e env) (bool, error) {
	lock, err := os.OpenFile(filepath.Join(e.stateDir, "bridge", "run.lock"), os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read bridge lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return true, nil
		}
		return false, fmt.Errorf("probe bridge lock: %w", err)
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return false, nil
}

func touch(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	now := time.Now()
	if err := os.WriteFile(path, []byte(now.Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Chtimes(path, now, now)
}
