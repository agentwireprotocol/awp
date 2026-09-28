package daemon

import (
	"time"

	"github.com/agentwireprotocol/awp/internal/api"
	"github.com/agentwireprotocol/awp/internal/node"
	"github.com/agentwireprotocol/awp/wire"
)

// presence answers the "presence" call: this agent's live view, plus every
// agent whose signed presence reached us, fresh or stale.
func (d *Daemon) presence() (*api.Network, error) {
	n := d.n
	self, err := n.LocalPresence()
	if err != nil {
		return nil, err
	}
	out := &api.Network{
		Self:   api.AgentView{Presence: *self, Short: wire.ShortKey(self.Origin), Status: api.AgentSelf, Sharing: n.SharesPresence()},
		Agents: []api.AgentView{},
	}
	rows, err := n.Store().PresenceSince(time.Now().Add(-10 * time.Minute))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		p, err := wire.ParsePresence(r.Doc)
		if err != nil {
			continue
		}
		status := api.AgentFresh
		if time.Since(r.TS) > node.PresenceStaleAfter {
			status = api.AgentStale
		}
		out.Agents = append(out.Agents, api.AgentView{
			Presence: *p,
			Short:    wire.ShortKey(p.Origin),
			Status:   status,
			Direct:   n.Connected(p.Origin),
			Received: r.Received,
			Via:      r.Via,
			Hops:     r.Hops,
		})
	}
	return out, nil
}
