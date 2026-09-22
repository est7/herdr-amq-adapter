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
)

var hostRe = regexp.MustCompile(`^[a-z0-9_][a-z0-9_-]{0,31}$`)

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
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
