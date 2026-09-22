package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// EnsureIdentity makes root a bridge host named host: writes bridge/host-id
// (amq-bridge requires it before identity init) and generates the Ed25519
// identity once. Idempotent.
func EnsureIdentity(ctx context.Context, bridgeBin, root, host string) error {
	if err := ValidHost(host); err != nil {
		return err
	}
	dir := filepath.Join(root, "bridge")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	hostID := filepath.Join(dir, "host-id")
	if b, err := os.ReadFile(hostID); err == nil {
		if got := strings.TrimSpace(string(b)); got != host {
			return fmt.Errorf("root %s is already bridge host %q, not %q", root, got, host)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(hostID, []byte(host+"\n"), 0o600); err != nil {
			return err
		}
	} else {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "identity")); err == nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, bridgeBin, "identity", "init", "--root", root).CombinedOutput()
	if err != nil {
		return fmt.Errorf("amq-bridge identity init: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

var publicLine = regexp.MustCompile(`generation=(\S+) public=([0-9a-f]+)`)

// PublicKey returns this root's trusted-file record, the exact bytes a peer
// must store at <its root>/bridge/trusted/<our host>.
func PublicKey(ctx context.Context, bridgeBin, root string) (string, error) {
	out, err := exec.CommandContext(ctx, bridgeBin, "identity", "public", "--root", root).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("amq-bridge identity public: %w: %s", err, strings.TrimSpace(string(out)))
	}
	m := publicLine.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("unexpected identity public output: %q", strings.TrimSpace(string(out)))
	}
	return fmt.Sprintf("generation %s\npublic %s\n", m[1], m[2]), nil
}

// Trust installs a peer's public record. amq-bridge reads
// bridge/trusted/<host> as a regular mode-0600 file.
func Trust(root, host, record string) error {
	if err := ValidHost(host); err != nil {
		return err
	}
	if !strings.HasPrefix(record, "generation ") || !strings.Contains(record, "\npublic ") {
		return fmt.Errorf("trusted record for %s is not a generation/public file", host)
	}
	dir := filepath.Join(root, "bridge", "trusted")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	p := filepath.Join(dir, host)
	if b, err := os.ReadFile(p); err == nil && bytes.Equal(b, []byte(record)) {
		return nil
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(record), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
