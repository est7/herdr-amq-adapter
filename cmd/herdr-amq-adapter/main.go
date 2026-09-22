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
	switch act.Kind {
	case adapter.ActionEnsure:
		return ensure(e, act.PaneID)
	case adapter.ActionStop:
		return stop(e, act.PaneID)
	}
	return nil
}

// ensure adopts the agent in paneID: name it if needed, register the handle
// in the shared root, write the pane identity file, and start its waker.
// It is idempotent, so both the hook and reconcile can call it.
func ensure(e env, paneID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	info, ok, err := e.herdr.AgentGet(ctx, paneID)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Printf("pane %s hosts no agent; nothing to adopt\n", paneID)
		return nil
	}
	handle, named := adapter.Handle(info)
	if !named {
		live, err := e.herdr.AgentList(ctx)
		if err != nil {
			return err
		}
		handle = adapter.ChooseHandle(info, adapter.TakenNames(live))
		if err := e.herdr.AgentRename(ctx, paneID, handle); err != nil {
			return err
		}
		fmt.Printf("named pane %s agent %q\n", paneID, handle)
	}
	if cur, exists, err := e.store.Get(paneID); err != nil {
		return err
	} else if exists && cur.Handle == handle && cur.Root == e.root && adapter.Alive(cur.PID) {
		fmt.Printf("pane %s already has waker pid=%d handle=%s\n", paneID, cur.PID, handle)
		return nil
	} else if exists {
		if err := stop(e, paneID); err != nil {
			return err
		}
	}
	if err := adapter.EnsureRoot(ctx, e.amq, e.root, handle); err != nil {
		return err
	}
	if err := adapter.RegisterHandle(e.root, handle); err != nil {
		return err
	}
	id := adapter.Identity{PaneID: paneID, Handle: handle, Root: e.root}
	if err := adapter.WriteIdentity(e.configDir, id); err != nil {
		return err
	}
	rec, err := adapter.Spawn(adapter.WakerSpec{
		AmqBin: e.amq, SelfBin: e.self, LogDir: e.logs, Agent: info, Handle: handle, Root: e.root,
	})
	if err != nil {
		return err
	}
	if err := e.store.Put(rec); err != nil {
		_ = adapter.Terminate(rec.PID)
		return err
	}
	fmt.Printf("adopted pane=%s handle=%s waker pid=%d identity=%s\n",
		paneID, handle, rec.PID, adapter.IdentityPath(e.configDir, paneID))
	return nil
}

func stop(e env, paneID string) error {
	if err := adapter.RemoveIdentity(e.configDir, paneID); err != nil {
		return err
	}
	rec, ok, err := e.store.Get(paneID)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Printf("pane %s has no waker record\n", paneID)
		return nil
	}
	if err := adapter.Terminate(rec.PID); err != nil {
		return fmt.Errorf("terminate pid %d: %w", rec.PID, err)
	}
	if err := e.store.Delete(paneID); err != nil {
		return err
	}
	fmt.Printf("retired pane=%s handle=%s waker pid=%d\n", paneID, rec.Handle, rec.PID)
	return nil
}

func runReconcile() error {
	e, err := loadEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	live, err := e.herdr.AgentList(ctx)
	if err != nil {
		return err
	}
	wakers, err := e.store.List()
	if err != nil {
		return err
	}
	plan := adapter.Plan(live, wakers, adapter.Alive)
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
	fmt.Printf("root: %s\n", e.root)
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PANE\tHANDLE\tPID\tALIVE\tCWD")
	for _, w := range wakers {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%v\t%s\n", w.PaneID, w.Handle, w.PID, adapter.Alive(w.PID), w.Cwd)
	}
	return tw.Flush()
}

// runInject is invoked by amq as `<self> inject <pane_id> <handle> <root> <payload>`.
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
	out, _ := adapter.HerdrFromEnv().Prompt(paneID, text, promptTimeout)
	fmt.Fprintf(os.Stderr, "AMQ_INJECT_PROGRESS=%s\n", out.Progress)
	if out.Code != "" {
		fmt.Fprintf(os.Stderr, "herdr-amq-adapter: inject pane=%s %s %s\n", paneID, out.Code, out.Note)
	}
	return out.ExitCode()
}
