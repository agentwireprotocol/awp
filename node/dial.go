package node

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/agentwireprotocol/awp/store"
	"github.com/agentwireprotocol/awp/tunnel"
	"github.com/agentwireprotocol/awp/wire"
)

// dialResult is the outcome of a handshake on a connection we dialed.
type dialResult struct {
	key string
	err error
}

// dial opens a tunnel stream to the peer the addresses describe and runs
// the handshake. On success the connection keeps running in its own
// goroutine and the peer's key is returned.
func (n *Node) dial(ctx context.Context, addrs ...tunnel.Address) (string, error) {
	s, err := n.tun.Dial(ctx, addrs...)
	if err != nil {
		return "", err
	}
	res := make(chan dialResult, 1)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.serveConn(s, true, res)
	}()
	select {
	case r := <-res:
		return r.key, r.err
	case <-ctx.Done():
		s.Close()
		r := <-res
		if r.err == nil {
			return r.key, nil
		}
		return "", ctx.Err()
	}
}

// Connect dials an address given out of band (section 6) and returns the
// key of the peer. If that peer is already connected, the connection is
// reused.
func (n *Node) Connect(ctx context.Context, addr string) (string, error) {
	a, err := tunnel.ParseAddress(addr)
	if err != nil {
		return "", err
	}
	key := a.KeyString()
	if key == n.key {
		return "", errors.New("that is this peer's own address")
	}
	if n.settle(ctx, key) {
		n.st.SetParked(key, false)
		return key, nil
	}
	known := n.knownAddresses(key)
	got, err := n.dial(ctx, append([]tunnel.Address{a}, known...)...)
	if err != nil {
		return "", err
	}
	if err := n.st.Tx(func(q store.Q) error { return store.AddAddr(q, key, a.String(), "connect", time.Now()) }); err != nil {
		n.logf("remember address: %v", err)
	}
	n.st.SetParked(got, false)
	return got, nil
}

// settle reports whether key has a live connection to reuse. A connection
// that is saying bye is not one: settle waits for it to end, so that a
// connect right after a bye dials afresh instead of returning it.
func (n *Node) settle(ctx context.Context, key string) bool {
	n.mu.Lock()
	var c *Conn
	if p := n.peers[key]; p != nil {
		c = p.conn
	}
	n.mu.Unlock()
	if c == nil {
		return false
	}
	if !c.byeSent.Load() {
		return true
	}
	// Wait for the node to let go of it, not just for its reader to stop.
	for {
		ch := n.Changed()
		n.mu.Lock()
		p := n.peers[key]
		gone := p == nil || p.conn != c
		live := p != nil && p.conn != nil && p.conn != c
		n.mu.Unlock()
		if gone {
			return live
		}
		select {
		case <-ch:
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return false
		}
	}
}

// ConnectKey dials a known peer at everything known about reaching it.
func (n *Node) ConnectKey(ctx context.Context, key string) error {
	if n.settle(ctx, key) {
		n.st.SetParked(key, false)
		return nil
	}
	a, ok := n.addressOf(key)
	if !ok {
		return fmt.Errorf("no known address for %s", wire.ShortKey(key))
	}
	got, err := n.dial(ctx, a)
	if err != nil {
		return err
	}
	if got != key {
		return fmt.Errorf("address now belongs to %s", wire.ShortKey(got))
	}
	n.st.SetParked(key, false)
	return nil
}

// knownAddresses parses the stored addresses of key, those given out of
// band (which carry the pre-shared key) first.
func (n *Node) knownAddresses(key string) []tunnel.Address {
	rows, _ := store.Addrs(n.st.DB(), key)
	var first, rest []tunnel.Address
	for _, r := range rows {
		a, err := tunnel.ParseAddress(r.Addr)
		if err != nil || a.KeyString() != key {
			continue
		}
		if r.Source == "connect" {
			first = append(first, a)
		} else {
			rest = append(rest, a)
		}
	}
	return append(first, rest...)
}

// addressOf is everything known about reaching key, merged: what the
// reconnect loop dials and what an introduction hands on.
func (n *Node) addressOf(key string) (tunnel.Address, bool) {
	as := n.knownAddresses(key)
	if len(as) == 0 {
		return tunnel.Address{}, false
	}
	m, err := tunnel.Merge(as...)
	return m, err == nil
}

// dialer is a peer's reconnect loop (section 10.3): exponential backoff
// capped at MaxBackoff, no give-up, for as long as wantConnection holds.
type dialer struct {
	n      *Node
	key    string
	wakeCh chan struct{}

	mu  sync.Mutex
	err string
}

func (d *dialer) wake() {
	select {
	case d.wakeCh <- struct{}{}:
	default:
	}
}

func (d *dialer) lastErr() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

func (d *dialer) setErr(err error) {
	d.mu.Lock()
	d.err = err.Error()
	d.mu.Unlock()
}

// ensureDialer starts a reconnect loop for key if it has no connection, no
// loop yet, a known address, and a reason to connect. immediate skips the
// grace period for addresses learned from the peer's own hello.
func (n *Node) ensureDialer(key string, immediate bool) {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	p := n.peer(key)
	if p.conn != nil || p.dialer != nil {
		if p.dialer != nil && immediate {
			p.dialer.wake()
		}
		n.mu.Unlock()
		return
	}
	n.mu.Unlock()

	if !n.wantConnection(key) {
		return
	}
	if _, ok := n.addressOf(key); !ok {
		return
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed || p.conn != nil || p.dialer != nil {
		return
	}
	d := &dialer{n: n, key: key, wakeCh: make(chan struct{}, 1)}
	p.dialer = d
	n.wg.Add(1)
	go d.run(immediate)
}

func (d *dialer) run(immediate bool) {
	n := d.n
	defer n.wg.Done()
	defer func() {
		n.mu.Lock()
		if p := n.peers[d.key]; p != nil && p.dialer == d {
			p.dialer = nil
		}
		n.mu.Unlock()
	}()

	backoff := time.Second
	first := true
	for {
		if n.ctx.Err() != nil || n.Connected(d.key) || !n.wantConnection(d.key) {
			return
		}
		addrs, err := store.Addrs(n.st.DB(), d.key)
		if err != nil || len(addrs) == 0 {
			return
		}
		if first && !immediate && onlyFromHello(addrs) {
			// We learned this address from the peer's hello, so the peer
			// dialed us originally and will most likely be back on its own.
			// Give it a head start rather than racing it.
			if ok, _ := d.sleep(n.cfg.DialGrace); !ok {
				return
			}
			first = false
			continue
		}
		first = false
		addr, ok := n.addressOf(d.key)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(n.ctx, 45*time.Second)
		key, err := n.dial(ctx, addr)
		cancel()
		if err == nil && key != d.key {
			err = fmt.Errorf("address now belongs to %s", wire.ShortKey(key))
		}
		if err == nil {
			for _, a := range addrs {
				n.st.AddrResult(d.key, a.Addr, nil, time.Now())
			}
			return
		}
		var peerErr errPeerSaid
		if errors.As(err, &peerErr) && (peerErr.e.Code == wire.ErrRefused || peerErr.e.Code == wire.ErrAuth || peerErr.e.Code == wire.ErrVersion) {
			n.logf("%s refused us: %v", wire.ShortKey(d.key), err)
		}
		d.setErr(err)
		for _, a := range addrs {
			n.st.AddrResult(d.key, a.Addr, err, time.Now())
		}
		wait := backoff/2 + rand.N(backoff) // jitter in [b/2, 3b/2)
		n.logf("reconnect to %s failed (%s); retrying in %v", wire.ShortKey(d.key), d.lastErr(), wait.Round(100*time.Millisecond))
		ok, woken := d.sleep(wait)
		if !ok {
			return
		}
		if woken {
			backoff = time.Second // new outbound traffic: start over quickly
		} else {
			backoff = min(backoff*2, n.cfg.MaxBackoff)
		}
	}
}

// sleep waits for dur, a wake-up, or shutdown. ok is false on shutdown;
// woken reports an early wake-up.
func (d *dialer) sleep(dur time.Duration) (ok, woken bool) {
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-t.C:
		return true, false
	case <-d.wakeCh:
		return true, true
	case <-d.n.ctx.Done():
		return false, false
	}
}

func onlyFromHello(addrs []store.Addr) bool {
	for _, a := range addrs {
		if a.Source != "hello" {
			return false
		}
	}
	return true
}
