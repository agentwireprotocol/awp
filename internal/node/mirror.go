package node

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agentwireprotocol/awp/internal/store"
	"github.com/agentwireprotocol/awp/wire"
)

// Conversation sharing (wire/mirror.go): a node configured to share its
// conversations with some hosts mirrors every msg and state of its threads,
// in both directions, to each of them. Its hello and presence say so, so the
// other party of every thread knows, and that party can keep a thread out
// with a private message.

// sharing guards the hosts this node shares with, which change at run time,
// and the hosts with mirror lines waiting for a transaction to commit.
type sharing struct {
	mu   sync.Mutex
	with []string
	wake map[string]bool
}

// mirror queues copies of a line this node just logged (inside the same
// transaction) and remembers which hosts to wake once it commits.
func (n *Node) mirror(q store.Q, peer, th, dir string, line []byte, now time.Time) error {
	woken, err := n.mirrorLine(q, peer, th, dir, line, now)
	if err != nil || len(woken) == 0 {
		return err
	}
	n.share.mu.Lock()
	if n.share.wake == nil {
		n.share.wake = map[string]bool{}
	}
	for _, k := range woken {
		n.share.wake[k] = true
	}
	n.share.mu.Unlock()
	return nil
}

// wakeMirrors kicks the hosts that have new mirror lines. Call it after the
// transaction that queued them commits.
func (n *Node) wakeMirrors() {
	n.share.mu.Lock()
	wake := n.share.wake
	n.share.wake = nil
	n.share.mu.Unlock()
	for k := range wake {
		n.kick(k)
	}
}

// ShareWith returns the keys of the hosts this node mirrors its
// conversations to.
func (n *Node) ShareWith() []string {
	n.share.mu.Lock()
	defer n.share.mu.Unlock()
	return slices.Clone(n.share.with)
}

// SetShareWith sets the hosts to share conversations with (none to stop),
// remembers them, and republishes presence. Peers learn of the change in
// presence at once, and in hello on their next connection.
func (n *Node) SetShareWith(keys []string) error {
	var clean []string
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if _, err := wire.ParseKey(k); err != nil {
			return fmt.Errorf("share with %q: %w", k, err)
		}
		if k == n.key {
			return errors.New("an agent cannot share its conversations with itself")
		}
		if !slices.Contains(clean, k) {
			clean = append(clean, k)
		}
	}
	n.share.mu.Lock()
	n.share.with = clean
	n.share.mu.Unlock()
	if err := n.st.SetKV("share_with", strings.Join(clean, ",")); err != nil {
		return err
	}
	now := time.Now()
	for _, k := range clean {
		if err := n.st.Tx(func(q store.Q) error { return store.EnsurePeer(q, k, now) }); err != nil {
			return err
		}
	}
	n.bump()
	return nil
}

func (n *Node) loadSharing() {
	with := n.cfg.ShareWith
	if with == nil {
		if v, ok, _ := n.st.GetKV("share_with"); ok && v != "" {
			with = strings.Split(v, ",")
		}
	} else {
		n.st.SetKV("share_with", strings.Join(with, ","))
	}
	n.share.with = with
}

// mirrorLine queues a copy of one line of the thread th with peer to every
// host this node shares with, unless the thread is private. dir is "out"
// for a line this node sent, "in" for one it received. It returns the hosts
// to wake once the transaction commits.
func (n *Node) mirrorLine(q store.Q, peer, th, dir string, line []byte, now time.Time) ([]string, error) {
	targets := n.ShareWith()
	if len(targets) == 0 || th == "" {
		return nil, nil
	}
	if private, err := store.IsPrivate(q, peer, th); err != nil || private {
		return nil, err
	}
	var subject, ofName string
	if t, err := store.GetThread(q, peer, th); err == nil && t != nil {
		subject = t.Subject
	}
	if p, err := store.GetPeer(q, peer); err == nil && p != nil {
		ofName = p.Name
	}
	var woken []string
	for _, target := range targets {
		if target == peer {
			continue // the host already has this thread first-hand
		}
		m := wire.Mirror{
			Envelope: wire.Envelope{T: wire.TMirror, ID: n.ids.New(), TS: wire.FormatTime(now), Th: th},
			Of:       peer, OfName: ofName, Subject: subject, Dir: dir, Line: line,
		}
		if err := n.enqueueMirror(q, target, m, now); err != nil {
			return nil, err
		}
		woken = append(woken, target)
	}
	return woken, nil
}

// withdrawMirrors asks every host this node shares with to forget the
// thread th with peer.
func (n *Node) withdrawMirrors(q store.Q, peer, th string, now time.Time) ([]string, error) {
	var woken []string
	for _, target := range n.ShareWith() {
		if target == peer {
			continue
		}
		m := wire.Mirror{Envelope: wire.Envelope{T: wire.TMirror, ID: n.ids.New(), TS: wire.FormatTime(now), Th: th}, Of: peer, Withdraw: true}
		if err := n.enqueueMirror(q, target, m, now); err != nil {
			return nil, err
		}
		woken = append(woken, target)
	}
	return woken, nil
}

// enqueueMirror puts a mirror line in the outbox, reliable like a msg, but
// not in the log: it is a copy, and this node's own history has the original.
func (n *Node) enqueueMirror(q store.Q, target string, m wire.Mirror, now time.Time) error {
	line, err := wire.Encode(m)
	if err != nil {
		return err
	}
	if err := store.EnsurePeer(q, target, now); err != nil {
		return err
	}
	return store.Enqueue(q, &store.OutboxRow{Peer: target, ID: m.ID, T: wire.TMirror, Th: m.Th, Line: line, Created: now})
}

func (n *Node) kickAll(keys []string) {
	for _, k := range keys {
		n.kick(k)
	}
}

// MakePrivate keeps the thread th with peer out of conversation sharing, on
// both sides: this node stops mirroring it (and has its hosts forget it),
// and peer is asked to do the same.
func (n *Node) MakePrivate(peer, th string) error {
	t, err := store.GetThread(n.st.DB(), peer, th)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("no thread %s with %s", th, wire.ShortKey(peer))
	}
	now := time.Now()
	var woken []string
	err = n.st.Tx(func(q store.Q) error {
		if _, err := store.SetPrivate(q, peer, th, now); err != nil {
			return err
		}
		w, err := n.withdrawMirrors(q, peer, th, now)
		if err != nil {
			return err
		}
		woken = w
		p := wire.Private{Envelope: wire.Envelope{T: wire.TPrivate, ID: n.ids.New(), TS: wire.FormatTime(now), Th: th}}
		line, err := wire.Encode(p)
		if err != nil {
			return err
		}
		return store.Enqueue(q, &store.OutboxRow{Peer: peer, ID: p.ID, T: wire.TPrivate, Th: th, Line: line, Created: now})
	})
	if err != nil {
		return err
	}
	n.kick(peer)
	n.kickAll(woken)
	n.bump()
	return nil
}

// onPrivate handles a peer's request to keep a thread out of sharing.
func (n *Node) onPrivate(c *Conn, line []byte) error {
	var p wire.Private
	if err := wire.Decode(line, &p); err != nil || p.ID == "" || p.Th == "" {
		return c.fatal(wire.ErrBadFrame, p.ID, "private needs id and th")
	}
	now := time.Now()
	var fresh bool
	var woken []string
	err := n.st.Tx(func(q store.Q) error {
		var err error
		if fresh, err = store.SetPrivate(q, c.peerKey, p.Th, now); err != nil {
			return err
		}
		if fresh {
			if woken, err = n.withdrawMirrors(q, c.peerKey, p.Th, now); err != nil {
				return err
			}
		}
		return store.BumpSeen(q, c.peerKey, p.Th, p.ID, now)
	})
	if err != nil {
		return n.storageFailed(c, p.ID, err)
	}
	n.ack(c, p.Th, p.ID)
	n.kickAll(woken)
	if fresh {
		n.sysEvent(c.peerKey, "private", map[string]any{"th": p.Th, "sharing": len(n.ShareWith()) > 0})
	}
	return nil
}

// onMirror stores a line another agent shares with this host, or forgets a
// thread it withdraws.
func (n *Node) onMirror(c *Conn, line []byte) error {
	var m wire.Mirror
	if err := wire.Decode(line, &m); err != nil || m.ID == "" || m.Th == "" || m.Of == "" {
		return c.fatal(wire.ErrBadFrame, m.ID, "mirror needs id, th and of")
	}
	now := time.Now()
	changed := false
	err := n.st.Tx(func(q store.Q) error {
		if m.Withdraw {
			k, err := store.DeleteMirrored(q, c.peerKey, m.Th, m.Of)
			if err != nil {
				return err
			}
			changed = k > 0
		} else if orig, err := wire.ParseEnvelope(m.Line); err == nil && orig.ID != "" && (orig.T == wire.TMsg || orig.T == wire.TState) {
			from := c.peerKey
			if m.Dir == "in" {
				from = m.Of
			}
			rec := &store.Record{Peer: c.peerKey, Dir: "mirror", ID: orig.ID, T: orig.T, Th: m.Th, Line: m.Line, At: now,
				Meta: map[string]any{"of": m.Of, "of_name": m.OfName, "from": from, "subject": m.Subject}}
			if changed, err = store.Append(q, rec); err != nil {
				return err
			}
		}
		return store.BumpSeen(q, c.peerKey, m.Th, m.ID, now)
	})
	if err != nil {
		return n.storageFailed(c, m.ID, err)
	}
	n.ack(c, m.Th, m.ID)
	if changed {
		n.bump()
	}
	return nil
}

// noteShares tells this node's agent when a peer shares its conversations
// with other hosts, or stops: the agent's messages to that peer are then
// seen by those hosts. It is recorded only when it changes.
func (n *Node) noteShares(peer string, shares []string) {
	slices.Sort(shares)
	now := strings.Join(shares, ",")
	key := "shares:" + peer
	if before, _, _ := n.st.GetKV(key); before == now {
		return
	}
	n.st.SetKV(key, now)
	var names []string
	for _, k := range shares {
		name := wire.ShortKey(k)
		if p, err := store.GetPeer(n.st.DB(), k); err == nil && p != nil && p.Name != "" {
			name = p.Name
		}
		names = append(names, name)
	}
	n.sysEvent(peer, "shares", map[string]any{"with": shares, "names": names})
}
