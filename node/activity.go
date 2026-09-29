package node

import (
	"sync"
	"time"
)

// Activity: when this node's agent last did something through awp (a
// hook ran, it sent, set a state, read its inbox...), and whether it is
// blocked waiting for a message. Presence carries both, coarsely, so a
// dashboard can tell "working" from "said working, then went quiet".
// Reading by dashboards is not activity.

// activityGrain is how coarsely presence reports the last activity, so an
// agent's every tool call does not change its presence document.
const activityGrain = 30 * time.Second

type activity struct {
	mu      sync.Mutex
	last    time.Time
	waiting int
}

// Touch records that the agent just acted.
func (n *Node) Touch() {
	n.act.mu.Lock()
	n.act.last = time.Now()
	n.act.mu.Unlock()
}

// Waiting marks the agent as blocked waiting for a message until the
// returned function is called.
func (n *Node) Waiting() (done func()) {
	n.act.mu.Lock()
	n.act.waiting++
	n.act.last = time.Now()
	n.act.mu.Unlock()
	n.bump()
	var once sync.Once
	return func() {
		once.Do(func() {
			n.act.mu.Lock()
			n.act.waiting--
			n.act.last = time.Now()
			n.act.mu.Unlock()
			n.bump()
		})
	}
}

// Activity reports when the agent last acted (zero if not since the daemon
// started) and whether it is waiting for a message now.
func (n *Node) Activity() (last time.Time, waiting bool) {
	n.act.mu.Lock()
	defer n.act.mu.Unlock()
	return n.act.last, n.act.waiting > 0
}
