// herdr-amq-adapter is a Herdr plugin binary with four entry points:
//
//	hook       — [[events]] handler: attach/stop a waker for the event's pane
//	reconcile  — [[startup]] / action: diff live named agents vs recorded wakers
//	inject     — amq --inject-via target: `herdr agent prompt <pane> <payload>`
//	status     — action: print the waker inventory
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

	"github.com/est9/herdr-amq-adapter/internal/adapter"
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
	fmt.Fprintln(os.Stderr, "usage: herdr-amq-adapter hook|reconcile|status|inject <pane_id> <payload>")
}

type env struct {
	herdr adapter.Herdr
	amq   string
	self  string
	store *adapter.Store
	logs  string
}

func loadEnv() (env, error) {
	store, err := adapter.NewStore(os.Getenv("HERDR_PLUGIN_STATE_DIR"))
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
		herdr: adapter.HerdrFromEnv(),
		amq:   amq,
		self:  self,
		store: store,
		logs:  filepath.Join(os.Getenv("HERDR_PLUGIN_STATE_DIR"), "logs"),
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

func ensure(e env, paneID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, ok, err := e.herdr.AgentGet(ctx, paneID)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Printf("pane %s hosts no agent; nothing to attach\n", paneID)
		return nil
	}
	handle, named := adapter.Handle(info)
	if !named {
		fmt.Printf("pane %s agent is unnamed; skip (name it with `herdr agent rename` to opt in)\n", paneID)
		return nil
	}
	if cur, exists, err := e.store.Get(paneID); err != nil {
		return err
	} else if exists && cur.Handle == handle && adapter.Alive(cur.PID) {
		fmt.Printf("pane %s already has waker pid=%d handle=%s\n", paneID, cur.PID, handle)
		return nil
	} else if exists {
		if err := stop(e, paneID); err != nil {
			return err
		}
	}
	return start(e, info, handle)
}

func start(e env, info adapter.AgentInfo, handle string) error {
	rec, err := adapter.Spawn(adapter.WakerSpec{
		AmqBin: e.amq, SelfBin: e.self, LogDir: e.logs, Agent: info, Handle: handle,
	})
	if err != nil {
		return err
	}
	if err := e.store.Put(rec); err != nil {
		_ = adapter.Terminate(rec.PID)
		return err
	}
	fmt.Printf("started waker pid=%d handle=%s pane=%s cwd=%s\n", rec.PID, handle, info.PaneID, info.Cwd)
	return nil
}

func stop(e env, paneID string) error {
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
	fmt.Printf("stopped waker pid=%d handle=%s pane=%s\n", rec.PID, rec.Handle, paneID)
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
		handle, _ := adapter.Handle(a)
		if err := start(e, a, handle); err != nil {
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
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PANE\tHANDLE\tPID\tALIVE\tCWD")
	for _, w := range wakers {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%v\t%s\n", w.PaneID, w.Handle, w.PID, adapter.Alive(w.PID), w.Cwd)
	}
	return tw.Flush()
}

// runInject is invoked by amq as `<self> inject <pane_id> <payload>`.
// It speaks the AMQ_INJECT_PROGRESS protocol on stderr and never prints
// the payload back.
func runInject(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: herdr-amq-adapter inject <pane_id> <payload>")
		return 2
	}
	paneID, payload := args[0], args[1]
	out, _ := adapter.HerdrFromEnv().Prompt(paneID, payload, promptTimeout)
	fmt.Fprintf(os.Stderr, "AMQ_INJECT_PROGRESS=%s\n", out.Progress)
	if out.Code != "" {
		fmt.Fprintf(os.Stderr, "herdr-amq-adapter: inject pane=%s %s %s\n", paneID, out.Code, out.Note)
	}
	return out.ExitCode()
}
