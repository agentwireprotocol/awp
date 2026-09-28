package node

import (
	"testing"
	"time"

	"github.com/agentwireprotocol/awp/wire"
)

// Activity reaches the network in presence: coarse, and with waiting.
func TestActivityInPresence(t *testing.T) {
	share := func(c *Config) { c.Presence = true }
	a := testNode(t, "a", share)
	b := testNode(t, "b")
	connect(t, a, b)

	if last, waiting := a.Activity(); !last.IsZero() || waiting {
		t.Fatalf("a fresh node reports activity: %v %v", last, waiting)
	}
	p, err := a.LocalPresence()
	if err != nil || p.Active != "" {
		t.Fatalf("presence before any activity: %q %v", p.Active, err)
	}

	a.Touch()
	done := a.Waiting()
	p, _ = a.LocalPresence()
	at, err := wire.ParseTime(p.Active)
	if err != nil || !p.Waiting || time.Since(at) > activityGrain+time.Second || at.Truncate(activityGrain) != at {
		t.Errorf("presence while waiting: active %q (%v) waiting %v", p.Active, err, p.Waiting)
	}
	waitFor(t, b, "a's waiting at b", func() bool {
		row := presenceOf(t, b, a.Key())
		if row == nil {
			return false
		}
		doc, err := wire.ParsePresence(row.Doc)
		return err == nil && doc.Waiting && doc.Active != ""
	})
	done()
	done() // idempotent
	if _, waiting := a.Activity(); waiting {
		t.Error("still waiting after done")
	}
}

// A connection's round trip time is measured from its first ping, and
// presence carries it per peer.
func TestRoundTripTime(t *testing.T) {
	a := testNode(t, "a", func(c *Config) { c.Presence = true })
	b := testNode(t, "b")
	connect(t, a, b)
	waitFor(t, a, "a round trip to b", func() bool {
		ci := a.ConnInfo(b.Key())
		return ci != nil && ci.RTT > 0
	})
	p, err := a.LocalPresence()
	if err != nil {
		t.Fatal(err)
	}
	for _, pp := range p.Peers {
		if pp.Key == b.Key() && (pp.RTT < 1 || !pp.Up) {
			t.Errorf("b in a's presence: %+v", pp)
		}
	}
}
