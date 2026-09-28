package adapter

import (
	"encoding/json"
	"strings"

	"github.com/est7/herdr-amq-adapter/internal/screen"
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

// Outcome is the classified result of one delivery attempt.
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

// herdrError is the JSON shape herdr prints on stderr for server errors.
type herdrError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ClassifyHerdrResult maps a herdr exit status and stderr to the amq
// progress protocol. Codes that guarantee nothing was typed defer; any
// other failure is terminal for this cohort.
//
//	rc 0                                  -> accepted
//	agent_blocked, server_not_running     -> deferred (approval UI; server restarting)
//	anything else                         -> failed   (not found, stalled, usage error)
func ClassifyHerdrResult(exitCode int, stderr string) Outcome {
	if exitCode == 0 {
		return Outcome{Progress: ProgressAccepted}
	}
	code, msg := parseHerdrError(stderr)
	switch code {
	case "agent_blocked", "server_not_running":
		return Outcome{Progress: ProgressDeferred, Code: code, Note: msg}
	case "":
		return Outcome{Progress: ProgressFailed, Code: "exit_" + itoa(exitCode), Note: strings.TrimSpace(stderr)}
	default:
		return Outcome{Progress: ProgressFailed, Code: code, Note: msg}
	}
}

// Gate decides from an agent's visible screen whether text may be typed
// into it now. Typing into a box that holds someone's unsent draft merges
// the two and submits both; Enter on a trust dialog accepts it for every
// later session in that folder. Both defer until the screen changes. A box
// this package cannot place (unknown kind or layout) does not block.
func Gate(kind, ansiScreen string) (Outcome, bool) {
	if phrase, ok := screen.TrustScreen(kind, ansiScreen); ok {
		return Outcome{Progress: ProgressDeferred, Code: "trust_screen", Note: phrase}, false
	}
	if screen.Check(kind, ansiScreen) == screen.Typed {
		return Outcome{Progress: ProgressDeferred, Code: "draft_in_box", Note: "the input box holds unsent text"}, false
	}
	return Outcome{}, true
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
