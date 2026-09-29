package tunnel

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// carrier moves WireGuard datagrams (section 7). send must not block for
// long: WireGuard calls it from its send path.
type carrier interface {
	send(to string, pkt []byte) error
	close() error
}

// pconn is one carrier connection that preserves datagram boundaries.
type pconn interface {
	readPacket() ([]byte, error)
	writePacket([]byte) error
	Close() error
}

// lenConn carries datagrams on a byte stream, each prefixed by its length
// as a 16-bit big-endian integer (sections 7.2 and 7.4).
type lenConn struct {
	c   net.Conn
	r   *bufio.Reader
	wmu sync.Mutex
}

func newLenConn(c net.Conn) *lenConn { return &lenConn{c: c, r: bufio.NewReaderSize(c, 64<<10)} }

func (l *lenConn) readPacket() ([]byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(l.r, h[:]); err != nil {
		return nil, err
	}
	p := make([]byte, binary.BigEndian.Uint16(h[:]))
	if _, err := io.ReadFull(l.r, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (l *lenConn) writePacket(p []byte) error {
	if len(p) > 0xffff {
		return errors.New("datagram too large")
	}
	buf := make([]byte, 2+len(p))
	binary.BigEndian.PutUint16(buf, uint16(len(p)))
	copy(buf[2:], p)
	l.wmu.Lock()
	defer l.wmu.Unlock()
	l.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := l.c.Write(buf)
	return err
}

func (l *lenConn) Close() error { return l.c.Close() }

// mux is a carrier made of connections: accepted ones are addressed as
// "@n", dialed ones by their endpoint value. A send to an endpoint with no
// connection dials in the background and queues a few datagrams meanwhile;
// WireGuard retransmits anything that does not make it.
type mux struct {
	b    *bind
	kind string
	dial func(ctx context.Context, to string) (pconn, error)
	// failed, if set, is told about each failed dial.
	failed func(to string, err error)

	mu       sync.Mutex
	next     int
	accepted map[string]pconn
	dialed   map[string]*slot
	closers  []io.Closer
	done     bool
}

type slot struct {
	c       pconn
	dialing bool
	queue   [][]byte
}

const maxQueued = 16

func newMux(b *bind, kind string, dial func(context.Context, string) (pconn, error)) *mux {
	return &mux{b: b, kind: kind, dial: dial, accepted: map[string]pconn{}, dialed: map[string]*slot{}}
}

// serveListener accepts connections until ln closes.
func (m *mux) serveListener(ln net.Listener, wrap func(net.Conn) pconn) {
	m.addCloser(ln)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			m.accept(wrap(c))
		}
	}()
}

func (m *mux) addCloser(c io.Closer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.done {
		c.Close()
		return
	}
	m.closers = append(m.closers, c)
}

func (m *mux) accept(pc pconn) {
	m.mu.Lock()
	if m.done {
		m.mu.Unlock()
		pc.Close()
		return
	}
	m.next++
	id := fmt.Sprintf("@%d", m.next)
	m.accepted[id] = pc
	m.mu.Unlock()
	go m.read(pc, id, func() {
		m.mu.Lock()
		delete(m.accepted, id)
		m.mu.Unlock()
	})
}

func (m *mux) read(pc pconn, from string, end func()) {
	defer end()
	defer pc.Close()
	for {
		p, err := pc.readPacket()
		if err != nil {
			return
		}
		m.b.deliver(m.kind, from, p)
	}
}

func (m *mux) send(to string, pkt []byte) error {
	if strings.HasPrefix(to, "@") {
		m.mu.Lock()
		pc := m.accepted[to]
		m.mu.Unlock()
		if pc == nil {
			return errors.New(m.kind + ": connection gone")
		}
		return pc.writePacket(pkt)
	}
	m.mu.Lock()
	if m.done {
		m.mu.Unlock()
		return net.ErrClosed
	}
	s := m.dialed[to]
	if s == nil {
		s = &slot{}
		m.dialed[to] = s
	}
	if pc := s.c; pc != nil {
		m.mu.Unlock()
		if err := pc.writePacket(pkt); err == nil {
			return nil
		}
		pc.Close()
		m.mu.Lock()
		if s.c == pc {
			s.c = nil
		}
	}
	if len(s.queue) < maxQueued {
		s.queue = append(s.queue, append([]byte(nil), pkt...))
	}
	if !s.dialing {
		s.dialing = true
		go m.dialSlot(to, s)
	}
	m.mu.Unlock()
	return nil
}

func (m *mux) dialSlot(to string, s *slot) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	pc, err := m.dial(ctx, to)
	cancel()
	m.mu.Lock()
	s.dialing = false
	if err != nil || m.done {
		s.queue = nil
		m.mu.Unlock()
		if pc != nil {
			pc.Close()
		}
		if err != nil && m.failed != nil {
			m.failed(to, err)
		}
		return
	}
	s.c = pc
	q := s.queue
	s.queue = nil
	m.mu.Unlock()
	for _, p := range q {
		pc.writePacket(p)
	}
	go m.read(pc, to, func() {
		m.mu.Lock()
		if s.c == pc {
			s.c = nil
		}
		m.mu.Unlock()
	})
}

func (m *mux) close() error {
	m.mu.Lock()
	m.done = true
	var cs []io.Closer
	cs = append(cs, m.closers...)
	for _, pc := range m.accepted {
		cs = append(cs, pc)
	}
	for _, s := range m.dialed {
		if s.c != nil {
			cs = append(cs, s.c)
		}
	}
	m.mu.Unlock()
	for _, c := range cs {
		c.Close()
	}
	return nil
}
