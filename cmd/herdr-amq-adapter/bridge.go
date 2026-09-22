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
		return errors.New("usage: herdr-amq-adapter bridge run|ensure|status")
	}
	switch args[0] {
	case "run":
		return bridgeRun()
	case "ensure":
		return bridgeEnsure()
	case "status":
		return bridgeStatus()
	}
	return fmt.Errorf("unknown bridge command %q", args[0])
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
	local, ok, err := bridge.LoadLocal(e.configDir)
	if err != nil || !ok {
		return bridge.Env{}, false, err
	}
	peers, err := bridge.ListPeers(e.configDir)
	if err != nil {
		return bridge.Env{}, false, err
	}
	agents, err := localAgents(e)
	if err != nil {
		return bridge.Env{}, false, err
	}
	url := ""
	if local.RendezvousPort > 0 {
		url = fmt.Sprintf("http://127.0.0.1:%d", local.RendezvousPort)
	} else {
		for _, p := range peers {
			if p.RendezvousPort > 0 {
				url = fmt.Sprintf("http://127.0.0.1:%d", p.RendezvousPort)
				break
			}
		}
	}
	return bridge.Env{
		BridgeBin: e.bridgeBin, Root: e.root, StateDir: e.stateDir, Local: local, Peers: peers,
		LocalAgents: agents, RendezvousURL: url,
	}, true, nil
}

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
	if benv.Local.RendezvousPort > 0 {
		store, err := rendezvous.Open(filepath.Join(e.stateDir, "bridge", "rendezvous"))
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", benv.Local.RendezvousPort))
		if err != nil {
			return fmt.Errorf("rendezvous listen: %w", err)
		}
		srv := &http.Server{Handler: store.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		defer srv.Close()
		fmt.Printf("rendezvous serving on %s\n", ln.Addr())
	}
	tunnels := map[string]context.CancelFunc{}
	reconcileTunnels := func(peers []bridge.Peer) {
		want := map[string]bridge.Peer{}
		for _, p := range peers {
			if p.SSHTarget != "" && p.RendezvousPort > 0 {
				want[p.Host+"|"+p.SSHTarget+"|"+fmt.Sprint(p.RendezvousPort)] = p
			}
		}
		for key, cancel := range tunnels {
			if _, still := want[key]; !still {
				cancel()
				delete(tunnels, key)
				fmt.Printf("tunnel %s stopped\n", key)
			}
		}
		for key, p := range want {
			if _, running := tunnels[key]; running {
				continue
			}
			tctx, tcancel := context.WithCancel(ctx)
			tunnels[key] = tcancel
			go keepTunnel(tctx, p)
		}
	}
	reconcileTunnels(benv.Peers)
	lastInventory := time.Time{}
	for {
		if time.Since(lastInventory) >= inventoryEvery {
			for _, p := range benv.Peers {
				if p.SSHTarget == "" {
					continue
				}
				if err := syncInventory(ctx, e, benv.Local, p); err != nil {
					fmt.Printf("inventory %s: %v\n", p.Host, err)
				}
			}
			lastInventory = time.Now()
			fresh, ok, err := bridgeEnv(e)
			if err != nil || !ok {
				return fmt.Errorf("reload bridge config: %v", err)
			}
			if fresh.Local.RendezvousPort != benv.Local.RendezvousPort {
				return fmt.Errorf("rendezvous port changed (%d -> %d); restart bridge run", benv.Local.RendezvousPort, fresh.Local.RendezvousPort)
			}
			benv = fresh
			reconcileTunnels(benv.Peers)
			if err := ensureAliasMailboxes(ctx, e, benv.Peers); err != nil {
				fmt.Printf("alias mailboxes: %v\n", err)
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
		for _, err := range rep.Errors {
			fmt.Printf("tick: %v\n", err)
		}
		select {
		case <-ctx.Done():
			fmt.Println("bridge run: stopping")
			return nil
		case <-time.After(tickEvery):
		}
	}
}

// keepTunnel forwards the peer's rendezvous port to the same local loopback
// port for as long as the runner lives, restarting ssh with backoff.
func keepTunnel(ctx context.Context, p bridge.Peer) {
	backoff := time.Second
	for {
		cmd := exec.CommandContext(ctx, "ssh", "-N", "-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=yes",
			"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-o", "ConnectTimeout=10",
			"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", p.RendezvousPort, p.RendezvousPort), p.SSHTarget)
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
	_, err = bridge.UpdatePeer(e.configDir, p.Host, func(cur *bridge.Peer) { cur.Agents = theirs })
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

// bridgeEnsure starts `bridge run` detached when the bridge is configured;
// a second instance exits on the run lock, so this is safe to call from
// every reconcile.
func bridgeEnsure() error {
	e, err := loadEnv()
	if err != nil {
		return err
	}
	if _, ok, err := bridge.LoadLocal(e.configDir); err != nil || !ok {
		return err
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

func bridgeStatus() error {
	e, err := loadEnv()
	if err != nil {
		return err
	}
	local, ok, err := bridge.LoadLocal(e.configDir)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("bridge: not configured")
		return nil
	}
	fmt.Printf("host: %s rendezvous_port_here: %d amq-bridge: %s\n", local.Host, local.RendezvousPort, e.bridgeBin)
	peers, err := bridge.ListPeers(e.configDir)
	if err != nil {
		return err
	}
	for _, p := range peers {
		fmt.Printf("peer %s label=%s ssh=%s rendezvous_port=%d agents=%s\n", p.Host, p.Label, p.SSHTarget, p.RendezvousPort, strings.Join(p.Agents, ","))
	}
	lock, err := os.OpenFile(filepath.Join(e.stateDir, "bridge", "run.lock"), os.O_RDWR, 0)
	if err == nil {
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			fmt.Println("bridge run: running")
		} else {
			fmt.Println("bridge run: not running")
		}
	}
	return nil
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
	e, err := loadEnv()
	if err != nil {
		return err
	}
	if e.bridgeBin == "" {
		return errors.New("amq-bridge not found on PATH (set AMQ_BRIDGE_BIN)")
	}
	if existing, ok, err := bridge.LoadLocal(e.configDir); err != nil {
		return err
	} else if ok && existing.Host != *me {
		return fmt.Errorf("this machine is already bridge host %q; pass --me %s", existing.Host, existing.Host)
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
	local, err := bridge.UpdateLocal(e.configDir, func(l *bridge.Local) {
		l.Host = *me
		if *rendezvous == "here" {
			l.RendezvousPort = *port
		}
	})
	if err != nil {
		return err
	}
	peer, err = bridge.UpdatePeer(e.configDir, *host, func(p *bridge.Peer) {
		p.Label, p.SSHTarget, p.RemoteAdapter = peer.Label, peer.SSHTarget, peer.RemoteAdapter
		p.RendezvousPort = peer.RendezvousPort
		p.Agents = accepted.Agents
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

// installRemoteAdapter copies this binary to the peer when the remote path
// has none. Same OS/arch is assumed for v1; a mismatch fails at first use.
func installRemoteAdapter(ctx context.Context, e env, p bridge.Peer) error {
	if err := bridge.ValidRemotePath(p.RemoteAdapter); err != nil {
		return err
	}
	probe := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "--", p.SSHTarget, "test", "-x", p.RemoteAdapter)
	if probe.Run() == nil {
		return nil
	}
	dir := filepath.Dir(p.RemoteAdapter)
	if out, err := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "--", p.SSHTarget, "mkdir", "-p", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("ssh mkdir: %w: %s", err, out)
	}
	if out, err := exec.CommandContext(ctx, "scp", "-q", "--", e.self, p.SSHTarget+":"+p.RemoteAdapter).CombinedOutput(); err != nil {
		return fmt.Errorf("scp adapter: %w: %s", err, out)
	}
	fmt.Printf("installed adapter at %s:%s\n", p.SSHTarget, p.RemoteAdapter)
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
	if existing, ok, err := bridge.LoadLocal(e.configDir); err != nil {
		return err
	} else if ok && existing.Host != *host {
		return fmt.Errorf("this machine is already bridge host %q, not %q", existing.Host, *host)
	}
	if _, err := bridge.UpdateLocal(e.configDir, func(l *bridge.Local) {
		l.Host = *host
		if *here {
			l.RendezvousPort = *port
		}
	}); err != nil {
		return err
	}
	if _, err := bridge.UpdatePeer(e.configDir, *peerHost, func(p *bridge.Peer) {
		if !*here {
			p.RendezvousPort = *port
		} else {
			p.RendezvousPort = 0
		}
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
	p, err := bridge.UpdatePeer(e.configDir, *host, func(cur *bridge.Peer) { cur.Agents = list })
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return ensureAliasMailboxes(ctx, e, []bridge.Peer{p})
}
