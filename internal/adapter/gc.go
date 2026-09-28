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

// archiveMarker is written into every mailbox this package archives. Only a
// dir carrying it is an archive: anything else in the archive dir is left
// alone, whatever its name.
const archiveMarker = ".amq-adapter-archive"

type archiveMark struct {
	Handle       string `json:"handle"`
	ArchivedUnix int64  `json:"archived_unix"`
}

// ReservedHandles are the handles an unnamed newcomer in paneID must not be
// given: departed agents' (tombstones) and those other panes' records still
// hold, whose stop may simply not have run yet (hooks arrive in any order).
func ReservedHandles(tombs []Tombstone, recs []WakerRecord, paneID string) []string {
	var out []string
	for _, t := range tombs {
		out = append(out, t.Handle)
	}
	for _, r := range recs {
		if r.PaneID != paneID {
			out = append(out, r.Handle)
		}
	}
	return out
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
// halfway is finished by the next one. A tombstone is forgotten only once
// its handle is off the amq agent list.
func RunGC(ctx context.Context, amqBin, root string, s *Store, plan GCPlan, now time.Time) error {
	var errs []error
	for _, h := range plan.Archive {
		done, err := s.archiveMailbox(ctx, amqBin, root, h, now)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if done {
			errs = append(errs, s.DeleteTombstone(h))
		}
	}
	for _, h := range plan.Forget {
		errs = append(errs, s.DeleteTombstone(h))
	}
	for _, d := range plan.Purge {
		errs = append(errs, s.purgeArchive(d))
	}
	return errors.Join(errs...)
}

// purgeArchive removes one archive, only when it is a real dir (not a
// link) inside a real archive dir and carries this package's marker.
func (s *Store) purgeArchive(name string) error {
	base, err := s.archiveBase()
	if err != nil {
		return err
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("gc: refusing to purge %q", name)
	}
	dir := filepath.Join(base, name)
	if !isRealDir(dir) || readMark(dir) == nil {
		return fmt.Errorf("gc: refusing to purge %s: not an archive this adapter made", dir)
	}
	return os.RemoveAll(dir)
}

// archiveBase is the archive dir; a link there is refused, so nothing is
// ever listed or removed through it.
func (s *Store) archiveBase() (string, error) {
	base := s.archiveDir()
	st, err := os.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		return base, nil
	}
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("gc: %s is not a plain directory; refusing to use it", base)
	}
	return base, nil
}

// archiveTarget is a free `<handle>-<unix>[.n]` name in archiveDir. Only an
// existing name moves on to the next; any other error is returned.
func archiveTarget(ctx context.Context, archiveDir, handle string, now time.Time) (string, error) {
	for i := 1; i <= 100; i++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		name := fmt.Sprintf("%s-%d", handle, now.Unix())
		if i > 1 {
			name = fmt.Sprintf("%s.%d", name, i)
		}
		dst := filepath.Join(archiveDir, name)
		_, err := os.Lstat(dst)
		if errors.Is(err, os.ErrNotExist) {
			return dst, nil
		}
		if err != nil {
			return "", fmt.Errorf("gc: archive target %s: %w", dst, err)
		}
	}
	return "", fmt.Errorf("gc: no free archive name for %s", handle)
}

// writeMark creates dir's marker exclusively, so it never writes through a
// link. An existing entry under that name is replaced only when it is a
// link (removing a link removes the link, not its target) or this
// adapter's marker for the same handle (an interrupted earlier pass);
// anything else is kept and the archive fails.
func writeMark(dir string, m archiveMark) error {
	p := filepath.Join(dir, archiveMarker)
	st, err := os.Lstat(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	case st.Mode()&os.ModeSymlink != 0:
		if err := os.Remove(p); err != nil {
			return err
		}
	default:
		if old := readMark(dir); old == nil || old.Handle != m.Handle {
			return fmt.Errorf("%s exists and is not this adapter's marker for %s; leaving it", p, m.Handle)
		}
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

func isRealDir(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.IsDir()
}

func readMark(dir string) *archiveMark {
	p := filepath.Join(dir, archiveMarker)
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var m archiveMark
	if json.Unmarshal(b, &m) != nil || m.ArchivedUnix == 0 {
		return nil
	}
	return &m
}

// archiveMailbox moves handle's mailbox out of the root and drops it from
// the amq agent list, under the registry lock every list writer takes.
// done is false while the list cannot drop it yet: amq refuses an empty
// list, so the last agent stays listed until another one joins.
func (s *Store) archiveMailbox(ctx context.Context, amqBin, root, handle string, now time.Time) (done bool, err error) {
	if !herdrNameRe.MatchString(handle) {
		return false, fmt.Errorf("gc: invalid handle %q", handle)
	}
	archiveDir, err := s.archiveBase()
	if err != nil {
		return false, err
	}
	unlock, err := lockMailboxRegistry(ctx, root)
	if err != nil {
		return false, err
	}
	defer unlock()
	src := filepath.Join(root, "agents", handle)
	if isRealDir(src) {
		if err := os.MkdirAll(archiveDir, 0o755); err != nil {
			return false, err
		}
		// Mark, then move: whatever lands in the archive carries the marker,
		// and a pass that died in between is redone by the next one.
		if err := writeMark(src, archiveMark{Handle: handle, ArchivedUnix: now.Unix()}); err != nil {
			return false, fmt.Errorf("gc: mark %s: %w", src, err)
		}
		dst, err := archiveTarget(ctx, archiveDir, handle, now)
		if err != nil {
			return false, err
		}
		if err := os.Rename(src, dst); err != nil {
			return false, fmt.Errorf("gc: archive %s: %w", handle, err)
		}
	}
	cfgPath := filepath.Join(root, "meta", "config.json")
	b, err := os.ReadFile(cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var cfg struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return false, fmt.Errorf("decode amq config: %w", err)
	}
	var kept []string
	for _, a := range cfg.Agents {
		if a != handle {
			kept = append(kept, a)
		}
	}
	switch {
	case len(kept) == len(cfg.Agents):
		return true, nil
	case len(kept) == 0:
		return false, nil
	}
	cmd := exec.CommandContext(ctx, amqBin, "init", "--root", root, "--agents", strings.Join(kept, ","), "--force")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("amq init --force %s: %w: %s", root, err, out.String())
	}
	return true, nil
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
	// A fresh, exclusively created temp file: nothing already in the dir is
	// ever written through.
	f, err := os.CreateTemp(filepath.Dir(p), ".tombstone-*")
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(f.Name(), 0o644)
	}
	if werr == nil {
		werr = os.Rename(f.Name(), p)
	}
	if werr != nil {
		_ = os.Remove(f.Name())
	}
	return werr
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
		// A tombstone leads to archiving a mailbox, so it must be exactly
		// what PutTombstone writes: a regular file named after its handle.
		p := filepath.Join(s.tombstoneDir(), e.Name())
		if st, err := os.Lstat(p); err != nil || !st.Mode().IsRegular() {
			return nil, fmt.Errorf("tombstone %s is not a regular file", p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var t Tombstone
		if err := json.Unmarshal(b, &t); err != nil {
			return nil, fmt.Errorf("corrupt tombstone %s: %w", e.Name(), err)
		}
		if e.Name() != t.Handle+".json" || !herdrNameRe.MatchString(t.Handle) {
			return nil, fmt.Errorf("tombstone %s names handle %q", p, t.Handle)
		}
		out = append(out, t)
	}
	return out, nil
}

// Archives lists the archives this package made: real dirs carrying its
// marker, dated by the marker. Anything else is not an archive.
func (s *Store) Archives() ([]Archive, error) {
	base, err := s.archiveBase()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Archive
	for _, e := range entries {
		dir := filepath.Join(base, e.Name())
		if !isRealDir(dir) {
			continue
		}
		if m := readMark(dir); m != nil {
			out = append(out, Archive{Dir: e.Name(), ArchivedUnix: m.ArchivedUnix})
		}
	}
	return out, nil
}
