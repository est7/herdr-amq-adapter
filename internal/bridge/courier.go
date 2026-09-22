package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EnqueueConfig is the private file `amq-bridge enqueue --config` reads.
// One per (local sender, peer): the spool is keyed by SourceHandle, which
// must equal the readdressed message's from.
type EnqueueConfig struct {
	Root               string   `json:"root"`
	SourceHost         string   `json:"source_host"`
	SourceHandle       string   `json:"source_handle"`
	AllowedDestAliases []string `json:"allowed_dest_aliases"`
}

func enqueueConfigPath(stateDir, sourceHandle string) string {
	return filepath.Join(stateDir, "bridge", "enqueue", sourceHandle+".json")
}

// WriteEnqueueConfig persists the config amq-bridge requires (mode 0600,
// regular file) and returns its path.
func WriteEnqueueConfig(stateDir string, cfg EnqueueConfig) (string, error) {
	p := enqueueConfigPath(stateDir, cfg.SourceHandle)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return "", err
	}
	return p, os.Rename(tmp, p)
}

// Enqueue hands one readdressed message to amq-bridge's spool for dest.
func Enqueue(ctx context.Context, bridgeBin, configPath, dest string, msg []byte) error {
	cmd := exec.CommandContext(ctx, bridgeBin, "enqueue", "--config", configPath, "--dest-alias", dest)
	cmd.Stdin = bytes.NewReader(msg)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("amq-bridge enqueue %s: %w: %s", dest, err, strings.TrimSpace(out.String()))
	}
	return nil
}

// CourierSpec is one bounded `amq-bridge` cycle.
type CourierSpec struct {
	Root          string
	RendezvousURL string
	LocalHost     string
	LocalAgent    string   // real local handle; receive alias is <LocalHost>/<LocalAgent>
	SourceHandle  string   // spool to drain on push: the alias peers know LocalAgent by
	DestAliases   []string // every remote alias this host may address
	PeerHosts     []string // inbound source allowlist
	Mode          string   // push | poll
}

// CourierArgs is the pure argv builder for one cycle. --allow-dest must
// include the receive alias itself: amq-bridge validates the poll target
// against the same allowlist as push destinations.
func CourierArgs(s CourierSpec) []string {
	receive := DestAlias(s.LocalHost, s.LocalAgent)
	allow := append(append([]string{}, s.DestAliases...), receive)
	dest := receive
	if len(s.DestAliases) > 0 {
		dest = s.DestAliases[0]
	}
	return []string{
		"--root", s.Root,
		"--rendezvous", s.RendezvousURL,
		"--source-host", s.LocalHost,
		"--source-handle", s.SourceHandle,
		"--dest-alias", dest,
		"--receive-alias", receive,
		"--allow-dest", strings.Join(allow, ","),
		"--allow-source-host", strings.Join(s.PeerHosts, ","),
		"--mode", s.Mode,
		"--once",
	}
}

// CourierResult is one receipt a cycle reported: transport_accepted for a
// push, destination_maildir_committed for an apply. amq-bridge prints one
// JSON object per line.
type CourierResult struct {
	Stage           string `json:"stage"`
	TransferID      string `json:"transfer_id"`
	SourceMessageID string `json:"source_message_id"`
	CommittedPath   string `json:"committed_path"`
}

// RefusedTransfer is an envelope the poll skipped instead of applying:
// an uncertain ledger history (retried by redelivery) or a terminal
// conflict (operator action). amq-bridge prints them as one
// {"refused":[...]} line after the receipts.
type RefusedTransfer struct {
	TransferID string `json:"transfer_id"`
	Reason     string `json:"reason"`
	Uncertain  bool   `json:"uncertain"`
	Conflict   bool   `json:"conflict"`
}

// CourierOutcome is everything one cycle printed, kept apart by kind so a
// refusal or an unresolved-ledger diagnostic is never mistaken for a blank
// receipt. Diagnostics are upstream's own JSON, retained verbatim.
type CourierOutcome struct {
	Receipts    []CourierResult
	Refused     []RefusedTransfer
	Diagnostics []json.RawMessage
}

// ParseCourierOutput classifies amq-bridge's stdout lines. A line with a
// "stage" is a receipt, a line with "refused" carries refusals, any other
// JSON object is a diagnostic; non-JSON lines are ignored. A malformed
// JSON line is an error: the contract is one object per line.
func ParseCourierOutput(stdout []byte) (CourierOutcome, error) {
	var out CourierOutcome
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var probe map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			return CourierOutcome{}, fmt.Errorf("decode courier output %q: %w", line, err)
		}
		switch {
		case probe["stage"] != nil:
			var r CourierResult
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				return CourierOutcome{}, fmt.Errorf("decode courier receipt: %w", err)
			}
			out.Receipts = append(out.Receipts, r)
		case probe["refused"] != nil:
			var r struct {
				Refused []RefusedTransfer `json:"refused"`
			}
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				return CourierOutcome{}, fmt.Errorf("decode courier refusals: %w", err)
			}
			out.Refused = append(out.Refused, r.Refused...)
		default:
			out.Diagnostics = append(out.Diagnostics, json.RawMessage(line))
		}
	}
	return out, nil
}

// RunCourier executes one cycle. Whatever the process printed is returned
// even when it exits non-zero, so partial receipts and diagnostics are not
// lost with the failure.
func RunCourier(ctx context.Context, bridgeBin string, s CourierSpec) (CourierOutcome, error) {
	cmd := exec.CommandContext(ctx, bridgeBin, CourierArgs(s)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	runErr := cmd.Run()
	outcome, parseErr := ParseCourierOutput(out.Bytes())
	if runErr != nil {
		return outcome, fmt.Errorf("amq-bridge %s %s/%s: %w: %s", s.Mode, s.LocalHost, s.LocalAgent, runErr, strings.TrimSpace(errb.String()))
	}
	return outcome, parseErr
}
