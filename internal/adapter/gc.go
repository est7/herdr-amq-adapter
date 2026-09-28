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
	"strconv"
	"strings"
	"time"
)

// A departed agent's mailbox outlives it in the shared root. Without care
// the next unnamed agent of the same kind is given that handle and reads
// the backlog. When a pane's agent goes away the adapter leaves a tombstone:
// for a grace period the handle stays reserved (the agent may come back
// under its name, e.g. Herdr resuming it once a client attaches), and after
// it the mailbox is moved to an archive and dropped from the amq agent list.
// Archives are removed after a retention period.

// Tombstone marks a handle whose agent left its pane.
type Tombstone struct {
	Handle       string `json:"handle"`
	DepartedUnix int64  `json:"departed_unix"`
	Cwd          string `json:"cwd,omitempty"`
	Agent        string `json:"agent,omitempty"`
}

// Archive is one archived mailbox, `<state>/archive/<handle>-<unix>`.
type Archive struct {
	Dir          string
	ArchivedUnix int64
}

// GCPlan is what one pass does. Reserved are the handles still in their
// grace period, which naming must not hand to a newcomer.
type GCPlan struct {
	Archive  []string // expired, unused handles whose mailbox is archived
	Forget   []string // expired tombstones whose handle is in use again
	Purge    []string // archive dirs past retention
	Reserved []string
}

func (p GCPlan) Empty() bool {
	return len(p.Archive) == 0 && len(p.Forget) == 0 && len(p.Purge) == 0
}

// PlanGC decides a pass from tombstones, live agents, waker records and
// archives. It never plans to touch a handle a live agent or a record holds.
func PlanGC(tombs []Tombstone, live []AgentInfo, recs []WakerRecord, archives []Archive, now time.Time, grace, keep time.Duration) GCPlan {
	inUse := TakenNames(live)
	for _, r := range recs {
		inUse[r.Handle] = true
	}
	var p GCPlan
	for _, t := range tombs {
		switch {
		case now.Sub(time.Unix(t.DepartedUnix, 0)) < grace:
			p.Reserved = append(p.Reserved, t.Handle)
		case inUse[t.Handle]:
			p.Forget = append(p.Forget, t.Handle)
		default:
			p.Archive = append(p.Archive, t.Handle)
		}
	}
	for _, a := range archives {
		if now.Sub(time.Unix(a.ArchivedUnix, 0)) >= keep {
			p.Purge = append(p.Purge, a.Dir)
		}
	}
	return p
}

// RunGC carries out a plan. Each step is idempotent, so a pass that died
// halfway is finished by the next one.
func RunGC(ctx context.Context, amqBin, root string, s *Store, plan GCPlan, now time.Time) error {
	var errs []error
	for _, h := range plan.Archive {
		if err := archiveMailbox(ctx, amqBin, root, s.archiveDir(), h, now); err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, s.DeleteTombstone(h))
	}
	for _, h := range plan.Forget {
		errs = append(errs, s.DeleteTombstone(h))
	}
	for _, d := range plan.Purge {
		if strings.ContainsAny(d, `/\`) || d == "." || d == ".." {
			errs = append(errs, fmt.Errorf("gc: refusing to purge %q", d))
			continue
		}
		errs = append(errs, os.RemoveAll(filepath.Join(s.archiveDir(), d)))
	}
	return errors.Join(errs...)
}

// archiveMailbox moves handle's mailbox out of the root and drops it from
// the amq agent list, under the registry lock every list writer takes.
func archiveMailbox(ctx context.Context, amqBin, root, archiveDir, handle string, now time.Time) error {
	if !herdrNameRe.MatchString(handle) {
		return fmt.Errorf("gc: invalid handle %q", handle)
	}
	unlock, err := lockMailboxRegistry(ctx, root)
	if err != nil {
		return err
	}
	defer unlock()
	src := filepath.Join(root, "agents", handle)
	if _, err := os.Stat(src); err == nil {
		if err := os.MkdirAll(archiveDir, 0o755); err != nil {
			return err
		}
		dst := filepath.Join(archiveDir, fmt.Sprintf("%s-%d", handle, now.Unix()))
		for i := 2; ; i++ {
			if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
				break
			}
			dst = filepath.Join(archiveDir, fmt.Sprintf("%s-%d.%d", handle, now.Unix(), i))
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("gc: archive %s: %w", handle, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cfgPath := filepath.Join(root, "meta", "config.json")
	b, err := os.ReadFile(cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var cfg struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("decode amq config: %w", err)
	}
	var kept []string
	for _, a := range cfg.Agents {
		if a != handle {
			kept = append(kept, a)
		}
	}
	if len(kept) == len(cfg.Agents) || len(kept) == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, amqBin, "init", "--root", root, "--agents", strings.Join(kept, ","), "--force")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("amq init --force %s: %w: %s", root, err, out.String())
	}
	return nil
}

func (s *Store) tombstoneDir() string { return filepath.Join(filepath.Dir(s.dir), "tombstones") }
func (s *Store) archiveDir() string   { return filepath.Join(filepath.Dir(s.dir), "archive") }

func (s *Store) tombstonePath(handle string) (string, error) {
	if !herdrNameRe.MatchString(handle) {
		return "", fmt.Errorf("tombstone: invalid handle %q", handle)
	}
	return filepath.Join(s.tombstoneDir(), handle+".json"), nil
}

func (s *Store) PutTombstone(t Tombstone) error {
	p, err := s.tombstonePath(t.Handle)
	if err != nil {
		return err
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (s *Store) DeleteTombstone(handle string) error {
	p, err := s.tombstonePath(handle)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) Tombstones() ([]Tombstone, error) {
	entries, err := os.ReadDir(s.tombstoneDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Tombstone
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.tombstoneDir(), e.Name()))
		if err != nil {
			return nil, err
		}
		var t Tombstone
		if err := json.Unmarshal(b, &t); err != nil {
			return nil, fmt.Errorf("corrupt tombstone %s: %w", e.Name(), err)
		}
		out = append(out, t)
	}
	return out, nil
}

// Archives lists archived mailboxes; a dir whose name carries no time is
// not one this package made and is left alone.
func (s *Store) Archives() ([]Archive, error) {
	entries, err := os.ReadDir(s.archiveDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Archive
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		stamp := name[strings.LastIndexByte(name, '-')+1:]
		if i := strings.IndexByte(stamp, '.'); i >= 0 {
			stamp = stamp[:i]
		}
		unix, err := strconv.ParseInt(stamp, 10, 64)
		if err != nil || !strings.Contains(name, "-") {
			continue
		}
		out = append(out, Archive{Dir: name, ArchivedUnix: unix})
	}
	return out, nil
}
