package adapter

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// WakerSpec is everything needed to start one `amq wake` for a pane.
type WakerSpec struct {
	AmqBin   string
	SelfBin  string // this adapter binary, used as --inject-via
	LogDir   string
	Agent    AgentInfo
	Handle   string
	ArgvPane string // pane id baked into the injector argv (see WakerRecord.SpawnPaneID)
	Root     string // shared amq root the waker watches
}

// AmqWakeArgs is the pure argv builder; kept separate so the contract with
// `amq wake --inject-via` is testable without spawning.
func AmqWakeArgs(selfBin, handle, paneID, root string) []string {
	return []string{
		"wake",
		"--root", root,
		"--me", handle,
		"--inject-via", selfBin,
		"--inject-arg", "inject",
		"--inject-arg", paneID,
		"--inject-arg", handle,
		"--inject-arg", root,
		"--retry-until", "injected",
		"--interrupt-cmd", "none",
		"--no-self-upgrade",
	}
}

// Spawn starts the waker detached: own session, stdio to a log file, cwd set
// to the agent's cwd (the root is passed explicitly). It must not inherit
// the hook's pipes, or Herdr's hook reader would block until the waker exits
// and hold an in-flight slot forever. Starting is not adoption: the caller
// must AwaitLive before recording it, because a start that loses amq's lock
// race exits on its own.
func Spawn(spec WakerSpec) (pid int, err error) {
	if err := os.MkdirAll(spec.LogDir, 0o755); err != nil {
		return 0, err
	}
	logPath := filepath.Join(spec.LogDir, strings.ReplaceAll(spec.Agent.PaneID, ":", "_")+".log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	fmt.Fprintf(logf, "\n=== %s spawn handle=%s pane=%s cwd=%s\n", time.Now().Format(time.RFC3339), spec.Handle, spec.Agent.PaneID, spec.Agent.Cwd)

	cmd := exec.Command(spec.AmqBin, append([]string{"--no-update-check"}, AmqWakeArgs(spec.SelfBin, spec.Handle, spec.ArgvPane, spec.Root)...)...)
	cmd.Dir = spec.Agent.Cwd
	cmd.Stdin = nil
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Env = wakerEnv(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start amq wake: %w", err)
	}
	pid = cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// wakerEnv strips the plugin-invocation variables so the waker (and the
// injector it execs) does not mistake itself for a hook run, and keeps
// HERDR_SOCKET_PATH / HERDR_BIN_PATH which the injector needs.
func wakerEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "HERDR_PLUGIN_EVENT", "HERDR_PLUGIN_EVENT_JSON", "HERDR_PLUGIN_CONTEXT_JSON",
			"HERDR_PLUGIN_ACTION_ID", "HERDR_PANE_ID", "HERDR_TAB_ID", "HERDR_WORKSPACE_ID":
			continue
		}
		out = append(out, kv)
	}
	return out
}
