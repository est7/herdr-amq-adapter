package adapter

import (
	"encoding/json"
	"strings"
)

// Progress is the amq --inject-via acknowledgement protocol, emitted on
// stderr as AMQ_INJECT_PROGRESS=<value>.
type Progress string

const (
	// ProgressAccepted: the prompt was written to the agent pane.
	ProgressAccepted Progress = "accepted"
	// ProgressDeferred: keep the cohort and retry through the wake loop.
	ProgressDeferred Progress = "deferred"
	// ProgressFailed: terminal for this unchanged cohort; a new inbox change re-arms.
	ProgressFailed Progress = "failed"
)

// Outcome is the classified result of one `herdr agent prompt` run.
type Outcome struct {
	Progress Progress
	Code     string // herdr error code when present
	Note     string
}

// ExitCode is what the injector process must exit with for this outcome.
// amq's classifier (internal/cli/wake.go classifyInjectViaResult) accepts
// the deferred marker ONLY together with a non-zero exit; deferred + exit 0
// is downgraded to "uncertain", which is terminal and never replayed.
func (o Outcome) ExitCode() int {
	if o.Progress == ProgressAccepted {
		return 0
	}
	return 1
}

// GateOnStatus decides, from the agent's current Herdr status, whether to
// submit now. Only a settled, input-ready agent gets the notice; a busy or
// blocked one defers so amq retries on its own ladder (5s base, 2m cap, no
// budget consumed). "unknown" cannot prove anything and is treated as ready,
// matching Herdr's own guidance that it does not imply completion.
func GateOnStatus(status string) (Outcome, bool) {
	switch status {
	case "working":
		return Outcome{Progress: ProgressDeferred, Code: "agent_working", Note: "agent mid-turn; retry when settled"}, false
	case "blocked":
		return Outcome{Progress: ProgressDeferred, Code: "agent_blocked", Note: "agent at approval/question UI"}, false
	default:
		return Outcome{}, true
	}
}

// herdrError is the JSON shape herdr prints on stderr for server errors.
type herdrError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ClassifyPromptResult maps a `herdr agent prompt` exit status and stderr to
// the amq progress protocol.
//
//	rc 0                      -> accepted
//	agent_blocked             -> deferred (approval/question UI; retry when free)
//	anything else             -> failed   (not found, stalled, usage error, timeout)
func ClassifyPromptResult(exitCode int, stderr string) Outcome {
	if exitCode == 0 {
		return Outcome{Progress: ProgressAccepted}
	}
	code, msg := parseHerdrError(stderr)
	switch code {
	case "agent_blocked":
		return Outcome{Progress: ProgressDeferred, Code: code, Note: msg}
	case "":
		return Outcome{Progress: ProgressFailed, Code: "exit_" + itoa(exitCode), Note: strings.TrimSpace(stderr)}
	default:
		return Outcome{Progress: ProgressFailed, Code: code, Note: msg}
	}
}

func parseHerdrError(stderr string) (code, message string) {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var he herdrError
		if err := json.Unmarshal([]byte(line), &he); err == nil && he.Error.Code != "" {
			return he.Error.Code, he.Error.Message
		}
	}
	return "", ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
