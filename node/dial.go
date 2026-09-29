package node

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/agentwireprotocol/awp/store"
	"github.com/agentwireprotocol/awp/transport"
	"github.com/agentwireprotocol/awp/wire"
)

// dialResult is the outcome of a handshake on a connection we dialed.
type dialResult struct {
	key string
	err error
}

// dial connects to addr and runs the handshake. On success the connection
// keeps running in its own goroutine and the peer's key is returned.
func (n *Node) dial(ctx context.Context, addr string) (string, error) {
	a, err := transport.Parse(addr)
	if err != nil {
		return "", err
	}
	raw, err := n.tr.Dial(ctx, a)
	if err != nil {
		return "", err
	}
	res := make(chan dialResult, 1)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.serveConn(raw, true, a.String(), res)
	}()
	select {
	case r := <-res:
		return r.key, r.err
	case <-ctx.Done():
		raw.Close()
		r := <-res
		if r.err == nil {
			return r.key, nil
		}
		return "", ctx.Err()
	}
}

// Connect dials an address given out of band (section 4.1) and returns the
// key of the peer that answered. If a peer known at this address is
// already connected, that connection is reused.
func (n *Node) Connect(ctx context.Context, addr string) (string, error) {
	a, err := transport.Parse(addr)
	if err != nil {
		return "", err
	}
	var existing string
	n.st.DB().QueryRow(`SELECT peer FROM addrs WHERE addr = ? ORDER BY last_ok DESC LIMIT 1`, a.String()).Scan(&existing)
	if existing != "" && n.Connected(existing) {
		n.st.SetParked(existing, false)
		return existing, nil
	}
	key, err := n.dial(ctx, a.String())
	if err != nil {
		return "", err
	}
	n.st.SetParked(key, false)
	return key, nil
}

// dialer is a peer's reconnect loop (section 4.3): exponential backoff
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
	if addrs, _ := store.Addrs(n.st.DB(), key); len(addrs) == 0 {
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
	failures := 0
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
		for _, a := range addrs {
			if n.Connected(d.key) {
				return
			}
			ctx, cancel := context.WithTimeout(n.ctx, 30*time.Second)
			key, err := n.dial(ctx, a.Addr)
			cancel()
			if err == nil && key != d.key {
				err = fmt.Errorf("address now belongs to %s", wire.ShortKey(key))
			}
			if err == nil {
				return
			}
			var peerErr errPeerSaid
			if errors.As(err, &peerErr) && (peerErr.e.Code == wire.ErrAuth || peerErr.e.Code == wire.ErrVersion) {
				n.logf("%s refused us: %v", wire.ShortKey(d.key), err)
			}
			d.setErr(err)
			n.st.AddrResult(d.key, a.Addr, err, time.Now())
			failures++
			if failures%3 == 0 {
				if pa, err := transport.Parse(a.Addr); err == nil {
					n.tr.Reset(pa) // start the next attempt from a fresh tailcat node
				}
			}
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
