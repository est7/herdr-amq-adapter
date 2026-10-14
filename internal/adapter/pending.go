package adapter

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// PendingMail counts undrained messages in handle's inbox under root
// (amq's Maildir layout: <root>/agents/<handle>/inbox/new/*.md). A missing
// inbox is zero, not an error. Only regular files count: the adapter never
// follows links it did not create.
func PendingMail(root, handle string) (int, error) {
	dir := filepath.Join(root, "agents", handle, "inbox", "new")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".md") {
			n++
		}
	}
	return n, nil
}

// KickVerdict is the pure decision behind a status-change kick.
type KickVerdict struct {
	Record WakerRecord
	Kick   bool
	Reason string
}

// DecideKick says whether a status change in paneID should re-deliver a
// doorbell. It fails closed: the registry must belong to socket (the same
// single-session ownership rule lifecycle changes enforce), the pane's
// record must be bound to that server and live, and amq must be sitting on
// a deferred cohort for the handle (last inject outcome deferred) with mail
// still undrained. Kicking only after a deferral is what keeps repeated
// status changes from re-ringing for mail an earlier doorbell announced:
// an accepted kick or amq retry records "accepted", which ends the window.
func DecideKick(recs []WakerRecord, socket, paneID string, last InjectEntry, haveLast bool, pending int) KickVerdict {
	if err := CheckServer(recs, socket); err != nil {
		return KickVerdict{Reason: "server ownership: " + err.Error()}
	}
	var rec WakerRecord
	found := false
	for _, r := range recs {
		if r.PaneID == paneID {
			rec, found = r, true
			break
		}
	}
	switch {
	case !found:
		return KickVerdict{Reason: "no waker record"}
	case rec.PID == 0:
		return KickVerdict{Record: rec, Reason: "parked"}
	case rec.ServerSocket == "" || canonPath(rec.ServerSocket) != canonPath(socket):
		return KickVerdict{Record: rec, Reason: "record not bound to this server"}
	case !haveLast || last.Progress != ProgressDeferred:
		return KickVerdict{Record: rec, Reason: "no deferred delivery pending"}
	case pending == 0:
		return KickVerdict{Record: rec, Reason: "inbox empty"}
	}
	return KickVerdict{Record: rec, Kick: true, Reason: "deferred delivery with undrained mail"}
}

// kickLock serialises status-change kicks for one handle across hook
// processes (Herdr runs each event's hook as its own process), so two
// concurrent kicks cannot both read the same deferred outcome and deliver
// twice. It is per handle and never the lifecycle lock. The lock file is
// adapter-created; O_NOFOLLOW refuses a link planted at its path.
func kickLock(stateDir, handle string) (func(), error) {
	logPath, err := injectLogPath(stateDir, handle)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(strings.TrimSuffix(logPath, ".jsonl")+".kick.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open kick lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("acquire kick lock: %w", err)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// KickOnce is the whole status-change kick for handle as one critical
// section: under the handle's kick lock it reloads the records, re-reads
// the last inject outcome and the inbox, applies DecideKick, delivers, and
// records the outcome before releasing. The record is what closes the
// window for the next kick, so it must land inside the lock.
func KickOnce(stateDir, handle, socket, paneID string, load func() ([]WakerRecord, error), deliver func(WakerRecord) Outcome, now func() time.Time) (KickVerdict, Outcome, error) {
	unlock, err := kickLock(stateDir, handle)
	if err != nil {
		return KickVerdict{}, Outcome{}, err
	}
	defer unlock()
	recs, err := load()
	if err != nil {
		return KickVerdict{}, Outcome{}, err
	}
	root := ""
	for _, r := range recs {
		if r.PaneID == paneID {
			root = r.Root
		}
	}
	last, haveLast, err := LastInject(stateDir, handle)
	if err != nil {
		return KickVerdict{}, Outcome{}, err
	}
	pending := 0
	if root != "" {
		if pending, err = PendingMail(root, handle); err != nil {
			return KickVerdict{}, Outcome{}, err
		}
	}
	v := DecideKick(recs, socket, paneID, last, haveLast, pending)
	if !v.Kick || v.Record.Handle != handle {
		return v, Outcome{}, nil
	}
	out := deliver(v.Record)
	if out.Note == "" {
		out.Note = "kick after status change"
	} else {
		out.Note = "kick: " + out.Note
	}
	if err := RecordInject(stateDir, handle, out, now()); err != nil {
		return v, out, err
	}
	return v, out, nil
}
