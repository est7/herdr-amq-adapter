package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// amq owns the waker lifecycle: `amq wake check` proves whether the lock
// holder is live, stale, or missing and names its generation; `amq wake
// retire --if-generation` stops exactly the waker whose saved injector
// identity we name; `amq wake repair` restarts a stale waker from its saved
// target. The adapter never inspects pids or signals processes itself.

// WakeTarget is the injector identity amq saves for a handle
// (agents/<handle>/.wake.target). Two wakers are the same waker when their
// targets are equal.
type WakeTarget struct {
	InjectVia  string   `json:"inject_via"`
	InjectArgs []string `json:"inject_args"`
	RetryUntil string   `json:"retry_until"`
}

// ExpectedTarget is the target the adapter wants for a pane's waker; it is
// the same argv AmqWakeArgs passes to `amq wake`.
func ExpectedTarget(selfBin, handle, paneID, root string) WakeTarget {
	return WakeTarget{
		InjectVia:  selfBin,
		InjectArgs: []string{"inject", paneID, handle, root},
		RetryUntil: "injected",
	}
}

// Equal compares injector identities by path identity: amq may save a
// resolved path (/private/var for /var on macOS) for what we passed
// unresolved. The raw form is kept for retire, which amq matches verbatim.
func (t WakeTarget) Equal(o WakeTarget) bool {
	t, o = t.canonical(), o.canonical()
	if t.InjectVia != o.InjectVia || t.RetryUntil != o.RetryUntil || len(t.InjectArgs) != len(o.InjectArgs) {
		return false
	}
	for i := range t.InjectArgs {
		if t.InjectArgs[i] != o.InjectArgs[i] {
			return false
		}
	}
	return true
}

// canonical resolves symlinks in path-shaped fields.
func (t WakeTarget) canonical() WakeTarget {
	out := WakeTarget{InjectVia: canonPath(t.InjectVia), RetryUntil: t.RetryUntil, InjectArgs: make([]string, len(t.InjectArgs))}
	for i, a := range t.InjectArgs {
		out.InjectArgs[i] = canonPath(a)
	}
	return out
}

func canonPath(p string) string {
	if !filepath.IsAbs(p) {
		return p
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// retireArgs is the exact identity `amq wake retire` demands.
func (t WakeTarget) retireArgs() []string {
	args := []string{"--inject-via", t.InjectVia}
	for _, a := range t.InjectArgs {
		args = append(args, "--inject-arg", a)
	}
	return append(args, "--retry-until", t.RetryUntil)
}

// WakeState is one observation of a handle's waker.
type WakeState struct {
	Status       string // valid | stale | missing | other amq statuses
	Live         bool
	PID          int
	Generation   string
	TargetDigest string
	Target       WakeTarget
	HasTarget    bool
}

// ParseWakeCheck decodes `amq wake check --json --json-schema 2`.
func ParseWakeCheck(js []byte) (WakeState, error) {
	var doc struct {
		Schema int `json:"schema"`
		Wake   struct {
			Status       string  `json:"status"`
			Live         bool    `json:"live"`
			PID          *int    `json:"pid"`
			Generation   *string `json:"generation"`
			TargetDigest *string `json:"target_digest"`
		} `json:"wake"`
	}
	if err := json.Unmarshal(js, &doc); err != nil {
		return WakeState{}, fmt.Errorf("decode wake check: %w", err)
	}
	if doc.Schema != 2 {
		return WakeState{}, fmt.Errorf("wake check schema %d, want 2", doc.Schema)
	}
	st := WakeState{Status: doc.Wake.Status, Live: doc.Wake.Live}
	if doc.Wake.PID != nil {
		st.PID = *doc.Wake.PID
	}
	if doc.Wake.Generation != nil {
		st.Generation = *doc.Wake.Generation
	}
	if doc.Wake.TargetDigest != nil {
		st.TargetDigest = *doc.Wake.TargetDigest
	}
	return st, nil
}

// ReadWakeTarget loads the saved target; ok is false when none is saved.
func ReadWakeTarget(root, handle string) (WakeTarget, bool, error) {
	b, err := os.ReadFile(filepath.Join(root, "agents", handle, ".wake.target"))
	if errors.Is(err, os.ErrNotExist) {
		return WakeTarget{}, false, nil
	}
	if err != nil {
		return WakeTarget{}, false, err
	}
	var t WakeTarget
	if err := json.Unmarshal(b, &t); err != nil {
		return WakeTarget{}, false, fmt.Errorf("decode wake target: %w", err)
	}
	return t, true, nil
}

// WakeCheck observes the handle's waker without mutating anything. The
// generation and the saved target come from two reads, so the target read
// is fenced by a check on either side: a replacement always publishes a
// new generation and target digest, and a snapshot whose two checks
// disagree is discarded and retried.
func WakeCheck(ctx context.Context, amqBin, root, handle string) (WakeState, error) {
	var last error
	for attempt := 0; attempt < 4; attempt++ {
		before, err := wakeCheckOnce(ctx, amqBin, root, handle)
		if err != nil {
			return WakeState{}, err
		}
		target, hasTarget, err := ReadWakeTarget(root, handle)
		if err != nil {
			return WakeState{}, err
		}
		after, err := wakeCheckOnce(ctx, amqBin, root, handle)
		if err != nil {
			return WakeState{}, err
		}
		if before.Generation == after.Generation && before.TargetDigest == after.TargetDigest && before.Status == after.Status {
			before.Target, before.HasTarget = target, hasTarget
			return before, nil
		}
		last = fmt.Errorf("wake state for %s changed during observation (%s/%s -> %s/%s)", handle, before.Status, before.Generation, after.Status, after.Generation)
		select {
		case <-ctx.Done():
			return WakeState{}, last
		case <-time.After(100 * time.Millisecond):
		}
	}
	return WakeState{}, last
}

func wakeCheckOnce(ctx context.Context, amqBin, root, handle string) (WakeState, error) {
	out, err := runAmq(ctx, amqBin, "wake", "check", "--root", root, "--me", handle, "--json", "--json-schema", "2")
	if err != nil {
		return WakeState{}, err
	}
	return ParseWakeCheck(out)
}

// WakeDecision is what ensure must do for a handle given its observed waker
// and the target the adapter wants.
type WakeDecision int

const (
	// DecisionKeep: a live waker with exactly the wanted target.
	DecisionKeep WakeDecision = iota
	// DecisionStart: no lock at all; start fresh.
	DecisionStart
	// DecisionRepair: proven-stale lock whose saved target is the wanted one;
	// amq restarts it from that target.
	DecisionRepair
	// DecisionReplace: a waker (live or stale) with a different target, or any
	// other lock state; retire it by its exact saved identity, then start.
	DecisionReplace
)

func (d WakeDecision) String() string {
	switch d {
	case DecisionKeep:
		return "keep"
	case DecisionStart:
		return "start"
	case DecisionRepair:
		return "repair"
	default:
		return "replace"
	}
}

func DecideWake(st WakeState, want WakeTarget) WakeDecision {
	same := st.HasTarget && st.Target.Equal(want)
	switch {
	case st.Status == "missing":
		return DecisionStart
	case st.Status == "valid" && st.Live && same:
		return DecisionKeep
	case st.Status == "stale" && same:
		return DecisionRepair
	default:
		return DecisionReplace
	}
}

// WakeRetire stops the waker whose saved target and generation are exactly
// the observed ones. amq refuses when either changed since the observation,
// so a replacement published in between is never retired by mistake. A
// missing lock is not an error: there is nothing to retire. A lock without
// a generation cannot be fenced and is never retired by this plugin.
func WakeRetire(ctx context.Context, amqBin, root, handle string, st WakeState) error {
	if st.Status == "missing" {
		return nil
	}
	if !st.HasTarget {
		return fmt.Errorf("wake lock for %s has no saved target; refusing an unidentified retire", handle)
	}
	if st.Generation == "" {
		return fmt.Errorf("wake lock for %s has no generation; refusing an unfenced retire", handle)
	}
	args := append([]string{"wake", "retire", "--root", root, "--me", handle, "--json"}, st.Target.retireArgs()...)
	args = append(args, "--if-generation", st.Generation)
	out, err := runAmq(ctx, amqBin, args...)
	status, reason := parseStatusReason(out)
	if status == "retired" {
		return nil
	}
	if strings.Contains(reason, "no wake lock present") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("amq wake retire %s: %w", handle, err)
	}
	return fmt.Errorf("amq wake retire %s: %s: %s", handle, status, reason)
}

// WakeRepair restarts a proven-stale waker from its saved target.
func WakeRepair(ctx context.Context, amqBin, root, handle string) error {
	out, err := runAmq(ctx, amqBin, "wake", "repair", "--root", root, "--me", handle, "--json")
	status, reason := parseStatusReason(out)
	if status == "repaired" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("amq wake repair %s: %w", handle, err)
	}
	return fmt.Errorf("amq wake repair %s: %s: %s", handle, status, reason)
}

// AwaitLive polls until amq reports a live waker with the wanted target, so
// a record is only written for a waker that actually holds the lock (a
// spawn that lost a lock race exits and must not be recorded).
func AwaitLive(ctx context.Context, amqBin, root, handle string, want WakeTarget) (WakeState, error) {
	var last WakeState
	for {
		st, err := WakeCheck(ctx, amqBin, root, handle)
		if err != nil {
			return WakeState{}, err
		}
		if DecideWake(st, want) == DecisionKeep {
			return st, nil
		}
		last = st
		select {
		case <-ctx.Done():
			return WakeState{}, fmt.Errorf("waker for %s not live: status=%s live=%v", handle, last.Status, last.Live)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func runAmq(ctx context.Context, amqBin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, amqBin, append([]string{"--no-update-check"}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// amq reports refusals as JSON on stdout with a non-zero exit;
			// callers classify by the decoded status.
			return out.Bytes(), fmt.Errorf("rc=%d: %s", ee.ExitCode(), strings.TrimSpace(errb.String()))
		}
		return nil, fmt.Errorf("amq %s: %w", strings.Join(args, " "), err)
	}
	return out.Bytes(), nil
}

func parseStatusReason(out []byte) (status, reason string) {
	var doc struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(out, &doc)
	return doc.Status, doc.Reason
}

// Ownership is what a record can prove about the lock currently held for
// its handle.
type Ownership int

const (
	// OwnsNothing: no lock, or the record is parked (owns no generation).
	OwnsNothing Ownership = iota
	// OwnsLock: the live lock carries the record's generation and target.
	OwnsLock
	// ForeignLock: a positively different generation; someone else's waker
	// (the handle was reused after a delayed release). Leave it alone.
	ForeignLock
)

// DecideOwnership classifies the observed lock against the record. An
// observation that cannot identify the lock (no generation, no saved
// target, or a target that disagrees with the generation's record) is an
// error: cleanup must fail closed rather than abandon a possibly live
// waker and discard the only credential that could retire it.
func DecideOwnership(st WakeState, rec WakerRecord, want WakeTarget) (Ownership, error) {
	if rec.Generation == "" || st.Status == "missing" {
		return OwnsNothing, nil
	}
	if st.Generation == "" || !st.HasTarget {
		return OwnsNothing, fmt.Errorf("wake lock for %s cannot be identified (generation %q, target present %v); refusing to treat it as retired", rec.Handle, st.Generation, st.HasTarget)
	}
	if st.Generation != rec.Generation {
		return ForeignLock, nil
	}
	if !st.Target.Equal(want) {
		return OwnsNothing, fmt.Errorf("wake lock for %s has this record's generation %s but a different target; refusing to guess", rec.Handle, rec.Generation)
	}
	return OwnsLock, nil
}
