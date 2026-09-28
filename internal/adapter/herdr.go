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

// Deliver types text into target's input box, but only once its screen shows
// that doing so is safe (see Gate). The whole call must stay under amq's
// --inject-timeout (5s default) so amq sees our marker, not its own timeout.
//
// Everything before `agent prompt` types nothing, so its failures (timeout
// included) defer and amq retries. The prompt itself is sent without --wait;
// Herdr owns the blocked check and returns agent_blocked before sending
// input, while a prompt that timed out may have typed and is not retried.
func (h Herdr) Deliver(target, text string, timeout time.Duration) (Outcome, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	kind, screenText, refused, err := h.look(ctx, target)
	if err != nil || refused != nil {
		return *refused, err
	}
	if out, ok := Gate(kind, screenText); !ok {
		return out, nil
	}
	return h.call(ctx, ProgressFailed, "agent", "prompt", target, text)
}

// look returns the agent's kind and visible screen, or the outcome that
// refuses delivery before anything is typed.
func (h Herdr) look(ctx context.Context, target string) (kind, screenText string, refused *Outcome, err error) {
	out, err := h.call(ctx, ProgressDeferred, "agent", "get", target)
	if err != nil || out.Progress != ProgressAccepted {
		return "", "", &out, err
	}
	var resp struct {
		Result struct {
			Agent AgentInfo `json:"agent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out.Note), &resp); err != nil {
		return "", "", &Outcome{Progress: ProgressFailed, Code: "decode", Note: "agent get: " + err.Error()}, nil
	}
	if resp.Result.Agent.Agent != nil {
		kind = *resp.Result.Agent.Agent
	}
	out, err = h.call(ctx, ProgressDeferred, "agent", "read", target, "--source", "visible", "--format", "ansi")
	if err != nil || out.Progress == ProgressDeferred {
		return "", "", &out, err
	}
	if out.Progress != ProgressAccepted {
		// An unreadable screen might hide a draft or a trust dialog: never
		// type blind, try again later.
		return "", "", &Outcome{Progress: ProgressDeferred, Code: "screen_unreadable", Note: out.Code + ": " + out.Note}, nil
	}
	return kind, out.Note, nil, nil
}

// call runs one herdr command under ctx. On success the outcome is accepted
// and Note carries stdout; a timeout is reported with onTimeout, because
// only the caller knows whether the command may already have typed.
func (h Herdr) call(ctx context.Context, onTimeout Progress, args ...string) (Outcome, error) {
	stdout, errs, rc, err := h.run(ctx, args...)
	if ctx.Err() != nil {
		return Outcome{Progress: onTimeout, Code: "timeout", Note: "herdr " + strings.Join(args[:2], " ") + " ran out of time"}, nil
	}
	if err != nil {
		return Outcome{Progress: ProgressFailed, Code: "exec", Note: err.Error()}, err
	}
	out := ClassifyHerdrResult(rc, errs)
	if out.Progress == ProgressAccepted {
		out.Note = stdout
	}
	return out, nil
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
