package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Env is everything one tick needs; the caller (the bridge runner) owns
// discovery and configuration.
type Env struct {
	BridgeBin     string
	Root          string
	StateDir      string
	Local         Local
	Peers         []Peer
	LocalAgents   []string // real local handles (the waker inventory)
	RendezvousURL string
}

// Report is what one tick did; the runner logs it.
type Report struct {
	Forwarded []string        // "<sender> -> <host>/<agent> <id>"
	Pushed    []CourierResult // transport_accepted receipts
	Applied   []CourierResult // destination_maildir_committed receipts
	Errors    []error
}

// Tick runs one bounded round: forward alias-mailbox mail into spools, push
// every non-empty spool, poll every local agent's receive alias. Each step
// is idempotent, so a crash anywhere is repaired by the next tick:
// forwarding skips a message whose id already sits in the spool (new or
// sent), the courier's transfer ledger dedupes pushes and applies.
func Tick(ctx context.Context, env Env) Report {
	var rep Report
	peerHosts := make([]string, 0, len(env.Peers))
	var destAliases []string
	for _, p := range env.Peers {
		peerHosts = append(peerHosts, p.Host)
		for _, a := range p.Agents {
			destAliases = append(destAliases, DestAlias(p.Host, a))
		}
	}
	if len(peerHosts) == 0 {
		return rep
	}
	for _, p := range env.Peers {
		for _, a := range p.Agents {
			if err := forwardAlias(ctx, env, p, a, &rep); err != nil {
				rep.Errors = append(rep.Errors, err)
			}
		}
	}
	// push: one cycle per spool that has work
	spools, err := spoolsWithWork(env.Root)
	if err != nil {
		rep.Errors = append(rep.Errors, err)
	}
	for _, src := range spools {
		sender, ok := strings.CutPrefix(src, env.Local.Host+"-")
		if !ok {
			continue
		}
		res, err := RunCourier(ctx, env.BridgeBin, CourierSpec{
			Root: env.Root, RendezvousURL: env.RendezvousURL, LocalHost: env.Local.Host, LocalAgent: sender,
			SourceHandle: src, DestAliases: destAliases, PeerHosts: peerHosts, Mode: "push",
		})
		if err != nil {
			rep.Errors = append(rep.Errors, err)
			continue
		}
		rep.Pushed = append(rep.Pushed, res...)
	}
	// poll: one cycle per local agent
	for _, agent := range env.LocalAgents {
		res, err := RunCourier(ctx, env.BridgeBin, CourierSpec{
			Root: env.Root, RendezvousURL: env.RendezvousURL, LocalHost: env.Local.Host, LocalAgent: agent,
			SourceHandle: AliasHandle(env.Local.Host, agent), DestAliases: destAliases, PeerHosts: peerHosts, Mode: "poll",
		})
		if err != nil {
			rep.Errors = append(rep.Errors, err)
			continue
		}
		rep.Applied = append(rep.Applied, res...)
	}
	return rep
}

// forwardAlias moves every new message in the alias mailbox for peer/agent
// into the sender's bridge spool. Messages are isolated from each other: a
// malformed one is quarantined, a transient failure (enqueue, I/O) is
// reported and retried next tick, and the rest of the mailbox proceeds.
func forwardAlias(ctx context.Context, env Env, peer Peer, agent string, rep *Report) error {
	alias := AliasHandle(peer.Host, agent)
	newDir := filepath.Join(env.Root, "agents", alias, "inbox", "new")
	entries, err := os.ReadDir(newDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dest := DestAlias(peer.Host, agent)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(newDir, e.Name())
		sender, id, err := forwardOne(ctx, env, alias, agent, dest, path)
		if err != nil {
			rep.Errors = append(rep.Errors, err)
			continue
		}
		rep.Forwarded = append(rep.Forwarded, fmt.Sprintf("%s -> %s %s", sender, dest, id))
	}
	return nil
}

// forwardOne spools a single alias-mailbox message. A file that is not an
// AMQ message can never be forwarded and is moved to inbox/quarantine so
// it stops being retried; every other failure leaves the file in new.
func forwardOne(ctx context.Context, env Env, alias, agent, dest, path string) (sender, id string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	id, sender, err = headerIDFrom(raw)
	if err != nil {
		return "", "", quarantine(env.Root, alias, path, err)
	}
	src := AliasHandle(env.Local.Host, sender)
	if !spooled(env.Root, src, id) {
		cfgPath, err := WriteEnqueueConfig(env.StateDir, EnqueueConfig{
			Root: env.Root, SourceHost: env.Local.Host, SourceHandle: src, AllowedDestAliases: []string{dest},
		})
		if err != nil {
			return "", "", err
		}
		msg, err := Readdress(raw, src, agent)
		if err != nil {
			return "", "", quarantine(env.Root, alias, path, err)
		}
		if err := Enqueue(ctx, env.BridgeBin, cfgPath, dest, msg); err != nil {
			return "", "", fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
	}
	curDir := filepath.Join(env.Root, "agents", alias, "inbox", "cur")
	if err := os.MkdirAll(curDir, 0o700); err != nil {
		return "", "", err
	}
	if err := os.Rename(path, filepath.Join(curDir, filepath.Base(path))); err != nil {
		return "", "", err
	}
	return sender, id, nil
}

func quarantine(root, alias, path string, cause error) error {
	qdir := filepath.Join(root, "agents", alias, "inbox", "quarantine")
	if err := os.MkdirAll(qdir, 0o700); err != nil {
		return err
	}
	if err := os.Rename(path, filepath.Join(qdir, filepath.Base(path))); err != nil {
		return err
	}
	return fmt.Errorf("%s quarantined: %w", filepath.Base(path), cause)
}

func headerIDFrom(raw []byte) (id, from string, err error) {
	m := frontMatter.FindSubmatch(raw)
	if m == nil {
		return "", "", errors.New("not an AMQ message file")
	}
	var h struct {
		ID   string `json:"id"`
		From string `json:"from"`
	}
	if err := json.Unmarshal(m[1], &h); err != nil {
		return "", "", err
	}
	if h.ID == "" || h.From == "" {
		return "", "", errors.New("message header lacks id or from")
	}
	return h.ID, h.From, nil
}

// spooled reports whether the sender's spool already holds this message,
// pending or sent; amq-bridge names spool files by message id.
func spooled(root, src, id string) bool {
	for _, box := range []string{"new", "sent"} {
		if _, err := os.Stat(filepath.Join(root, "bridge", "outbox", src, box, id+".md")); err == nil {
			return true
		}
	}
	return false
}

// spoolsWithWork lists spools holding pending transfers. I/O failures are
// returned, never mistaken for an empty spool: a queue that cannot be
// read must show up in the report, not stall silently.
func spoolsWithWork(root string) ([]string, error) {
	base := filepath.Join(root, "bridge", "outbox")
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read spools: %w", err)
	}
	var out []string
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(base, e.Name(), "new"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("read spool %s: %w", e.Name(), err))
			continue
		}
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".md") {
				out = append(out, e.Name())
				break
			}
		}
	}
	sort.Strings(out)
	return out, errors.Join(errs...)
}
