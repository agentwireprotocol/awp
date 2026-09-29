package tunnel

import (
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"

	"github.com/tailscale/wireguard-go/conn"
)

// bind is the WireGuard conn.Bind over every carrier. WireGuard sees one
// endpoint per peer (peerEP); the bind decides which carrier path a packet
// takes. A peer's current path is the one its last authenticated packet
// came from; handshake packets also go to every candidate path, so a peer
// that moved or slept is found again (section 7).
type bind struct {
	t *Tunnel

	mu     sync.Mutex
	peers  map[Key]*peerEP
	open   bool
	closed chan struct{}
	recv   chan inPacket
}

type inPacket struct {
	data []byte
	from *pathEP
}

type path struct{ kind, value string }

func (p path) String() string { return p.kind + ":" + p.value }

var errNoPath = errors.New("no path to peer")

func newBind(t *Tunnel) *bind {
	return &bind{t: t, peers: map[Key]*peerEP{}, recv: make(chan inPacket, 1024), closed: make(chan struct{})}
}

// deliver hands a received datagram to WireGuard. The bind owns pkt from
// here. A full queue drops the packet, as a UDP socket would.
func (b *bind) deliver(kind, from string, pkt []byte) {
	select {
	case b.recv <- inPacket{pkt, &pathEP{b: b, p: path{kind, from}}}:
	default:
	}
}

func (b *bind) peer(k Key) *peerEP {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.peers[k]
	if e == nil {
		e = &peerEP{b: b, key: k}
		b.peers[k] = e
	}
	return e
}

func (b *bind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.open {
		return nil, 0, conn.ErrBindAlreadyOpen
	}
	b.open = true
	closed := make(chan struct{})
	b.closed = closed
	fn := func(slab []byte, pkts []conn.ReceivedPacket) (int, error) {
		select {
		case p := <-b.recv:
			n := copy(slab, p.data)
			pkts[0] = conn.ReceivedPacket{Offset: 0, Size: n, Endpoint: p.from}
			return 1, nil
		case <-closed:
			return 0, net.ErrClosed
		}
	}
	return []conn.ReceiveFunc{fn}, port, nil
}

func (b *bind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.open {
		close(b.closed)
		b.open = false
	}
	return nil
}

func (b *bind) SetMark(uint32) error { return nil }
func (b *bind) BatchSize() int       { return 1 }

func (b *bind) Send(bufs [][]byte, ep conn.Endpoint, offset int) error {
	var err error
	for _, buf := range bufs {
		pkt := buf[offset:]
		switch e := ep.(type) {
		case *peerEP:
			err = e.send(pkt)
		case *pathEP:
			err = b.sendPath(e.p, pkt)
		default:
			return conn.ErrWrongEndpointType
		}
	}
	return err
}

func (b *bind) sendPath(p path, pkt []byte) error {
	c := b.t.carrier(p.kind)
	if c == nil {
		return errors.New("no carrier " + p.kind)
	}
	return c.send(p.value, pkt)
}

// ParseEndpoint accepts "peer:<hex tunnel key>", the form the tunnel uses
// to configure peers, and "kind:value" for a single path.
func (b *bind) ParseEndpoint(s string) (conn.Endpoint, error) {
	if h, ok := strings.CutPrefix(s, "peer:"); ok {
		raw, err := hex.DecodeString(h)
		if err != nil || len(raw) != 32 {
			return nil, errors.New("bad peer endpoint")
		}
		var k Key
		copy(k[:], raw)
		return b.peer(k), nil
	}
	kind, value, ok := strings.Cut(s, ":")
	if !ok {
		return nil, errors.New("bad endpoint " + s)
	}
	return &pathEP{b: b, p: path{kind, value}}, nil
}

// maxCands bounds the candidate paths kept per peer.
const maxCands = 8

// peerEP is the endpoint WireGuard holds for one peer.
type peerEP struct {
	b   *bind
	key Key

	mu    sync.Mutex
	cur   *path  // where the last authenticated packet came from
	cands []path // endpoints from addresses, and paths initiations came from
}

// addCands adds candidate paths, newest first.
func (e *peerEP) addCands(ps ...path) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := append([]path{}, ps...)
	for _, c := range e.cands {
		dup := false
		for _, p := range ps {
			if p == c {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, c)
		}
	}
	if len(out) > maxCands {
		out = out[:maxCands]
	}
	e.cands = out
}

func (e *peerEP) setCur(p path) {
	e.mu.Lock()
	e.cur = &p
	e.mu.Unlock()
}

func (e *peerEP) current() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cur == nil {
		return ""
	}
	return e.cur.String()
}

// WireGuard message types, the first byte of every datagram.
const (
	msgInitiation = 1
	msgResponse   = 2
)

func (e *peerEP) send(pkt []byte) error {
	e.mu.Lock()
	cur := e.cur
	cands := append([]path{}, e.cands...)
	e.mu.Unlock()
	handshake := len(pkt) > 0 && (pkt[0] == msgInitiation || pkt[0] == msgResponse)
	if cur != nil {
		err := e.b.sendPath(*cur, pkt)
		if err == nil && !handshake {
			return nil
		}
		if err != nil && len(cands) == 0 {
			return err
		}
	}
	sent := cur != nil
	err := errNoPath
	for _, c := range cands {
		if cur != nil && c == *cur {
			continue
		}
		if e2 := e.b.sendPath(c, pkt); e2 == nil {
			sent = true
		} else {
			err = e2
		}
	}
	if sent {
		return nil
	}
	return err
}

func (e *peerEP) ClearSrc()           {}
func (e *peerEP) SrcToString() string { return "" }
func (e *peerEP) DstToString() string { return "peer:" + e.key.String() }
func (e *peerEP) DstToBytes() []byte  { return e.key[:] }
func (e *peerEP) DstIP() netip.Addr   { return netip.Addr{} }
func (e *peerEP) SrcIP() netip.Addr   { return netip.Addr{} }

// pathEP is the endpoint of one received datagram. WireGuard tells it which
// peer the datagram was from: before verification for an initiation (the
// path becomes a candidate, so the response can reach it), after
// decryption for everything else (the path becomes current).
type pathEP struct {
	b *bind
	p path
}

func (e *pathEP) InitiationMessagePublicKey(pk [32]byte) {
	e.b.peer(Key(pk)).addCands(e.p)
}

func (e *pathEP) FromPeer(pk [32]byte) { e.b.peer(Key(pk)).setCur(e.p) }

func (e *pathEP) ClearSrc()           {}
func (e *pathEP) SrcToString() string { return "" }
func (e *pathEP) DstToString() string { return e.p.String() }
func (e *pathEP) DstToBytes() []byte  { return []byte(e.p.String()) }
func (e *pathEP) DstIP() netip.Addr   { return netip.Addr{} }
func (e *pathEP) SrcIP() netip.Addr   { return netip.Addr{} }

var (
	_ conn.Bind                    = (*bind)(nil)
	_ conn.InitiationAwareEndpoint = (*pathEP)(nil)
	_ conn.PeerAwareEndpoint       = (*pathEP)(nil)
)
