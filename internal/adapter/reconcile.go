package adapter

// AgentInfo is the subset of Herdr's agent_info this adapter needs.
type AgentInfo struct {
	PaneID      string  `json:"pane_id"`
	Name        *string `json:"name,omitempty"`
	Agent       *string `json:"agent,omitempty"`
	Cwd         string  `json:"cwd"`
	AgentStatus string  `json:"agent_status"`
}

// Handle returns the agent's Herdr live name, which is its AMQ handle once
// the adapter has adopted it (unnamed agents get a name via ChooseHandle +
// `herdr agent rename` at adoption time).
func Handle(a AgentInfo) (string, bool) {
	if a.Name == nil || *a.Name == "" {
		return "", false
	}
	return *a.Name, true
}

// WakerRecord is the adapter's durable note about one pane's waker. Waker
// identity and liveness are amq's (see wake.go); the record carries what
// amq cannot know: which pane the waker serves and the pane id baked into
// its injector argv.
type WakerRecord struct {
	ServerSocket string `json:"server_socket,omitempty"`
	PaneID       string `json:"pane_id"`
	Handle       string `json:"handle"`
	// SpawnPaneID is the pane id in the waker's --inject-arg list. It stays
	// what it was at spawn time even after `herdr pane move` re-keys PaneID,
	// because amq identifies the waker by that exact argv.
	SpawnPaneID string `json:"spawn_pane_id"`
	Generation  string `json:"generation,omitempty"` // amq wake lock generation; empty while parked
	PID         int    `json:"pid"`                  // informational; 0 while parked
	Cwd         string `json:"cwd"`
	// Agent is the Herdr agent kind (claude, codex, …) at adoption; empty
	// on records written before it was kept.
	Agent       string `json:"agent,omitempty"`
	Root        string `json:"root"`
	StartedUnix int64  `json:"started_unix"`
	// PaneAliases are earlier pane ids of the same occupant (after moves);
	// their identity files are kept alive until the agent goes away.
	PaneAliases []string `json:"pane_aliases,omitempty"`
}

// SameOccupant reports whether the live agent can be the one this record
// was written for. Pane ids restart from w1 after a Herdr server restart, so
// the same id may name another pane: the working directory and agent kind
// must match too (herdr-projects' identity rule). A move keeps both, and an
// unnamed agent that matches is the same one resumed natively without its
// name. A record that did not keep cwd or kind cannot tell and matches.
func (w WakerRecord) SameOccupant(a AgentInfo) bool {
	if w.Cwd != "" && a.Cwd != w.Cwd {
		return false
	}
	return w.Agent == "" || a.Agent == nil || *a.Agent == "" || *a.Agent == w.Agent
}

// Inherit returns the record ensure may carry over (handle, argv pane,
// aliases, start time) to the live agent, and whether there is one. A
// record of an earlier occupant is not inherited: its handle would route
// that agent's mail to the newcomer.
func Inherit(rec WakerRecord, exists bool, live AgentInfo) (WakerRecord, bool) {
	if !exists || !rec.SameOccupant(live) {
		return WakerRecord{}, false
	}
	return rec, true
}

// ArgvPane is the pane id the waker's injector argv names.
func (w WakerRecord) ArgvPane() string {
	if w.SpawnPaneID != "" {
		return w.SpawnPaneID
	}
	return w.PaneID
}

// ReconcilePlan lists the panes to (re)adopt and the records to retire.
type ReconcilePlan struct {
	Start []AgentInfo   // panes whose live agent has no healthy, current waker
	Stop  []WakerRecord // records whose pane hosts no agent any more
}

// Plan diffs live agents against recorded wakers. Every live agent is
// wanted; a pane whose record is current (same occupant, handle matches the
// live name and amq reports a live waker with the wanted target) is left alone, any other
// live pane goes to Start, where ensure decides what to replace and keeps
// the pane's previous handle. Only a record whose pane no longer hosts an
// agent is retired outright.
func Plan(live []AgentInfo, wakers []WakerRecord, current func(WakerRecord) bool) ReconcilePlan {
	want := map[string]AgentInfo{}
	for _, a := range live {
		want[a.PaneID] = a
	}
	covered := map[string]bool{}
	var plan ReconcilePlan
	for _, w := range wakers {
		a, wanted := want[w.PaneID]
		if !wanted {
			plan.Stop = append(plan.Stop, w)
			continue
		}
		name, named := Handle(a)
		// A record without a generation (parked, or written before
		// generations were recorded) cannot prove ownership and is not
		// advertised to peers; ensure refreshes it even when the waker is
		// current.
		if (!named || name == w.Handle) && w.SameOccupant(a) && w.Generation != "" && current(w) {
			covered[w.PaneID] = true
		}
	}
	for _, a := range live {
		if !covered[a.PaneID] {
			plan.Start = append(plan.Start, a)
		}
	}
	return plan
}

// LiveHandles are the handles whose records currently own a waker; parked
// records (agent released) are excluded, so peers are never told about an
// agent that cannot read its mail.
func LiveHandles(recs []WakerRecord) []string {
	var out []string
	for _, r := range recs {
		if r.Generation != "" {
			out = append(out, r.Handle)
		}
	}
	return out
}

// AdoptHandle is the handle an unnamed agent in a pane is given: the
// record's when it is the same occupant (Inherit), otherwise a kind-based
// name. An earlier occupant's handle, and a reserved handle (a departed
// agent's, within its grace period), are never offered to a newcomer, even
// when kind-based naming would land on them: their mailbox holds mail for
// that agent.
func AdoptHandle(a AgentInfo, live []AgentInfo, rec WakerRecord, exists bool, reserved []string) string {
	prev, inherited := Inherit(rec, exists, a)
	taken := TakenNames(live)
	if exists && !inherited {
		taken[rec.Handle] = true
	}
	for _, h := range reserved {
		// The confirmed same occupant keeps its handle even when a stop that
		// died halfway left a tombstone for it.
		if inherited && h == prev.Handle {
			continue
		}
		taken[h] = true
	}
	return ChooseHandle(a, taken, prev.Handle)
}
