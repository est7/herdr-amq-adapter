package adapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// EnsureMailbox makes handle a fully provisioned member of the shared root:
// `amq init --force` is idempotent for directories and rewrites
// meta/config.json with the merged agent list (the adapter owns that file).
func EnsureMailbox(ctx context.Context, amqBin, root, handle string) error {
	unlock, err := lockMailboxRegistry(ctx, root)
	if err != nil {
		return err
	}
	defer unlock()
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

// All registry writers, including SSH aliases and the runner, share this lock.
func lockMailboxRegistry(ctx context.Context, root string) (func(), error) {
	dir := filepath.Join(root, "meta")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".adapter-registry.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func mailboxPresent(root, handle string) bool {
	st, err := os.Stat(filepath.Join(root, "agents", handle, "inbox", "new"))
	return err == nil && st.IsDir()
}
