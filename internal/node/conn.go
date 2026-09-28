package node

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentwireprotocol/awp/internal/store"
	"github.com/agentwireprotocol/awp/internal/version"
	"github.com/agentwireprotocol/awp/wire"
)

// helloCaps are the message families this implementation speaks beyond
// the mandatory ones.
var helloCaps = []string{"chat", "blob", "grant", "introduce", wire.TPresence, wire.TMirror}

// Conn is one connection to a peer, from hello to close.
type Conn struct {
	n        *Node
	raw      net.Conn
	r        *wire.Reader
	w        *wire.Writer
	outbound bool
	via      string

	// Set by the handshake.
	peerKey    string
	peerPub    ed25519.PublicKey
	hello      wire.Hello
	peerGrants []json.RawMessage

	ctrl       chan ctrlItem
	pumpCh     chan struct{}
	accepted   chan struct{} // closed when the peer's first non-err line arrives
	acceptOnce sync.Once
	pumping    atomic.Bool
	closed     chan struct{}
	closeOnce  sync.Once
	readerDone chan struct{}
	writerDone chan struct{}

	reasonMu sync.Mutex
	reason   string

	lastRecv atomic.Int64
	missed   atomic.Int32
	// The last ping sent (id and time, guarded by pingMu) and the round
	// trip its pong measured, in nanoseconds.
	pingMu  sync.Mutex
	pingID  string
	pingAt  time.Time
	rtt     atomic.Int64
	byeSent atomic.Bool
}

type ctrlItem struct {
	line  []byte
	after func() // runs after the line is written
}

func newConn(n *Node, raw net.Conn, outbound bool, via string) *Conn {
	return &Conn{
		n:          n,
		raw:        raw,
		r:          wire.NewReader(raw),
		w:          wire.NewWriter(raw),
		outbound:   outbound,
		via:        via,
		ctrl:       make(chan ctrlItem, 4096),
		pumpCh:     make(chan struct{}, 1),
		accepted:   make(chan struct{}),
		closed:     make(chan struct{}),
		readerDone: make(chan struct{}),
		writerDone: make(chan struct{}),
	}
}

// dialerKey is the key of whichever side dialed this connection.
func (c *Conn) dialerKey() string {
	if c.outbound {
		return c.n.key
	}
	return c.peerKey
}

// Close tears the connection down. It is safe to call more than once; the
// first reason sticks.
func (c *Conn) Close(reason string) {
	c.closeOnce.Do(func() {
		c.reasonMu.Lock()
		c.reason = reason
		c.reasonMu.Unlock()
		close(c.closed)
		c.raw.Close()
	})
}

func (c *Conn) closeReason() string {
	c.reasonMu.Lock()
	defer c.reasonMu.Unlock()
	return c.reason
}

func (c *Conn) trace(dir string, line []byte) {
	if !c.n.cfg.Trace {
		return
	}
	s := string(line)
	if len(s) > 600 {
		s = s[:600] + "…"
	}
	arrow := "<"
	if dir == "out" {
		arrow = ">"
	}
	c.n.logf("%s %s %s", wire.ShortKey(c.peerKey), arrow, s)
}

// writeNow writes a line directly. Only for the handshake, before the
// writer goroutine exists.
func (c *Conn) writeNow(v any) ([]byte, error) {
	line, err := wire.Encode(v)
	if err != nil {
		return nil, err
	}
	c.trace("out", line)
	return line, c.w.WriteLine(line)
}

func (c *Conn) errMsg(code, re, detail string) wire.Err {
	return wire.Err{Envelope: wire.Envelope{T: wire.TErr, ID: c.n.ids.New(), TS: wire.Now(), Re: re}, Code: code, Detail: detail}
}

// sendErrNow reports a handshake failure to the peer before closing.
func (c *Conn) sendErrNow(code, re, detail string) {
	c.raw.SetWriteDeadline(time.Now().Add(5 * time.Second))
	c.writeNow(c.errMsg(code, re, detail))
}

// errPeerSaid wraps an err message received from the peer.
type errPeerSaid struct{ e wire.Err }

func (e errPeerSaid) Error() string {
	return fmt.Sprintf("peer sent err %s: %s", e.e.Code, e.e.Detail)
}

// handshake runs section 7: both sides send hello at once, then auth over
// the transcript of both hellos.
func (c *Conn) handshake() error {
	n := c.n
	c.raw.SetDeadline(time.Now().Add(n.cfg.HandshakeTimeout))
	defer c.raw.SetDeadline(time.Time{})

	myHello, err := c.writeNow(wire.Hello{
		Envelope: wire.Envelope{T: wire.THello, ID: n.ids.New(), TS: wire.Now()},
		V:        wire.Version,
		Key:      n.key,
		Name:     n.cfg.Name,
		Nonce:    wire.Nonce(32),
		Caps:     helloCaps,
		About:    n.about(),
		Addr:     n.advertised(),
		Shares:   n.ShareWith(),
	})
	if err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	peerHello, env, err := c.readHandshakeLine(wire.THello)
	if err != nil {
		return err
	}
	var h wire.Hello
	if err := wire.Decode(peerHello, &h); err != nil {
		c.sendErrNow(wire.ErrBadFrame, env.ID, "malformed hello")
		return fmt.Errorf("malformed hello: %v", err)
	}
	if h.V != wire.Version {
		c.sendErrNow(wire.ErrVersion, h.ID, fmt.Sprintf("this peer speaks awp v%d", wire.Version))
		return fmt.Errorf("peer speaks v%d", h.V)
	}
	pub, err := wire.ParseKey(h.Key)
	if err != nil {
		c.sendErrNow(wire.ErrAuth, h.ID, "bad key: "+err.Error())
		return fmt.Errorf("bad hello key: %v", err)
	}
	if h.Key == n.key {
		c.sendErrNow(wire.ErrAuth, h.ID, "connected to self")
		return errors.New("connected to self")
	}
	c.peerKey, c.peerPub, c.hello = h.Key, pub, h

	if _, err := c.writeNow(wire.Auth{
		Envelope: wire.Envelope{T: wire.TAuth, ID: n.ids.New(), TS: wire.Now()},
		Sig:      wire.SignAuth(n.priv, myHello, peerHello),
		Grants:   n.grantsToPresent(h.Key),
	}); err != nil {
		return fmt.Errorf("send auth: %w", err)
	}

	authLine, env, err := c.readHandshakeLine(wire.TAuth)
	if err != nil {
		return err
	}
	var a wire.Auth
	if err := wire.Decode(authLine, &a); err != nil {
		c.sendErrNow(wire.ErrBadFrame, env.ID, "malformed auth")
		return fmt.Errorf("malformed auth: %v", err)
	}
	if err := wire.VerifyAuth(pub, a.Sig, peerHello, myHello); err != nil {
		c.sendErrNow(wire.ErrAuth, a.ID, "auth signature does not verify")
		return fmt.Errorf("auth from %s: %v", wire.ShortKey(h.Key), err)
	}
	c.peerGrants = a.Grants
	return nil
}

// readHandshakeLine reads the next line and checks it has type want. An err
// from the peer ends the handshake with that error.
func (c *Conn) readHandshakeLine(want string) ([]byte, wire.Envelope, error) {
	line, err := c.r.ReadLine()
	if err != nil {
		if errors.Is(err, wire.ErrLineTooLong) {
			c.sendErrNow(wire.ErrTooLarge, "", "line exceeds 1 MiB")
		}
		return nil, wire.Envelope{}, fmt.Errorf("waiting for %s: %w", want, err)
	}
	c.trace("in", line)
	env, err := wire.ParseEnvelope(line)
	if err != nil {
		c.sendErrNow(wire.ErrBadFrame, "", "not a JSON object")
		return nil, env, fmt.Errorf("waiting for %s: %w", want, err)
	}
	if env.T == wire.TErr {
		var e wire.Err
		wire.Decode(line, &e)
		return nil, env, errPeerSaid{e}
	}
	if env.T != want {
		code := wire.ErrBadFrame
		if want == wire.TAuth {
			code = wire.ErrAuth
		}
		c.sendErrNow(code, env.ID, "expected "+want+", got "+env.T)
		return nil, env, fmt.Errorf("expected %s, got %q", want, env.T)
	}
	return line, env, nil
}

// run drives an established connection until it ends: resume, then the
// reader loop, with a writer and a pinger alongside.
func (c *Conn) run() {
	n := c.n
	c.lastRecv.Store(time.Now().UnixNano())
	go c.writer()
	go c.pinger()

	seen, err := n.st.SeenMap(c.peerKey, time.Now().Add(-n.cfg.OutboxTTL))
	if err != nil {
		n.logf("seen map for %s: %v", wire.ShortKey(c.peerKey), err)
		seen = map[string]string{}
	}
	c.send(wire.Resume{Envelope: wire.Envelope{T: wire.TResume, ID: n.ids.New(), TS: wire.Now()}, Seen: seen})
	// Resume is mandatory, but do not wait forever for a peer that skips it:
	// replaying everything is safe because receivers dedup by id.
	resumeTimer := time.AfterFunc(15*time.Second, func() {
		select {
		case <-c.closed:
			return
		default:
		}
		if !c.pumping.Load() {
			n.logf("%s sent no resume within 15s; replaying the whole outbox", wire.ShortKey(c.peerKey))
			c.startPump()
		}
	})

	c.reader()
	resumeTimer.Stop()
	c.Close("reader stopped")
	<-c.writerDone
}

func (c *Conn) reader() {
	defer close(c.readerDone)
	for {
		line, err := c.r.ReadLine()
		if err != nil {
			switch {
			case errors.Is(err, wire.ErrLineTooLong):
				c.sendErrNow(wire.ErrTooLarge, "", "line exceeds 1 MiB")
				c.Close("peer sent a line over 1 MiB")
			case errors.Is(err, io.EOF):
				c.Close("peer closed the connection")
			default:
				c.Close(err.Error())
			}
			return
		}
		select {
		case <-c.closed:
			// Replaced or shutting down: do not process anything more
			// from this connection.
			return
		default:
		}
		c.lastRecv.Store(time.Now().UnixNano())
		c.missed.Store(0)
		c.trace("in", line)
		if err := c.n.handleLine(c, line); err != nil {
			c.Close(err.Error())
			return
		}
	}
}

// send queues a line for the writer. It never blocks: if the queue is full
// the connection is hopelessly behind and is closed (the outbox is on disk,
// nothing is lost).
func (c *Conn) send(v any) {
	c.sendThen(v, nil)
}

func (c *Conn) sendThen(v any, after func()) {
	if c.byeSent.Load() {
		return // after bye a peer sends nothing else
	}
	c.enqueue(v, after)
}

func (c *Conn) enqueue(v any, after func()) {
	line, err := wire.Encode(v)
	if err != nil {
		c.n.logf("encode %T: %v", v, err)
		return
	}
	select {
	case c.ctrl <- ctrlItem{line: line, after: after}:
	case <-c.closed:
	default:
		c.Close("control queue full")
	}
}

func (c *Conn) closeWrite() {
	if cw, ok := c.raw.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	time.AfterFunc(5*time.Second, func() { c.Close("closed after err or bye") })
}

func (c *Conn) kickPump() {
	select {
	case c.pumpCh <- struct{}{}:
	default:
	}
}

func (c *Conn) startPump() {
	c.pumping.Store(true)
	c.kickPump()
}

// writer owns all writes after the handshake. Control lines (acks, pongs,
// errs...) go first; outbox lines follow in outbox order once the peer's
// resume has been processed.
func (c *Conn) writer() {
	defer close(c.writerDone)
	var sentSeq int64
	write := func(it ctrlItem) bool {
		c.trace("out", it.line)
		if err := c.w.WriteLine(it.line); err != nil {
			// The peer may have said why it hung up (an err line still in
			// our receive buffer). Give the reader a moment to find it
			// before closing with the less helpful write error.
			time.AfterFunc(500*time.Millisecond, func() { c.Close("write: " + err.Error()) })
			return false
		}
		if it.after != nil {
			it.after()
		}
		return true
	}
	drain := func() bool {
		for {
			select {
			case it := <-c.ctrl:
				if !write(it) {
					return false
				}
			default:
				return true
			}
		}
	}
	for {
		if !drain() {
			return
		}
		select {
		case <-c.closed:
			return
		default:
		}
		if c.pumping.Load() && !c.byeSent.Load() {
			rows, err := c.n.st.OutboxAfter(c.peerKey, sentSeq, 16)
			if err != nil {
				c.n.logf("outbox for %s: %v", wire.ShortKey(c.peerKey), err)
			}
			if len(rows) > 0 {
				for _, r := range rows {
					if !drain() || !write(ctrlItem{line: r.Line}) {
						return
					}
					sentSeq = r.Seq
				}
				continue
			}
		}
		select {
		case it := <-c.ctrl:
			if !write(it) {
				return
			}
		case <-c.pumpCh:
		case <-c.closed:
			return
		}
	}
}

// pinger implements section 9.4: ping when idle, and two missed pongs mean
// the connection is dead. Any received line counts as proof of life.
func (c *Conn) pinger() {
	interval := c.n.cfg.PingInterval
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-t.C:
		}
		if c.missed.Load() >= 2 {
			c.Close("two missed pongs")
			return
		}
		idle := time.Since(time.Unix(0, c.lastRecv.Load()))
		if idle >= interval-interval/10 {
			c.missed.Add(1)
			c.ping()
		}
	}
}

// ping sends a ping and remembers it, to time the pong.
func (c *Conn) ping() {
	p := wire.Ping{Envelope: wire.Envelope{T: wire.TPing, ID: c.n.ids.New(), TS: wire.Now()}}
	c.pingMu.Lock()
	c.pingID, c.pingAt = p.ID, time.Now()
	c.pingMu.Unlock()
	c.send(p)
}

// onPong records the round trip time when the pong answers our last ping.
func (c *Conn) onPong(re string) {
	c.pingMu.Lock()
	id, at := c.pingID, c.pingAt
	c.pingMu.Unlock()
	if re != "" && re == id {
		c.rtt.Store(int64(time.Since(at)))
	}
}

// RTT is the last measured round trip time, or zero.
func (c *Conn) RTT() time.Duration { return time.Duration(c.rtt.Load()) }

// sendBye starts a graceful close (section 9.6): nothing else is sent, and
// the connection closes on the peer's bye or after five seconds.
func (c *Conn) sendBye(reason string) {
	if c.byeSent.Swap(true) {
		return
	}
	c.enqueue(wire.Bye{Envelope: wire.Envelope{T: wire.TBye, ID: c.n.ids.New(), TS: wire.Now()}, Reason: reason}, func() {
		time.AfterFunc(5*time.Second, func() { c.Close("bye (no reply within 5s)") })
	})
}

// serveConn runs a connection from handshake to close. For connections we
// dialed, done receives the handshake outcome.
func (n *Node) serveConn(raw net.Conn, outbound bool, via string, done chan<- dialResult) {
	c := newConn(n, raw, outbound, via)
	err := c.handshake()
	if err == nil {
		err = n.afterHandshake(c)
	}
	if err != nil && done != nil {
		done <- dialResult{key: c.peerKey, err: err}
	}
	if err == nil && done != nil {
		// A listener that refuses us (admission policy) says so with an err
		// right after auth, so the dial only counts once the peer's resume,
		// or anything other than an err, has arrived.
		go func() {
			select {
			case <-c.accepted:
				done <- dialResult{key: c.peerKey}
			case <-c.closed:
				done <- dialResult{key: c.peerKey, err: fmt.Errorf("closed during setup: %s", c.closeReason())}
			case <-time.After(n.cfg.HandshakeTimeout):
				done <- dialResult{key: c.peerKey} // no resume yet; be lenient
			}
		}()
	}
	if err != nil {
		if c.peerKey != "" {
			n.logf("connection with %s via %s failed: %v", wire.ShortKey(c.peerKey), via, err)
		} else {
			n.logf("connection via %s failed: %v", via, err)
		}
		c.Close(err.Error())
		return
	}
	n.logf("connected to %s (%s) via %s", c.hello.Name, wire.ShortKey(c.peerKey), via)
	c.run()
	n.logf("disconnected from %s (%s): %s", c.hello.Name, wire.ShortKey(c.peerKey), c.closeReason())
	n.deactivate(c)
}

// afterHandshake records the peer, checks grants and admission policy, and
// makes the connection active.
func (n *Node) afterHandshake(c *Conn) error {
	now := time.Now()
	h := c.hello
	err := n.st.Tx(func(q store.Q) error {
		if err := store.TouchPeer(q, c.peerKey, h.Name, h.About, h.Caps, now); err != nil {
			return err
		}
		if c.outbound {
			return store.AddAddr(q, c.peerKey, c.via, "connect", now)
		}
		if h.Addr != "" {
			return store.AddAddr(q, c.peerKey, h.Addr, "hello", now)
		}
		return nil
	})
	if err != nil {
		c.sendErrNow(wire.ErrInternal, "", "storage error")
		return err
	}
	for _, raw := range c.peerGrants {
		n.acceptPresentedGrant(c.peerKey, raw, "auth")
	}
	if err := n.admit(c.peerKey, c.outbound); err != nil {
		c.sendErrNow(wire.ErrAuth, "", err.Error())
		n.sysEvent(c.peerKey, "refused", map[string]any{"reason": err.Error(), "name": h.Name, "via": c.via})
		return err
	}
	if err := n.activate(c); err != nil {
		return err
	}
	if c.outbound {
		n.st.AddrResult(c.peerKey, c.via, nil, now)
	}
	n.sysEvent(c.peerKey, "connected", map[string]any{"name": h.Name, "about": h.About, "via": c.via, "outbound": c.outbound, "caps": h.Caps})
	n.noteShares(c.peerKey, h.Shares)
	return nil
}

func (n *Node) about() string {
	if n.cfg.About != "" {
		return n.cfg.About
	}
	return fmt.Sprintf("awp-go %s, key fingerprint %s", version.String(), wire.Fingerprint(n.pub))
}
