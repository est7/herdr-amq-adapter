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
	PaneID      string `json:"pane_id"`
	Handle      string `json:"handle"`
	PID         int    `json:"pid"`
	Cwd         string `json:"cwd"`
	Root        string `json:"root"`
	StartedUnix int64  `json:"started_unix"`
}

// ReconcilePlan lists the wakers to start and the records to stop/forget.
type ReconcilePlan struct {
	Start []AgentInfo
	Stop  []WakerRecord
}

// Plan diffs live agents against recorded wakers. Every live agent is
// wanted (unnamed ones get named at start). A record is stale when its pane
// no longer hosts an agent, when a named agent's name differs from the
// recorded handle, or when the process is dead (then it is forgotten and
// restarted).
func Plan(live []AgentInfo, wakers []WakerRecord, alive func(pid int) bool) ReconcilePlan {
	want := map[string]AgentInfo{}
	for _, a := range live {
		want[a.PaneID] = a
	}
	covered := map[string]bool{}
	var plan ReconcilePlan
	for _, w := range wakers {
		a, wanted := want[w.PaneID]
		name, named := Handle(a)
		switch {
		case !wanted:
			plan.Stop = append(plan.Stop, w)
		case named && name != w.Handle:
			plan.Stop = append(plan.Stop, w)
		case !alive(w.PID):
			plan.Stop = append(plan.Stop, w) // forget the dead record; restart below
		default:
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
