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

// WakerRecord is the durable record of one spawned `amq wake` process.
type WakerRecord struct {
	PaneID string `json:"pane_id"`
	Handle string `json:"handle"`
	PID    int    `json:"pid"` // 0 while parked (agent released, pane still open)
	Cwd    string `json:"cwd"`
	Root   string `json:"root"`
	// SelfBin is the adapter binary the waker was given as --inject-via. A
	// plugin update installs a new binary under a new path; a waker still
	// pointing at the old one must be restarted.
	SelfBin     string `json:"self_bin"`
	StartedUnix int64  `json:"started_unix"`
	// PaneAliases are earlier pane ids of the same occupant (after moves);
	// their identity files are kept alive until the agent goes away.
	PaneAliases []string `json:"pane_aliases,omitempty"`
}

// ReconcilePlan lists the panes to (re)adopt and the records to retire.
type ReconcilePlan struct {
	Start []AgentInfo   // panes whose live agent has no healthy, current waker
	Stop  []WakerRecord // records whose pane hosts no agent any more
}

// Plan diffs live agents against recorded wakers. Every live agent is
// wanted; a pane whose record is healthy (waker alive, handle matches the
// live name, --inject-via is the current binary) is left alone, any other
// live pane goes to Start, where ensure decides what to replace and keeps
// the pane's previous handle. Only a record whose pane no longer hosts an
// agent is retired outright.
func Plan(live []AgentInfo, wakers []WakerRecord, alive func(WakerRecord) bool, selfBin string) ReconcilePlan {
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
		if (!named || name == w.Handle) && w.SelfBin == selfBin && alive(w) {
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
