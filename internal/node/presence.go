package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/hollerprotocol/holler/internal/store"
	"github.com/hollerprotocol/holler/internal/version"
	"github.com/hollerprotocol/holler/wire"
)

// Presence gossip: an opt-in, signed summary of what each agent is doing,
// forwarded across the network so that a watcher on any connected host can
// see all of it (wire/presence.go, NOTES.md "Presence").
const (
	presenceHeartbeat  = 60 * time.Second
	presenceDebounce   = 2 * time.Second
	presenceMaxAge     = 10 * time.Minute  // older documents are dropped
	PresenceStaleAfter = 150 * time.Second // no heartbeat for this long: stale
	presenceFuture     = 5 * time.Minute   // tolerated clock skew
	presenceMaxOrigins = 1000
	presenceMaxThreads = 64
	presenceMaxPeers   = 64
)

// SharesPresence reports whether this node publishes its own presence.
func (n *Node) SharesPresence() bool { return n.cfg.Presence }

// LocalPresence describes this node's current activity. It is what the
// node would publish, minus the signature, and is available whether or not
// sharing is on.
func (n *Node) LocalPresence() (*wire.Presence, error) {
	p := &wire.Presence{
		Origin:  n.key,
		Name:    n.cfg.Name,
		About:   clipText(n.cfg.About, 200), // what it is doing; the hello default says nothing
		Version: version.String(),
		Harness: n.cfg.Harness,
		Model:   n.Model(),
		Host:    hostname(),
		Shares:  n.ShareWith(),
		TS:      wire.Now(),
	}
	if last, waiting := n.Activity(); !last.IsZero() {
		p.Active, p.Waiting = wire.FormatTime(last.Truncate(activityGrain)), waiting
	}
	peers, err := n.st.Peers()
	if err != nil {
		return nil, err
	}
	for _, peer := range peers {
		if peer.Name == "" && peer.LastSeen.IsZero() && len(peer.Addrs) == 0 {
			continue // a key we only granted to or heard of
		}
		if len(p.Peers) == presenceMaxPeers {
			break
		}
		name := peer.Name
		if peer.Alias != "" && name == "" {
			name = peer.Alias
		}
		pp := wire.PresencePeer{Key: peer.Key, Name: name, Up: n.Connected(peer.Key)}
		if ci := n.ConnInfo(peer.Key); ci != nil && ci.RTT > 0 {
			pp.RTT = max(1, ci.RTT.Milliseconds())
		}
		p.Peers = append(p.Peers, pp)
	}
	threads, err := n.st.Threads("", "")
	if err != nil {
		return nil, err
	}
	for _, t := range threads {
		if len(p.Threads) == presenceMaxThreads {
			break
		}
		p.Threads = append(p.Threads, wire.PresenceThread{
			Th:      t.Th,
			Peer:    t.Peer,
			Subject: clipText(t.Subject, 120),
			Mine:    t.MyState,
			Theirs:  t.TheirState,
			Updated: wire.FormatTime(t.Updated),
			Unread:  t.Unread,
		})
	}
	p.Outbox, _ = n.st.OutboxCount("")
	// Unread messages, not notices (sys records), as status counts them.
	unread, _ := n.st.Query(store.Filter{Inbox: true, UnreadOnly: true, Limit: 10000})
	for _, r := range unread {
		if r.Dir != "sys" {
			p.Unread++
		}
	}
	return p, nil
}

// digest identifies a document's content, ignoring seq, ts and sig, so
// that heartbeats are only needed when nothing changed.
func presenceDigest(p *wire.Presence) string {
	c := *p
	c.Seq, c.TS, c.Sig = 0, "", ""
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// nextPresenceSeq returns a sequence number larger than any this node has
// used, even across restarts and clock steps.
func (n *Node) nextPresenceSeq() int64 {
	n.presMu.Lock()
	defer n.presMu.Unlock()
	if n.presSeq == 0 {
		if v, ok, _ := n.st.GetKV("presence_seq"); ok {
			n.presSeq, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	next := max(n.presSeq+1, time.Now().UnixMilli())
	n.presSeq = next
	n.st.SetKV("presence_seq", strconv.FormatInt(next, 10))
	return next
}

// emitPresence signs the current document and sends it to every connected
// peer.
func (n *Node) emitPresence(p *wire.Presence) {
	p.Seq = n.nextPresenceSeq()
	p.TS = wire.Now()
	raw, err := wire.SignPresence(n.priv, p)
	if err != nil {
		n.logf("presence: %v", err)
		return
	}
	n.presMu.Lock()
	n.presDoc = raw
	n.presMu.Unlock()
	n.forwardPresence(raw, 0)
}

func (n *Node) presenceMsg(doc json.RawMessage, hops int) wire.PresenceMsg {
	return wire.PresenceMsg{Envelope: wire.Envelope{T: wire.TPresence, ID: n.ids.New(), TS: wire.Now()}, Doc: doc, Hops: hops}
}

// speaksPresence reports whether a peer listed presence in its hello caps.
// Presence goes only to peers that did: the rest would have to ignore it,
// and some pass unknown types on to their agent.
func speaksPresence(c *Conn) bool { return slices.Contains(c.hello.Caps, wire.TPresence) }

// forwardPresence sends a document to every connected peer that speaks
// presence, except those listed.
func (n *Node) forwardPresence(doc json.RawMessage, hops int, except ...string) {
	n.mu.Lock()
	var conns []*Conn
	for key, p := range n.peers {
		if p.conn != nil && speaksPresence(p.conn) && !slices.Contains(except, key) {
			conns = append(conns, p.conn)
		}
	}
	n.mu.Unlock()
	for _, c := range conns {
		c.send(n.presenceMsg(doc, hops))
	}
}

// presenceLoop publishes this node's presence: shortly after anything
// changes, and as a heartbeat while connected. It only ever sends over
// connections that already exist, so it never wakes a sleeping peer.
func (n *Node) presenceLoop() {
	defer n.wg.Done()
	heartbeat := time.NewTicker(presenceHeartbeat)
	defer heartbeat.Stop()
	var lastDigest string
	var lastSent time.Time
	for {
		changed := n.Changed()
		select {
		case <-n.ctx.Done():
			return
		case <-changed:
			// Let a burst of changes settle before describing them.
			settle := time.NewTimer(presenceDebounce)
		wait:
			for {
				select {
				case <-n.ctx.Done():
					settle.Stop()
					return
				case <-n.Changed():
				case <-settle.C:
					break wait
				}
			}
		case <-heartbeat.C:
		}
		if !n.anyPresencePeer() {
			continue
		}
		p, err := n.LocalPresence()
		if err != nil {
			n.logf("presence: %v", err)
			continue
		}
		d := presenceDigest(p)
		if d == lastDigest && time.Since(lastSent) < presenceHeartbeat-time.Second {
			continue
		}
		n.emitPresence(p)
		lastDigest, lastSent = d, time.Now()
	}
}

func (n *Node) anyPresencePeer() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, p := range n.peers {
		if p.conn != nil && speaksPresence(p.conn) {
			return true
		}
	}
	return false
}

// onPresence verifies, stores and forwards a presence document. Presence is
// an extension, so a malformed or forged one is dropped, never fatal.
func (n *Node) onPresence(c *Conn, line []byte) error {
	var m wire.PresenceMsg
	if err := wire.Decode(line, &m); err != nil || len(m.Doc) == 0 {
		return nil
	}
	p, err := wire.ParsePresence(m.Doc)
	if err != nil {
		n.logf("presence from %s dropped: %v", wire.ShortKey(c.peerKey), err)
		return nil
	}
	if p.Origin == n.key {
		return nil // our own, echoed back
	}
	now := time.Now()
	ts := p.Time()
	if now.Sub(ts) > presenceMaxAge || ts.Sub(now) > presenceFuture {
		return nil
	}
	stored, isNew, err := n.st.PutPresence(&store.PresenceRow{
		Origin: p.Origin, Seq: p.Seq, Name: p.Name, TS: ts, Received: now, Via: c.peerKey, Hops: m.Hops, Doc: m.Doc,
	})
	if err != nil {
		n.logf("presence: %v", err)
		return nil
	}
	if !stored {
		return nil // seen it already: stop here, which is what ends the gossip
	}
	if isNew {
		n.st.TrimPresence(presenceMaxOrigins)
		n.sysEvent(p.Origin, "presence", map[string]any{"event": "joined", "name": p.Name, "via": c.peerKey, "hops": m.Hops})
	} else {
		n.bump()
	}
	if m.Hops+1 < wire.PresenceMaxHops {
		n.forwardPresence(m.Doc, m.Hops+1, c.peerKey, p.Origin)
	}
	return nil
}

// presenceOnConnect brings a newly connected peer up to date: our own
// document, and every fresh one we know. This is the anti-entropy step, like
// resume is for messages.
func (n *Node) presenceOnConnect(c *Conn) {
	if !speaksPresence(c) {
		return
	}
	n.presMu.Lock()
	own := n.presDoc
	n.presMu.Unlock()
	if own != nil && n.cfg.Presence {
		c.send(n.presenceMsg(own, 0))
	}
	rows, err := n.st.PresenceSince(time.Now().Add(-presenceMaxAge))
	if err != nil {
		return
	}
	for _, r := range rows {
		if r.Origin == c.peerKey || r.Hops+1 >= wire.PresenceMaxHops {
			continue
		}
		c.send(n.presenceMsg(r.Doc, r.Hops+1))
	}
}

// expirePresence forgets agents that have not been heard from in a while.
func (n *Node) expirePresence() {
	old, err := n.st.ExpirePresence(time.Now().Add(-presenceMaxAge))
	if err != nil {
		n.logf("presence expiry: %v", err)
		return
	}
	for _, r := range old {
		n.sysEvent(r.Origin, "presence", map[string]any{"event": "left", "name": r.Name})
	}
}

func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// hostname is this machine's name, as presence shares it.
func hostname() string {
	h, _ := os.Hostname()
	return clipText(h, 100)
}
