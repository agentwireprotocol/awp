package node

import (
	"fmt"
	"time"

	"github.com/agentwireprotocol/awp/store"
	"github.com/agentwireprotocol/awp/transport"
	"github.com/agentwireprotocol/awp/wire"
)

// fatal reports a protocol error to the peer and returns an error that
// closes the connection.
func (c *Conn) fatal(code, re, detail string) error {
	c.sendErrNow(code, re, detail)
	return fmt.Errorf("%s: %s", code, detail)
}

// handleLine dispatches one received line. A returned error closes the
// connection.
func (n *Node) handleLine(c *Conn, line []byte) error {
	env, err := wire.ParseEnvelope(line)
	if err != nil {
		return c.fatal(wire.ErrBadFrame, "", "not a JSON object")
	}
	if env.T != wire.TErr {
		c.acceptOnce.Do(func() { close(c.accepted) })
	}
	switch env.T {
	case wire.TMsg:
		return n.onMsg(c, line)
	case wire.TState:
		return n.onState(c, line)
	case wire.TAck:
		return n.onAck(c, env)
	case wire.TChunk:
		return n.onChunk(c, line)
	case wire.TResume:
		return n.onResume(c, line)
	case wire.TPing:
		c.send(wire.Pong{Envelope: wire.Envelope{T: wire.TPong, ID: n.ids.New(), TS: wire.Now(), Re: env.ID}})
		return nil
	case wire.TPong:
		c.onPong(env.Re) // liveness itself is tracked by the reader
		return nil
	case wire.TBye:
		return n.onBye(c, line)
	case wire.TErr:
		return n.onErr(c, line)
	case wire.TGrant:
		return n.onGrant(c, line)
	case wire.TIntroduce:
		return n.onIntroduce(c, line)
	case wire.TPresence:
		return n.onPresence(c, line)
	case wire.TMirror:
		return n.onMirror(c, line)
	case wire.TPrivate:
		return n.onPrivate(c, line)
	case "":
		return c.fatal(wire.ErrBadFrame, env.ID, "missing message type")
	}
	// Unknown types are ignored (section 5). Replying unsupported is
	// optional; staying quiet keeps a newer peer's chatter harmless.
	return nil
}

func (n *Node) ack(c *Conn, th, id string) {
	c.send(wire.Ack{Envelope: wire.Envelope{T: wire.TAck, ID: n.ids.New(), TS: wire.Now(), Th: th, Re: id}})
}

// storageFailed answers a message we could not persist. We send internal
// and close: without an ack the peer keeps the message, and resume on the
// next connection replays it.
func (n *Node) storageFailed(c *Conn, id string, err error) error {
	n.logf("storage error on message %s from %s: %v", id, wire.ShortKey(c.peerKey), err)
	c.sendErrNow(wire.ErrInternal, id, "could not store message")
	return fmt.Errorf("storage error: %w", err)
}

func (n *Node) onMsg(c *Conn, line []byte) error {
	var m wire.Msg
	if err := wire.Decode(line, &m); err != nil {
		return c.fatal(wire.ErrBadFrame, "", "malformed msg: "+err.Error())
	}
	if m.ID == "" || m.Th == "" {
		return c.fatal(wire.ErrBadFrame, m.ID, "msg needs id and th")
	}
	now := time.Now()
	reqs := n.inspectRequests(c.peerKey, &m)
	var meta map[string]any
	if len(reqs) > 0 {
		meta = map[string]any{"requests": reqs}
	}
	var inserted bool
	var refuse []string
	var arrived []*store.Blob
	err := n.st.Tx(func(q store.Q) error {
		rec := &store.Record{Peer: c.peerKey, Dir: "in", ID: m.ID, T: wire.TMsg, Th: m.Th, Line: line, At: now, Meta: meta}
		var err error
		if inserted, err = store.Append(q, rec); err != nil || !inserted {
			return err
		}
		if err := store.TouchThread(q, c.peerKey, m.Th, m.Subject, "them", now); err != nil {
			return err
		}
		if err := store.BumpSeen(q, c.peerKey, m.Th, m.ID, now); err != nil {
			return err
		}
		if err := n.mirror(q, c.peerKey, m.Th, "in", line, now); err != nil {
			return err
		}
		for _, p := range m.Parts {
			if p.K != wire.PartBlob || p.Ref == "" {
				continue
			}
			refused, done, err := n.noteInboundBlob(q, c.peerKey, m.Th, p, now)
			if err != nil {
				return err
			}
			if refused {
				refuse = append(refuse, p.Ref)
			}
			if done != nil {
				arrived = append(arrived, done)
			}
		}
		return nil
	})
	if err != nil {
		for _, b := range arrived {
			n.unplaceBlob(b)
		}
		return n.storageFailed(c, m.ID, err)
	}
	n.ack(c, m.Th, m.ID)
	if !inserted {
		return nil // a replay of something we already have: re-acked, done
	}
	for _, ref := range refuse {
		c.send(wire.Err{Envelope: wire.Envelope{T: wire.TErr, ID: n.ids.New(), TS: wire.Now(), Re: m.ID}, Code: wire.ErrBlobRefused, Ref: ref,
			Detail: fmt.Sprintf("blob exceeds this peer's %d byte limit", n.cfg.BlobLimit)})
	}
	for _, r := range reqs {
		if !r.Allowed {
			c.send(c.errMsg(wire.ErrForbidden, m.ID, fmt.Sprintf("%s requires a grant for capability %q", r.Mime, r.Cap)))
		}
	}
	for _, b := range arrived {
		n.blobEvent(b)
	}
	n.bump()
	n.wakeMirrors()
	n.serveRequests(c.peerKey, &m, reqs)
	return nil
}

func (n *Node) onState(c *Conn, line []byte) error {
	var s wire.State
	if err := wire.Decode(line, &s); err != nil {
		return c.fatal(wire.ErrBadFrame, "", "malformed state: "+err.Error())
	}
	if s.ID == "" || s.Th == "" || s.State == "" {
		return c.fatal(wire.ErrBadFrame, s.ID, "state needs id, th and state")
	}
	now := time.Now()
	var inserted bool
	err := n.st.Tx(func(q store.Q) error {
		rec := &store.Record{Peer: c.peerKey, Dir: "in", ID: s.ID, T: wire.TState, Th: s.Th, Line: line, At: now}
		var err error
		if inserted, err = store.Append(q, rec); err != nil || !inserted {
			return err
		}
		if err := store.TouchThread(q, c.peerKey, s.Th, "", "them", now); err != nil {
			return err
		}
		if err := store.SetThreadState(q, c.peerKey, s.Th, false, s.State, s.Note, now); err != nil {
			return err
		}
		if err := n.mirror(q, c.peerKey, s.Th, "in", line, now); err != nil {
			return err
		}
		return store.BumpSeen(q, c.peerKey, s.Th, s.ID, now)
	})
	if err != nil {
		return n.storageFailed(c, s.ID, err)
	}
	n.ack(c, s.Th, s.ID)
	if inserted {
		n.bump()
		n.wakeMirrors()
	}
	return nil
}

func (n *Node) onAck(c *Conn, env wire.Envelope) error {
	if env.Re == "" {
		return nil
	}
	now := time.Now()
	err := n.st.Tx(func(q store.Q) error {
		_, _, _, err := store.AckOutbox(q, c.peerKey, env.Re, now)
		if err != nil {
			return err
		}
		return store.SetAcked(q, c.peerKey, env.Re)
	})
	if err != nil {
		n.logf("ack %s from %s: %v", env.Re, wire.ShortKey(c.peerKey), err)
	}
	n.bump()
	return nil
}

func (n *Node) onResume(c *Conn, line []byte) error {
	var r wire.Resume
	if err := wire.Decode(line, &r); err != nil {
		return c.fatal(wire.ErrBadFrame, "", "malformed resume: "+err.Error())
	}
	var pruned int64
	err := n.st.Tx(func(q store.Q) error {
		var err error
		pruned, err = store.PruneSeen(q, c.peerKey, r.Seen, time.Now())
		return err
	})
	if err != nil {
		n.logf("resume from %s: %v", wire.ShortKey(c.peerKey), err)
	}
	if pruned > 0 {
		n.logf("resume: %s already had %d queued lines", wire.ShortKey(c.peerKey), pruned)
	}
	// Re-send the grants we issued to this peer. Grant delivery is not
	// acked, so this is how a grant made while disconnected (or lost in a
	// drop) eventually arrives. Receivers dedup by content.
	if gs, err := n.st.Grants(store.GrantQuery{Role: store.RoleIssued, Sub: c.peerKey, ValidAt: time.Now()}); err == nil {
		for _, g := range gs {
			if g.Audience != "" {
				continue // introductions travel in introduce messages only
			}
			c.send(wire.GrantMsg{Envelope: wire.Envelope{T: wire.TGrant, ID: n.ids.New(), TS: wire.Now()}, Grant: g.Raw})
		}
	}
	c.startPump()
	n.presenceOnConnect(c)
	n.bump()
	// A first round trip time, soon rather than after an idle interval,
	// once the opening exchange (which a ping must not interleave) is over.
	time.AfterFunc(firstPingDelay, func() {
		select {
		case <-c.closed:
		default:
			if c.RTT() == 0 {
				c.ping()
			}
		}
	})
	return nil
}

// firstPingDelay is how long after resume a connection measures its first
// round trip time.
var firstPingDelay = 3 * time.Second

func (n *Node) onBye(c *Conn, line []byte) error {
	var b wire.Bye
	wire.Decode(line, &b)
	n.st.SetParked(c.peerKey, true)
	n.sysEvent(c.peerKey, "bye", map[string]any{"reason": b.Reason, "from": "peer"})
	if c.byeSent.Load() {
		c.Close("bye")
		return nil
	}
	// Answer with our own bye, then close.
	c.byeSent.Store(true)
	c.enqueue(wire.Bye{Envelope: wire.Envelope{T: wire.TBye, ID: n.ids.New(), TS: wire.Now(), Re: b.ID}, Reason: b.Reason}, func() {
		c.closeWrite()
	})
	return nil
}

func (n *Node) onErr(c *Conn, line []byte) error {
	var e wire.Err
	if err := wire.Decode(line, &e); err != nil {
		return c.fatal(wire.ErrBadFrame, "", "malformed err")
	}
	now := time.Now()
	err := n.st.Tx(func(q store.Q) error {
		id := e.ID
		if id == "" {
			id = n.ids.New()
		}
		meta := map[string]any{"code": e.Code, "detail": e.Detail}
		th := ""
		if e.Re != "" {
			meta["re"] = e.Re
			// Attach the error to the thread of the message it answers.
			q.QueryRow(`SELECT th FROM log WHERE peer = ? AND dir = 'out' AND id = ?`, c.peerKey, e.Re).Scan(&th)
		}
		if e.Ref != "" {
			meta["ref"] = e.Ref
		}
		if _, err := store.Append(q, &store.Record{Peer: c.peerKey, Dir: "in", ID: id, T: wire.TErr, Th: th, Line: line, At: now, Meta: meta}); err != nil {
			return err
		}
		if e.Code == wire.ErrBlobRefused && e.Ref != "" {
			if err := store.DropRef(q, c.peerKey, e.Ref); err != nil {
				return err
			}
			if b, err := store.GetBlob(q, c.peerKey, "out", e.Ref); err == nil && b != nil {
				b.Status, b.Updated = "refused", now
				return store.PutBlob(q, b)
			}
		}
		return nil
	})
	if err != nil {
		n.logf("err from %s: %v", wire.ShortKey(c.peerKey), err)
	}
	n.logf("%s sent err %s: %s", wire.ShortKey(c.peerKey), e.Code, e.Detail)
	n.bump()
	if wire.ErrCloses(e.Code) {
		return fmt.Errorf("peer sent err %s: %s", e.Code, e.Detail)
	}
	return nil
}

func (n *Node) onGrant(c *Conn, line []byte) error {
	var gm wire.GrantMsg
	if err := wire.Decode(line, &gm); err != nil || len(gm.Grant) == 0 {
		return c.fatal(wire.ErrBadFrame, gm.ID, "malformed grant message")
	}
	now := time.Now()
	g, err := wire.ParseGrant(gm.Grant, now)
	if err != nil {
		n.logf("grant from %s rejected: %v", wire.ShortKey(c.peerKey), err)
		return nil
	}
	switch g.Sub {
	case n.key:
		// A grant to us. Keep it, to present when we connect to the issuer.
		existing, _ := n.st.Grants(store.GrantQuery{Role: store.RoleHeld, Sub: n.key, Iss: g.Iss})
		for _, e := range existing {
			if e.Hash == g.Hash() {
				return nil // re-sent after a resume; nothing new
			}
		}
		err := n.st.Tx(func(q store.Q) error {
			if err := store.PutGrant(q, grantRow(g, store.RoleHeld, "", now)); err != nil {
				return err
			}
			_, err := store.Append(q, &store.Record{Peer: c.peerKey, Dir: "in", ID: gm.ID, T: wire.TGrant, Line: line, At: now,
				Meta: map[string]any{"caps": g.Caps, "exp": g.Exp, "iss": g.Iss}})
			return err
		})
		if err != nil {
			n.logf("store grant: %v", err)
		}
		n.bump()
	case c.peerKey:
		n.acceptPresentedGrant(c.peerKey, gm.Grant, "grant")
	default:
		n.logf("ignoring grant from %s about a third key %s", wire.ShortKey(c.peerKey), wire.ShortKey(g.Sub))
	}
	return nil
}

func (n *Node) onIntroduce(c *Conn, line []byte) error {
	var in wire.Introduce
	if err := wire.Decode(line, &in); err != nil {
		return c.fatal(wire.ErrBadFrame, "", "malformed introduce")
	}
	if _, err := wire.ParseKey(in.Peer.Key); err != nil || in.Peer.Key == n.key {
		n.logf("ignoring introduce from %s: bad or own key", wire.ShortKey(c.peerKey))
		return nil
	}
	now := time.Now()
	var grant *wire.Grant
	if len(in.Grant) > 0 {
		g, err := wire.ParseGrant(in.Grant, now)
		switch {
		case err != nil:
			n.logf("introduce from %s carries an unusable grant: %v", wire.ShortKey(c.peerKey), err)
		case g.Iss != c.peerKey || g.Sub != n.key:
			n.logf("introduce from %s carries a grant not issued by it to us; ignoring the grant", wire.ShortKey(c.peerKey))
		default:
			grant = g
		}
	}
	addr := in.Peer.Address
	if a, err := transport.Parse(addr); err == nil {
		addr = a.String()
	} else {
		addr = ""
	}
	err := n.st.Tx(func(q store.Q) error {
		id := in.ID
		if id == "" {
			id = n.ids.New()
		}
		inserted, err := store.Append(q, &store.Record{Peer: c.peerKey, Dir: "in", ID: id, T: wire.TIntroduce, Th: in.Th, Line: line, At: now,
			Meta: map[string]any{"key": in.Peer.Key, "name": in.Peer.Name, "address": addr}})
		if err != nil || !inserted {
			return err
		}
		if err := store.EnsurePeer(q, in.Peer.Key, now); err != nil {
			return err
		}
		if in.Peer.Name != "" {
			if _, err := q.Exec(`UPDATE peers SET name = ? WHERE key = ? AND name = ''`, in.Peer.Name, in.Peer.Key); err != nil {
				return err
			}
		}
		if err := store.AddAddr(q, in.Peer.Key, addr, "introduce", now); err != nil {
			return err
		}
		if grant != nil {
			if err := store.PutGrant(q, grantRow(grant, store.RoleHeld, in.Peer.Key, now)); err != nil {
				return err
			}
		}
		if in.Th != "" {
			if err := store.TouchThread(q, c.peerKey, in.Th, "", "them", now); err != nil {
				return err
			}
			return store.BumpSeen(q, c.peerKey, in.Th, id, now)
		}
		return nil
	})
	if err != nil {
		return n.storageFailed(c, in.ID, err)
	}
	n.bump()
	return nil
}

func grantRow(g *wire.Grant, role, audience string, now time.Time) *store.GrantRow {
	if audience == "" {
		audience = g.Aud
	}
	return &store.GrantRow{Hash: g.Hash(), Role: role, Iss: g.Iss, Sub: g.Sub, Caps: g.Caps, Exp: g.Exp, Raw: g.Raw, Audience: audience, Created: now}
}
