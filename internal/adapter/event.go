// Package adapter is the functional core of the Herdr AMQ adapter: pure
// decisions over Herdr events and agent inventories, plus the thin shell
// that runs amq/herdr subprocesses.
package adapter

import (
	"encoding/json"
	"fmt"
)

// Event mirrors Herdr's EventEnvelope as delivered in HERDR_PLUGIN_EVENT_JSON.
type Event struct {
	Event string    `json:"event"`
	Data  EventData `json:"data"`
}

// EventData is the union of the pane event payloads this adapter reacts to.
type EventData struct {
	PaneID      string  `json:"pane_id"`
	WorkspaceID string  `json:"workspace_id"`
	Agent       *string `json:"agent,omitempty"`
	Released    bool    `json:"released,omitempty"`
	FinalStatus *string `json:"final_status,omitempty"`
	AgentStatus *string `json:"agent_status,omitempty"`
}

// ParseEvent decodes the envelope; an empty input is a usage error, not a
// silently ignored event.
func ParseEvent(raw string) (Event, error) {
	if raw == "" {
		return Event{}, fmt.Errorf("HERDR_PLUGIN_EVENT_JSON is empty")
	}
	var ev Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		return Event{}, fmt.Errorf("decode event: %w", err)
	}
	if ev.Data.PaneID == "" {
		return Event{}, fmt.Errorf("event %q has no pane_id", ev.Event)
	}
	return ev, nil
}

// ActionKind is what the shell must do for one event.
type ActionKind int

const (
	ActionNone ActionKind = iota
	// ActionEnsure attaches a waker to the pane if it hosts a named agent.
	ActionEnsure
	// ActionStop kills and forgets the pane's waker if one exists.
	ActionStop
)

func (k ActionKind) String() string {
	switch k {
	case ActionEnsure:
		return "ensure"
	case ActionStop:
		return "stop"
	default:
		return "none"
	}
}

// Action is the decision for one event.
type Action struct {
	Kind   ActionKind
	PaneID string
	Reason string
}

// Decide maps a Herdr event to an adapter action. Only agent appearance and
// pane/agent departure matter; status changes are left to amq's own retry loop.
func Decide(ev Event) Action {
	switch ev.Event {
	case "pane.agent_detected":
		if ev.Data.Released {
			return Action{ActionStop, ev.Data.PaneID, "agent released"}
		}
		return Action{ActionEnsure, ev.Data.PaneID, "agent detected"}
	case "pane.closed":
		return Action{ActionStop, ev.Data.PaneID, "pane closed"}
	case "pane.exited":
		return Action{ActionStop, ev.Data.PaneID, "pane process exited"}
	default:
		return Action{ActionNone, ev.Data.PaneID, "unhandled event " + ev.Event}
	}
}
