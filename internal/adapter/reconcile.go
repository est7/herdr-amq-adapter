package adapter

// AgentInfo is the subset of Herdr's agent_info this adapter needs.
type AgentInfo struct {
	PaneID      string  `json:"pane_id"`
	Name        *string `json:"name,omitempty"`
	Agent       *string `json:"agent,omitempty"`
	Cwd         string  `json:"cwd"`
	AgentStatus string  `json:"agent_status"`
}

// Handle returns the AMQ handle for an agent: its Herdr live name.
// Unnamed agents are not adopted — naming the agent is the opt-in, and the
// name doubles as AM_ME so the agent and its waker agree on identity.
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
	StartedUnix int64  `json:"started_unix"`
}

// ReconcilePlan lists the wakers to start and the records to stop/forget.
type ReconcilePlan struct {
	Start []AgentInfo
	Stop  []WakerRecord
}

// Plan diffs live agents against recorded wakers. A record is stale when its
// pane no longer hosts a named agent, when the handle changed, or when the
// process is dead (then it is forgotten and, if still eligible, restarted).
func Plan(live []AgentInfo, wakers []WakerRecord, alive func(pid int) bool) ReconcilePlan {
	want := map[string]AgentInfo{}
	for _, a := range live {
		if _, ok := Handle(a); ok {
			want[a.PaneID] = a
		}
	}
	covered := map[string]bool{}
	var plan ReconcilePlan
	for _, w := range wakers {
		a, wanted := want[w.PaneID]
		handle, _ := Handle(a)
		switch {
		case !wanted:
			plan.Stop = append(plan.Stop, w)
		case handle != w.Handle:
			plan.Stop = append(plan.Stop, w)
		case !alive(w.PID):
			plan.Stop = append(plan.Stop, w) // forget the dead record; restart below
		default:
			covered[w.PaneID] = true
		}
	}
	for _, a := range live {
		if _, ok := want[a.PaneID]; ok && !covered[a.PaneID] {
			plan.Start = append(plan.Start, a)
		}
	}
	return plan
}
