// herdr-amq-adapter is a Herdr plugin binary with four entry points:
//
//	hook       — [[events]] handler: adopt/retire the event's pane
//	reconcile  — [[startup]] / action: diff live agents vs recorded wakers
//	inject     — amq --inject-via target: gate on status, `herdr agent prompt`
//	status     — action: print the waker inventory
//
// Adoption is zero-config: an unnamed agent is named after its kind
// (claude, codex-2, …) via `herdr agent rename`; that name is its AMQ handle.
// All agents share one amq root under the plugin state dir, and each pane
// gets a sourceable identity file under the plugin config dir.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/est7/herdr-amq-adapter/internal/adapter"
)

const promptTimeout = 4 * time.Second // < amq --inject-timeout (5s)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "hook":
		err = runHook()
	case "reconcile":
		err = runReconcile()
	case "inject":
		os.Exit(runInject(os.Args[2:]))
	case "status":
		err = runStatus()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "herdr-amq-adapter:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: herdr-amq-adapter hook|reconcile|status|inject <pane_id> <handle> <root> <payload>")
}

type env struct {
	herdr     adapter.Herdr
	amq       string
	self      string
	store     *adapter.Store
	logs      string
	root      string
	configDir string
}

func loadEnv() (env, error) {
	stateDir := os.Getenv("HERDR_PLUGIN_STATE_DIR")
	configDir := os.Getenv("HERDR_PLUGIN_CONFIG_DIR")
	if configDir == "" {
		return env{}, errors.New("HERDR_PLUGIN_CONFIG_DIR is not set")
	}
	store, err := adapter.NewStore(stateDir)
	if err != nil {
		return env{}, err
	}
	self, err := os.Executable()
	if err != nil {
		return env{}, err
	}
	amq := os.Getenv("AMQ_BIN")
	if amq == "" {
		amq, err = exec.LookPath("amq")
		if err != nil {
			return env{}, errors.New("amq not found on PATH (set AMQ_BIN)")
		}
	}
	return env{
		herdr:     adapter.HerdrFromEnv(),
		amq:       amq,
		self:      self,
		store:     store,
		logs:      filepath.Join(stateDir, "logs"),
		root:      filepath.Join(stateDir, "amq-root"),
		configDir: configDir,
	}, nil
}

func runHook() error {
	ev, err := adapter.ParseEvent(os.Getenv("HERDR_PLUGIN_EVENT_JSON"))
	if err != nil {
		return err
	}
	act := adapter.Decide(ev)
	fmt.Printf("event=%s pane=%s action=%s (%s)\n", ev.Event, act.PaneID, act.Kind, act.Reason)
	if act.Kind == adapter.ActionNone {
		return nil
	}
	e, err := loadEnv()
	if err != nil {
		return err
	}
	unlock, err := e.store.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	switch act.Kind {
	case adapter.ActionEnsure:
		return ensure(e, act.PaneID)
	case adapter.ActionStop:
		return stop(e, act.PaneID)
	case adapter.ActionPark:
		return park(e, act.PaneID)
	case adapter.ActionMove:
		return move(e, act.PreviousPaneID, act.PaneID)
	}
	return nil
}

// move re-keys a waker record after `herdr pane move`. The waker itself keeps
// running: it targets the agent by handle, which Herdr carries across moves.
// The old identity file stays because the moved process still sees its
// original HERDR_PANE_ID; it is removed with the record at stop time.
func move(e env, oldPane, newPane string) error {
	rec, ok, err := e.store.Get(oldPane)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Printf("pane %s moved to %s but had no waker record; adopting fresh\n", oldPane, newPane)
		return ensure(e, newPane)
	}
	rec.PaneAliases = append(rec.PaneAliases, oldPane)
	rec.PaneID = newPane
	if err := e.store.Put(rec); err != nil {
		return err
	}
	if err := e.store.Delete(oldPane); err != nil {
		return err
	}
	if err := adapter.WriteIdentity(e.configDir, adapter.Identity{PaneID: newPane, Handle: rec.Handle, Root: rec.Root}); err != nil {
		return err
	}
	fmt.Printf("moved pane=%s -> %s handle=%s waker pid=%d\n", oldPane, newPane, rec.Handle, rec.PID)
	return nil
}

// ensure adopts the agent in paneID: name it if needed, register the handle
// in the shared root, write the pane identity file, and make sure amq runs
// a waker with exactly the injector target this pane wants. It is
// idempotent, so both the hook and reconcile can call it. An existing
// record is consulted first: its handle is reused for an unnamed agent
// (Herdr drops the live name when an agent is released), and its argv pane
// id is kept so a moved pane still proves its own waker.
func ensure(e env, paneID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	info, ok, err := e.herdr.AgentGet(ctx, paneID)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Printf("pane %s hosts no agent; nothing to adopt\n", paneID)
		return nil
	}
	rec, exists, err := e.store.Get(paneID)
	if err != nil {
		return err
	}
	handle, named := adapter.Handle(info)
	if !named {
		live, err := e.herdr.AgentList(ctx)
		if err != nil {
			return err
		}
		preferred := ""
		if exists {
			preferred = rec.Handle
		}
		handle = adapter.ChooseHandle(info, adapter.TakenNames(live), preferred)
		if err := e.herdr.AgentRename(ctx, paneID, handle); err != nil {
			return err
		}
		fmt.Printf("named pane %s agent %q\n", paneID, handle)
	}
	argvPane := paneID
	if exists {
		argvPane = rec.ArgvPane()
	}
	if err := adapter.EnsureMailbox(ctx, e.amq, e.root, handle); err != nil {
		return err
	}
	// The occupant may still see an earlier pane id (aliases after a move),
	// so every identity file it could source must carry the current handle.
	for _, pane := range append([]string{paneID}, rec.PaneAliases...) {
		if err := adapter.WriteIdentity(e.configDir, adapter.Identity{PaneID: pane, Handle: handle, Root: e.root}); err != nil {
			return err
		}
	}
	want := adapter.ExpectedTarget(e.self, handle, argvPane, e.root)
	st, err := adapter.WakeCheck(ctx, e.amq, e.root, handle)
	if err != nil {
		return err
	}
	decision := adapter.DecideWake(st, want)
	switch decision {
	case adapter.DecisionKeep:
	case adapter.DecisionRepair:
		if err := adapter.WakeRepair(ctx, e.amq, e.root, handle); err != nil {
			return err
		}
	case adapter.DecisionReplace, adapter.DecisionStart:
		if err := adapter.WakeRetire(ctx, e.amq, e.root, handle, st); err != nil {
			return err
		}
		if _, err := adapter.Spawn(adapter.WakerSpec{
			AmqBin: e.amq, SelfBin: e.self, LogDir: e.logs, Agent: info, Handle: handle, ArgvPane: argvPane, Root: e.root,
		}); err != nil {
			return err
		}
	}
	if decision != adapter.DecisionKeep {
		liveCtx, liveCancel := context.WithTimeout(ctx, 8*time.Second)
		defer liveCancel()
		if st, err = adapter.AwaitLive(liveCtx, e.amq, e.root, handle, want); err != nil {
			return err
		}
	}
	fresh := adapter.WakerRecord{
		PaneID: paneID, Handle: handle, SpawnPaneID: argvPane, Generation: st.Generation, PID: st.PID,
		Cwd: info.Cwd, Root: e.root, StartedUnix: rec.StartedUnix, PaneAliases: rec.PaneAliases,
	}
	if decision != adapter.DecisionKeep || fresh.StartedUnix == 0 {
		fresh.StartedUnix = time.Now().Unix()
	}
	if err := e.store.Put(fresh); err != nil {
		return err
	}
	fmt.Printf("%s pane=%s handle=%s waker pid=%d gen=%s identity=%s\n",
		decision, paneID, handle, st.PID, st.Generation, adapter.IdentityPath(e.configDir, paneID))
	return nil
}

// retireWaker stops whatever waker amq currently holds for the handle,
// by its exact saved identity and generation.
func retireWaker(ctx context.Context, e env, handle string) error {
	st, err := adapter.WakeCheck(ctx, e.amq, e.root, handle)
	if err != nil {
		return err
	}
	return adapter.WakeRetire(ctx, e.amq, e.root, handle, st)
}

// park handles an agent released from a pane that stays open: the waker is
// retired, but the record (pid 0) and identity file are kept so the next
// agent detected in this pane is offered the same handle by ensure. A
// release event can arrive after the next agent was already detected in
// the same pane; the live occupant decides, not the event order.
func park(e env, paneID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, occupied, err := e.herdr.AgentGet(ctx, paneID); err != nil {
		return err
	} else if occupied {
		fmt.Printf("pane %s hosts an agent again; release is stale, ensuring instead\n", paneID)
		return ensure(e, paneID)
	}
	rec, ok, err := e.store.Get(paneID)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Printf("pane %s has no waker record\n", paneID)
		return nil
	}
	if err := retireWaker(ctx, e, rec.Handle); err != nil {
		return err
	}
	fmt.Printf("parked pane=%s handle=%s waker pid=%d\n", paneID, rec.Handle, rec.PID)
	rec.PID = 0
	rec.Generation = ""
	return e.store.Put(rec)
}

func stop(e env, paneID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rec, ok, err := e.store.Get(paneID)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Printf("pane %s has no waker record\n", paneID)
		return adapter.RemoveIdentity(e.configDir, paneID)
	}
	if err := retireWaker(ctx, e, rec.Handle); err != nil {
		return err
	}
	if err := e.store.Delete(paneID); err != nil {
		return err
	}
	for _, pane := range append([]string{paneID}, rec.PaneAliases...) {
		if err := adapter.RemoveIdentity(e.configDir, pane); err != nil {
			return err
		}
	}
	fmt.Printf("retired pane=%s handle=%s waker pid=%d\n", paneID, rec.Handle, rec.PID)
	return nil
}

func runReconcile() error {
	e, err := loadEnv()
	if err != nil {
		return err
	}
	unlock, err := e.store.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	live, err := e.herdr.AgentList(ctx)
	if err != nil {
		return err
	}
	wakers, err := e.store.List()
	if err != nil {
		return err
	}
	current := func(w adapter.WakerRecord) bool {
		st, err := adapter.WakeCheck(ctx, e.amq, e.root, w.Handle)
		if err != nil {
			return false
		}
		return adapter.DecideWake(st, adapter.ExpectedTarget(e.self, w.Handle, w.ArgvPane(), e.root)) == adapter.DecisionKeep
	}
	plan := adapter.Plan(live, wakers, current)
	for _, w := range plan.Stop {
		if err := stop(e, w.PaneID); err != nil {
			return err
		}
	}
	for _, a := range plan.Start {
		if err := ensure(e, a.PaneID); err != nil {
			return err
		}
	}
	fmt.Printf("reconcile: live=%d stopped=%d started=%d\n", len(live), len(plan.Stop), len(plan.Start))
	return nil
}

func runStatus() error {
	e, err := loadEnv()
	if err != nil {
		return err
	}
	wakers, err := e.store.List()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fmt.Printf("root: %s\n", e.root)
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PANE\tHANDLE\tWAKE\tPID\tCURRENT\tCWD")
	for _, w := range wakers {
		st, err := adapter.WakeCheck(ctx, e.amq, e.root, w.Handle)
		wake, current := "error", false
		if err == nil {
			wake = st.Status
			current = adapter.DecideWake(st, adapter.ExpectedTarget(e.self, w.Handle, w.ArgvPane(), e.root)) == adapter.DecisionKeep
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%v\t%s\n", w.PaneID, w.Handle, wake, st.PID, current, w.Cwd)
	}
	return tw.Flush()
}

// runInject is invoked by amq as `<self> inject <pane_id> <handle> <root> <payload>`.
// pane_id only names the identity file; delivery targets the handle.
// It speaks the AMQ_INJECT_PROGRESS protocol on stderr and never echoes the
// payload.
func runInject(args []string) int {
	if len(args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: herdr-amq-adapter inject <pane_id> <handle> <root> <payload>")
		return 2
	}
	paneID, handle, root, payload := args[0], args[1], args[2], args[3]
	configDir := os.Getenv("HERDR_PLUGIN_CONFIG_DIR")
	id := adapter.Identity{PaneID: paneID, Handle: handle, Root: root}
	text := adapter.Notice(payload, id, adapter.IdentityPath(configDir, paneID))
	// Target the agent by its live name, not the pane: the name follows the
	// occupant across `herdr pane move`, the pane id does not.
	out, _ := adapter.HerdrFromEnv().Prompt(handle, text, promptTimeout)
	fmt.Fprintf(os.Stderr, "AMQ_INJECT_PROGRESS=%s\n", out.Progress)
	if out.Code != "" {
		fmt.Fprintf(os.Stderr, "herdr-amq-adapter: inject pane=%s %s %s\n", paneID, out.Code, out.Note)
	}
	return out.ExitCode()
}
