package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Herdr wraps the herdr CLI. Plugins are told to call through HERDR_BIN_PATH;
// the socket path is inherited through HERDR_SOCKET_PATH in the environment.
type Herdr struct{ Bin string }

func HerdrFromEnv() Herdr {
	if bin := os.Getenv("HERDR_BIN_PATH"); bin != "" {
		return Herdr{Bin: bin}
	}
	return Herdr{Bin: "herdr"}
}

func (h Herdr) run(ctx context.Context, args ...string) (stdout string, stderr string, exitCode int, err error) {
	cmd := exec.CommandContext(ctx, h.Bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	exitCode = 0
	if ee, ok := err.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
		err = nil
	}
	return out.String(), errb.String(), exitCode, err
}

func (h Herdr) AgentList(ctx context.Context) ([]AgentInfo, error) {
	out, errs, rc, err := h.run(ctx, "agent", "list")
	if err != nil {
		return nil, fmt.Errorf("herdr agent list: %w", err)
	}
	if rc != 0 {
		return nil, fmt.Errorf("herdr agent list rc=%d: %s", rc, errs)
	}
	var resp struct {
		Result struct {
			Agents []AgentInfo `json:"agents"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, fmt.Errorf("decode agent list: %w", err)
	}
	return resp.Result.Agents, nil
}

func (h Herdr) AgentGet(ctx context.Context, target string) (AgentInfo, bool, error) {
	out, errs, rc, err := h.run(ctx, "agent", "get", target)
	if err != nil {
		return AgentInfo{}, false, fmt.Errorf("herdr agent get: %w", err)
	}
	if rc != 0 {
		if code, _ := parseHerdrError(errs); code == "agent_not_found" || code == "not_found" {
			return AgentInfo{}, false, nil
		}
		return AgentInfo{}, false, fmt.Errorf("herdr agent get rc=%d: %s", rc, errs)
	}
	var resp struct {
		Result struct {
			Agent AgentInfo `json:"agent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return AgentInfo{}, false, fmt.Errorf("decode agent get: %w", err)
	}
	return resp.Result.Agent, true, nil
}

// Prompt submits without --wait; Herdr owns the blocked check
// and returns agent_blocked before sending input. The
// whole call must stay under amq's --inject-timeout (5s default) so amq sees
// our marker, not its own timeout.
func (h Herdr) Prompt(target, text string, timeout time.Duration) (Outcome, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, errs, rc, err := h.run(ctx, "agent", "prompt", target, text)
	if err != nil {
		return Outcome{Progress: ProgressFailed, Code: "exec", Note: err.Error()}, err
	}
	if ctx.Err() != nil {
		return Outcome{Progress: ProgressFailed, Code: "timeout", Note: "herdr agent prompt exceeded " + timeout.String()}, nil
	}
	return ClassifyPromptResult(rc, errs), nil
}

// AgentRename assigns a Herdr live name to the agent hosted by target.
func (h Herdr) AgentRename(ctx context.Context, target, name string) error {
	_, errs, rc, err := h.run(ctx, "agent", "rename", target, name)
	if err != nil {
		return fmt.Errorf("herdr agent rename: %w", err)
	}
	if rc != 0 {
		return fmt.Errorf("herdr agent rename %s %s rc=%d: %s", target, name, rc, strings.TrimSpace(errs))
	}
	return nil
}
