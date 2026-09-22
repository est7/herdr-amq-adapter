package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

var hostRe = regexp.MustCompile(`^[a-z0-9_][a-z0-9_-]{0,31}$`)

// remotePathRe is the grammar for a path used inside an SSH remote command
// string; OpenSSH hands that string to the peer's shell, so no whitespace
// or metacharacters are ever accepted.
var remotePathRe = regexp.MustCompile(`^~?/[A-Za-z0-9_./-]+$|^~/[A-Za-z0-9_./-]+$`)

// ValidRemotePath accepts absolute or ~/ paths made of [A-Za-z0-9_./-].
func ValidRemotePath(p string) error {
	if !remotePathRe.MatchString(p) || strings.Contains(p, "..") {
		return fmt.Errorf("remote path %q must be absolute or ~/ and contain only [A-Za-z0-9_./-]", p)
	}
	return nil
}

// sshTargetRe: an OpenSSH destination (host alias, user@host, host:port
// forms are handled by ssh config); never something ssh could parse as an
// option, and no shell-significant characters.
var sshTargetRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@-]*$`)

// ValidSSHTarget accepts a plain OpenSSH destination.
func ValidSSHTarget(t string) error {
	if !sshTargetRe.MatchString(t) {
		return fmt.Errorf("ssh target %q must match %s", t, sshTargetRe)
	}
	return nil
}

// ValidPort accepts a TCP port for the rendezvous.
func ValidPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("rendezvous port %d must be in 1..65535", port)
	}
	return nil
}

// ApplyRendezvousRole sets both sides of the one-rendezvous invariant for
// a pairing: exactly one of Local.RendezvousPort / Peer.RendezvousPort is
// non-zero after any transition (here -> there or there -> here).
func ApplyRendezvousRole(l *Local, p *Peer, here bool, port int) error {
	if err := ValidPort(port); err != nil {
		return err
	}
	if here {
		l.RendezvousPort, p.RendezvousPort = port, 0
	} else {
		l.RendezvousPort, p.RendezvousPort = 0, port
	}
	return nil
}

// Snapshot is a consistent read of the whole bridge configuration.
type Snapshot struct {
	Local      Local
	Configured bool
	Peers      []Peer
}

// LoadSnapshot reads local config and every peer under the config lock, so
// a reader never sees a pairing half-written.
func LoadSnapshot(configDir string) (Snapshot, error) {
	unlock, err := lockConfig(configDir)
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	l, ok, err := LoadLocal(configDir)
	if err != nil {
		return Snapshot{}, err
	}
	peers, err := ListPeers(configDir)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Local: l, Configured: ok, Peers: peers}, nil
}

// UpdatePairing applies one coupled change to the local config and one
// peer under a single lock acquisition, writing the peer first and the
// local record last so a crash in between leaves the previous local role
// (the runner keeps serving what it served) rather than a half topology.
func UpdatePairing(configDir, peerHost string, fn func(*Local, *Peer) error) (Local, Peer, error) {
	unlock, err := lockConfig(configDir)
	if err != nil {
		return Local{}, Peer{}, err
	}
	defer unlock()
	l, existed, err := LoadLocal(configDir)
	if err != nil {
		return Local{}, Peer{}, err
	}
	p, _, err := LoadPeer(configDir, peerHost)
	if err != nil {
		return Local{}, Peer{}, err
	}
	p.Host = peerHost
	before := l.Host
	if err := fn(&l, &p); err != nil {
		return Local{}, Peer{}, err
	}
	if existed && before != "" && l.Host != before {
		return Local{}, Peer{}, fmt.Errorf("%w: is %q, update wanted %q", ErrHostImmutable, before, l.Host)
	}
	if p.RemoteAdapter != "" {
		if err := ValidRemotePath(p.RemoteAdapter); err != nil {
			return Local{}, Peer{}, err
		}
	}
	if p.SSHTarget != "" {
		if err := ValidSSHTarget(p.SSHTarget); err != nil {
			return Local{}, Peer{}, err
		}
	}
	if err := SavePeer(configDir, p); err != nil {
		return Local{}, Peer{}, err
	}
	return l, p, SaveLocal(configDir, l)
}

// ValidHost is amq-bridge's host alias grammar (also a valid Herdr name
// prefix, so <host>-<agent> stays a legal handle).
func ValidHost(h string) error {
	if !hostRe.MatchString(h) {
		return fmt.Errorf("host %q must match %s", h, hostRe)
	}
	return nil
}

// Local is this machine's bridge identity, stored in <configDir>/bridge.json.
type Local struct {
	Host string `json:"host"`
	// RendezvousPort is set on the host that serves the rendezvous; peers
	// tunnel to it. Zero means this host reaches a peer's rendezvous.
	RendezvousPort int `json:"rendezvous_port,omitempty"`
}

// Peer is one paired machine, stored in <configDir>/peers/<host>.json.
type Peer struct {
	Host string `json:"host"` // bridge host alias of the peer
	// Label is the Herdr saved-machine label, kept for the user; routing
	// never depends on it.
	Label string `json:"label,omitempty"`
	// SSHTarget is set on the side that can dial the peer (an OpenSSH host
	// alias). The dialing side also pushes agent inventories both ways.
	SSHTarget string `json:"ssh_target,omitempty"`
	// RemoteAdapter is the adapter binary path on the peer, used over SSH.
	RemoteAdapter string `json:"remote_adapter,omitempty"`
	// RendezvousPort is the loopback port the rendezvous listens on at the
	// peer; the dialing side forwards a local port to it. Zero when the
	// rendezvous is served here instead (see Local.RendezvousPort).
	RendezvousPort int `json:"rendezvous_port,omitempty"`
	// Agents is the last inventory of live agents at the peer; each gets an
	// alias mailbox here.
	Agents []string `json:"agents,omitempty"`
}

func localPath(configDir string) string   { return filepath.Join(configDir, "bridge.json") }
func peersDir(configDir string) string    { return filepath.Join(configDir, "peers") }
func peerPath(configDir, h string) string { return filepath.Join(peersDir(configDir), h+".json") }

func LoadLocal(configDir string) (Local, bool, error) {
	var l Local
	ok, err := readJSON(localPath(configDir), &l)
	return l, ok, err
}

func SaveLocal(configDir string, l Local) error {
	if err := ValidHost(l.Host); err != nil {
		return err
	}
	return writeJSON(localPath(configDir), l)
}

// ErrHostImmutable is returned when an update would rename this machine's
// bridge identity; the host names the key peers trust, so it can only be
// set once (pair again to change it).
var ErrHostImmutable = errors.New("bridge host identity is immutable once set")

// UpdateLocal applies fn to the current local config under the config
// lock, so concurrent writers (peer add, peer accept over SSH, the runner)
// never clobber each other's fields. The host immutability rule is
// checked inside the lock against what fn produced.
func UpdateLocal(configDir string, fn func(*Local)) (Local, error) {
	unlock, err := lockConfig(configDir)
	if err != nil {
		return Local{}, err
	}
	defer unlock()
	l, existed, err := LoadLocal(configDir)
	if err != nil {
		return Local{}, err
	}
	before := l.Host
	fn(&l)
	if existed && before != "" && l.Host != before {
		return Local{}, fmt.Errorf("%w: is %q, update wanted %q", ErrHostImmutable, before, l.Host)
	}
	return l, SaveLocal(configDir, l)
}

// UpdatePeer applies fn to the peer's current record under the config lock.
func UpdatePeer(configDir, host string, fn func(*Peer)) (Peer, error) {
	unlock, err := lockConfig(configDir)
	if err != nil {
		return Peer{}, err
	}
	defer unlock()
	p, _, err := LoadPeer(configDir, host)
	if err != nil {
		return Peer{}, err
	}
	p.Host = host
	fn(&p)
	if p.RemoteAdapter != "" {
		if err := ValidRemotePath(p.RemoteAdapter); err != nil {
			return Peer{}, err
		}
	}
	if p.SSHTarget != "" {
		if err := ValidSSHTarget(p.SSHTarget); err != nil {
			return Peer{}, err
		}
	}
	return p, SavePeer(configDir, p)
}

func lockConfig(configDir string) (func(), error) {
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(configDir, ".bridge.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func LoadPeer(configDir, host string) (Peer, bool, error) {
	var p Peer
	ok, err := readJSON(peerPath(configDir, host), &p)
	return p, ok, err
}

func SavePeer(configDir string, p Peer) error {
	if err := ValidHost(p.Host); err != nil {
		return err
	}
	sort.Strings(p.Agents)
	return writeJSON(peerPath(configDir, p.Host), p)
}

func ListPeers(configDir string) ([]Peer, error) {
	entries, err := os.ReadDir(peersDir(configDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Peer
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var p Peer
		if _, err := readJSON(filepath.Join(peersDir(configDir), e.Name()), &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

func readJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("decode %s: %w", path, err)
	}
	return true, nil
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
