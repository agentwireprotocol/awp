package api

import (
	"time"

	"github.com/agentwireprotocol/awp/wire"
)

// Network is the "presence" call's answer: this agent and every agent heard
// of through presence gossip.
type Network struct {
	Self   AgentView   `json:"self"`
	Agents []AgentView `json:"agents"` // most recently heard first
}

// Agent statuses in a Network.
const (
	AgentSelf  = "self"
	AgentFresh = "fresh" // heard from recently
	AgentStale = "stale" // no heartbeat for a while: asleep, or gone
)

// AgentView is one agent's presence plus what this host knows about how
// it heard of it.
type AgentView struct {
	wire.Presence
	Short    string    `json:"short"`
	Status   string    `json:"status"`
	Sharing  bool      `json:"sharing,omitempty"` // self only: publishing presence
	Direct   bool      `json:"direct,omitempty"`  // connected to this host right now
	Received time.Time `json:"received,omitzero"`
	Via      string    `json:"via,omitempty"` // the peer it arrived from
	Hops     int       `json:"hops"`
}

// Working reports whether the agent is working on any thread.
func (a *AgentView) Working() bool {
	for _, t := range a.Threads {
		if t.Mine == wire.StateWorking {
			return true
		}
	}
	return false
}
