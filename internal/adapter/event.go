// Package adapter is the functional core of the Herdr AMQ adapter: pure
// decisions over Herdr events and agent inventories, plus the thin shell
// that runs amq/herdr subprocesses.
package adapter

import (
	"encoding/json"
	"fmt"
	"strings"
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
	// pane.moved carries the new pane nested and the old id alongside.
	PreviousPaneID string `json:"previous_pane_id,omitempty"`
	Pane           *struct {
		PaneID string `json:"pane_id"`
	} `json:"pane,omitempty"`
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
	if ev.Data.PaneID == "" && ev.Data.Pane != nil {
		ev.Data.PaneID = ev.Data.Pane.PaneID
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
	// ActionMove re-keys the waker record from PreviousPaneID to PaneID and
	// writes an identity file for the new id (the old one stays: the moved
	// process still sees its original HERDR_PANE_ID).
	ActionMove
)

func (k ActionKind) String() string {
	switch k {
	case ActionEnsure:
		return "ensure"
	case ActionStop:
		return "stop"
	case ActionMove:
		return "move"
	default:
		return "none"
	}
}

// Action is the decision for one event.
type Action struct {
	Kind           ActionKind
	PaneID         string
	PreviousPaneID string // ActionMove only
	Reason         string
}

// Decide maps a Herdr event to an adapter action. Only agent appearance and
// pane/agent departure matter; status changes are left to amq's own retry loop.
//
// Herdr serialises EventKind in the envelope as snake_case ("pane_exited")
// while manifests and HERDR_PLUGIN_EVENT use the dotted form ("pane.exited");
// both are accepted.
func Decide(ev Event) Action {
	switch strings.ReplaceAll(ev.Event, "_", ".") {
	case "pane.agent.detected", "pane.agent_detected":
		if ev.Data.Released {
			return Action{ActionStop, ev.Data.PaneID, "", "agent released"}
		}
		return Action{ActionEnsure, ev.Data.PaneID, "", "agent detected"}
	case "pane.closed":
		return Action{ActionStop, ev.Data.PaneID, "", "pane closed"}
	case "pane.exited":
		return Action{ActionStop, ev.Data.PaneID, "", "pane process exited"}
	case "pane.moved":
		if ev.Data.PreviousPaneID == "" || ev.Data.PreviousPaneID == ev.Data.PaneID {
			return Action{ActionNone, ev.Data.PaneID, "", "move without id change"}
		}
		return Action{ActionMove, ev.Data.PaneID, ev.Data.PreviousPaneID, "pane moved"}
	default:
		return Action{ActionNone, ev.Data.PaneID, "", "unhandled event " + ev.Event}
	}
}
