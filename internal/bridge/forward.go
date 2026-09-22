package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/est7/herdr-amq-adapter/internal/durable"
)

var errSpoolBusy = errors.New("another destination occupies this message's spool slot")

// OperatorError marks a condition the runner cannot resolve by retrying:
// the message stays where it is until a person acts. The runner reports
// these as a set that is printed when it changes, not on every tick.
type OperatorError struct {
	Key    string // stable identity of the stuck item (path)
	Action string // what to do
	Err    error
}

func (e *OperatorError) Error() string { return e.Err.Error() + "; " + e.Action }
func (e *OperatorError) Unwrap() error { return e.Err }

// A forwarding marker contains the exact enqueued bytes, keyed by source,
// message id and destination. It survives upstream removing the .dest sidecar.
func forwardedPath(root, src, id, dest string) string {
	return filepath.Join(root, "bridge", "forwarded", url.PathEscape(src), url.PathEscape(id)+"__"+url.PathEscape(dest)+".md")
}

// enqueueDestination is called by the single bridge runner. The upstream spool
// has one slot per source/id, so fan-out takes successive ticks without changing
// message ids. Completion is recorded durably before consuming the alias copy.
func enqueueDestination(ctx context.Context, env Env, src, id, dest string, msg []byte) error {
	marker := forwardedPath(env.Root, src, id, dest)
	if b, err := os.ReadFile(marker); err == nil {
		if !bytes.Equal(b, msg) {
			return fmt.Errorf("forwarded payload conflict for %s to %s", id, dest)
		}
		// Re-publish also repairs an earlier rename whose directory sync failed.
		return durable.WriteFile(marker, msg, 0600)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	base := filepath.Join(env.Root, "bridge", "outbox", src)
	newPath := filepath.Join(base, "new", id+".md")
	if b, err := os.ReadFile(newPath); err == nil {
		binding, err := os.ReadFile(strings.TrimSuffix(newPath, ".md") + ".dest")
		if err != nil {
			return fmt.Errorf("incomplete spool %s: %w", newPath, err)
		}
		if strings.TrimSpace(string(binding)) != dest {
			return errSpoolBusy
		}
		if !bytes.Equal(b, msg) {
			return fmt.Errorf("pending payload conflict for %s to %s", id, dest)
		}
		// Recovery after enqueue succeeded but recording completion did not.
		if err := syncSpool(newPath); err != nil {
			return err
		}
		return durable.WriteFile(marker, msg, 0600)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// A receipt also covers a pre-upgrade sent file with no plugin marker.
	accepted, err := transportAccepted(env.Root, env.Local.Host, src, id, dest, msg)
	if err != nil {
		return err
	}
	if accepted {
		return durable.WriteFile(marker, msg, 0600)
	}
	// Upstream compares existing sent bytes on push; move the previous
	// destination's archive aside before reusing the original id's slot.
	sent := filepath.Join(base, "sent", id+".md")
	if b, err := os.ReadFile(sent); err == nil {
		// Upstream archives first, removes new/.md second and .dest last.
		// A crash between those removes leaves a sidecar that would make the
		// next enqueue fail with EEXIST. Retire it only with receipt evidence.
		sidecar := strings.TrimSuffix(newPath, ".md") + ".dest"
		if binding, err := os.ReadFile(sidecar); err == nil {
			accepted, err := transportAccepted(env.Root, env.Local.Host, src, id, strings.TrimSpace(string(binding)), b)
			if err != nil {
				return err
			}
			if !accepted {
				return &OperatorError{Key: sidecar, Err: fmt.Errorf("orphan destination sidecar has no transport receipt: %s", sidecar),
					Action: "verify with the peer whether " + id + " reached " + strings.TrimSpace(string(binding)) + ", then remove the sidecar (delivered) or the sent copy (not delivered)"}
			}
			if err := os.Remove(sidecar); err != nil {
				return err
			}
			if err := durable.SyncDir(filepath.Dir(sidecar)); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		archive := filepath.Join(env.Root, "bridge", "forwarded", url.PathEscape(src), "archive", url.PathEscape(id)+fmt.Sprintf("__%x.md", sha256.Sum256(b)))
		if err := durable.WriteFile(archive, b, 0600); err != nil {
			return err
		}
		if err := os.Remove(sent); err != nil {
			return err
		}
		if err := durable.SyncDir(filepath.Dir(sent)); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cfg, err := WriteEnqueueConfig(env.StateDir, EnqueueConfig{Root: env.Root, SourceHost: env.Local.Host, SourceHandle: src, AllowedDestAliases: []string{dest}})
	if err != nil {
		return err
	}
	if err := Enqueue(ctx, env.BridgeBin, cfg, dest, msg); err != nil {
		return err
	}
	return durable.WriteFile(marker, msg, 0600)
}

func syncSpool(path string) error {
	for _, p := range []string{path, strings.TrimSuffix(path, ".md") + ".dest"} {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			return err
		}
	}
	return durable.SyncDir(filepath.Dir(path))
}

// Refuse incomplete enqueue output before the upstream courier can fall back
// to its default destination when .dest is missing. Complete pre-upgrade spools
// are adopted with the same durable forwarding evidence as new enqueues.
func prepareSpool(root, src string) error {
	dir := filepath.Join(root, "bridge", "outbox", src, "new")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		p := filepath.Join(dir, entry.Name())
		msg, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		id, sender, err := headerIDFrom(msg)
		if err != nil {
			return err
		}
		if id+".md" != entry.Name() || sender != src {
			return fmt.Errorf("spool identity mismatch: %s", p)
		}
		binding, err := os.ReadFile(strings.TrimSuffix(p, ".md") + ".dest")
		if err != nil {
			return fmt.Errorf("incomplete spool %s; refusing default destination: %w", p, err)
		}
		dest := strings.TrimSpace(string(binding))
		if dest == "" {
			return fmt.Errorf("empty spool destination: %s", p)
		}
		marker := forwardedPath(root, src, id, dest)
		if b, err := os.ReadFile(marker); err == nil {
			if !bytes.Equal(b, msg) {
				return fmt.Errorf("spool payload conflicts with forwarding record: %s", p)
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := syncSpool(p); err != nil {
			return err
		}
		if err := durable.WriteFile(marker, msg, 0600); err != nil {
			return err
		}
	}
	return nil
}

// DeriveTransferID reproduces amq-bridge's transfer identity so a
// destination-bound receipt can be looked up by name. It copies
// internal/bridge.DeriveTransferID from agent-message-queue v0.80.1
// (envelope v2 preimage "amq-xfer-v2\0host\0handle\0id\0dest", sha256,
// lowercase unpadded base32). Upstream is the owner; TestTransferIDMatchesUpstream
// drives the real binary and fails the moment this drifts.
func DeriveTransferID(host, src, id, dest string) string {
	sum := sha256.Sum256([]byte("amq-xfer-v2\x00" + host + "\x00" + src + "\x00" + id + "\x00" + dest))
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))
}

// transportAccepted reports whether a destination-bound transport receipt
// exists for this exact message. Old sent archives lack .dest; only such a
// receipt proves that destination was sent.
func transportAccepted(root, host, src, id, dest string, msg []byte) (bool, error) {
	transfer := DeriveTransferID(host, src, id, dest)
	p := filepath.Join(root, "bridge", "receipts", transfer+"__transport_accepted.json")
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var r struct {
		Stage      string `json:"stage"`
		TransferID string `json:"transfer_id"`
		Digest     string `json:"payload_sha256"`
		ID         string `json:"source_message_id"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return false, fmt.Errorf("decode transport receipt: %w", err)
	}
	if r.Stage != "transport_accepted" || r.TransferID != transfer || r.ID != id || !strings.EqualFold(r.Digest, fmt.Sprintf("%x", sha256.Sum256(msg))) {
		return false, fmt.Errorf("transport receipt conflicts with %s to %s", id, dest)
	}
	return true, nil
}
