package adapter

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EnsureMailbox makes handle a fully provisioned member of the shared root:
// `amq init --force` is idempotent for directories and rewrites
// meta/config.json with the merged agent list (the adapter owns that file).
func EnsureMailbox(ctx context.Context, amqBin, root, handle string) error {
	agents := []string{handle}
	cfgPath := filepath.Join(root, "meta", "config.json")
	if b, err := os.ReadFile(cfgPath); err == nil {
		merged, changed, err := AgentsWith(b, handle)
		if err != nil {
			return err
		}
		if !changed && mailboxPresent(root, handle) {
			return nil
		}
		agents = merged
	} else if !os.IsNotExist(err) {
		return err
	}
	cmd := exec.CommandContext(ctx, amqBin, "init", "--root", root, "--agents", strings.Join(agents, ","), "--force")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("amq init --force %s: %w: %s", root, err, out.String())
	}
	return nil
}

func mailboxPresent(root, handle string) bool {
	st, err := os.Stat(filepath.Join(root, "agents", handle, "inbox", "new"))
	return err == nil && st.IsDir()
}
