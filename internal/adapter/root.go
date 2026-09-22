package adapter

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// EnsureRoot initialises the shared amq root on first use. All Herdr agents
// share one root so they can address each other by handle regardless of
// project cwd.
func EnsureRoot(ctx context.Context, amqBin, root, firstHandle string) error {
	if _, err := os.Stat(filepath.Join(root, "meta", "config.json")); err == nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, amqBin, "init", "--root", root, "--agents", firstHandle)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("amq init %s: %w: %s", root, err, out.String())
	}
	return nil
}

// RegisterHandle adds handle to <root>/meta/config.json if absent.
func RegisterHandle(root, handle string) error {
	p := filepath.Join(root, "meta", "config.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	updated, changed, err := AddAgent(b, handle)
	if err != nil || !changed {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, updated, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
