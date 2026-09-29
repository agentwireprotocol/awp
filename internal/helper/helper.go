// Package helper is `awp tunnel`: the WireGuard tunnel of SPEC.md sections
// 4 to 7 as a separate process, for SDKs in languages without a WireGuard
// implementation (section 18.2). The SDK keeps its identity and speaks the
// protocol from section 8 on; the helper moves its streams.
//
// One helper serves one peer. It prints JSON lines on stdout: a "ready"
// event, then an "address" event each time the address changes. It exits
// when stdin closes, so it never outlives the SDK that started it.
//
// Requests: the SDK connects to the control socket and writes one line.
//
//	{"dial": ["awp1...", ...], "key": "ed25519:..."}
//
// dials a peer (addresses of one peer, and/or a key the tunnel already
// knows); the helper answers {"ok":true,"key":...,"via":...} or
// {"ok":false,"error":...}, and the connection becomes the stream.
//
//	{"listen": "tailcat"}
//
// adds a carrier and answers, once it is up, {"ok":true,"address":...,
// "public":...}.
//
//	{"rotate": true}
//
// replaces the listener's pre-shared key and answers with the new address.
//
// Accepting: each stream a peer opens is connected to the SDK's forward
// socket. The helper first writes one line of its own,
//
//	{"tunnel": {"remote": "<the peer's X25519 key, base64url>"}}
//
// so the SDK knows which grants to present in its hello before the peer's
// hello arrives; then it splices the stream unchanged. A dial reply carries
// the same "remote".
//
// In both directions the helper reads the peer's first line and, if it is
// a hello whose key is not the tunnel's, sends err auth and closes, so the
// SDK can trust hello.key as section 10.1 requires.
package helper

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/agentwireprotocol/awp/tunnel"
	"github.com/agentwireprotocol/awp/wire"
)

// Config configures a helper.
type Config struct {
	Identity ed25519.PrivateKey
	StateDir string   // pre-shared keys and the tailcat key; empty keeps them in memory
	Listen   []string // carriers, as tunnel.Config.Listen
	Socket   string   // the control socket to create
	Forward  string   // where inbound streams go: a unix socket path, or tcp:HOST:PORT
	Logf     func(string, ...any)
}

// Event is one line on stdout.
type Event struct {
	Event   string `json:"event"` // ready or address
	Key     string `json:"key"`
	Address string `json:"address,omitempty"` // with the pre-shared key: the one to share
	Public  string `json:"public,omitempty"`  // without: for hello.addr
	Socket  string `json:"socket,omitempty"`
}

// Run serves until ctx ends or stdin closes.
func Run(ctx context.Context, cfg Config, stdin io.Reader, stdout io.Writer) error {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Socket == "" {
		return errors.New("no control socket")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	key := wire.FormatKey(cfg.Identity.Public().(ed25519.PublicKey))

	var outMu sync.Mutex
	emit := func(ev Event) {
		outMu.Lock()
		defer outMu.Unlock()
		b, _ := json.Marshal(ev)
		stdout.Write(append(b, '\n'))
	}
	var tu *tunnel.Tunnel
	changed := make(chan struct{}, 1)
	tu, err := tunnel.New(tunnel.Config{
		Identity: cfg.Identity,
		StateDir: cfg.StateDir,
		Listen:   cfg.Listen,
		Logf:     cfg.Logf,
		Changed: func() {
			select {
			case changed <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		return err
	}
	defer tu.Close()
	if err := tu.Start(); err != nil {
		return err
	}
	os.Remove(cfg.Socket)
	ln, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	defer os.Remove(cfg.Socket)

	addrEvent := func(name string) Event {
		ev := Event{Event: name, Key: key, Socket: cfg.Socket}
		if a := tu.Address(); len(a.Endpoints) > 0 {
			ev.Address, ev.Public = a.String(), a.Public().String()
		}
		return ev
	}
	emit(addrEvent("ready"))
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-changed:
				emit(addrEvent("address"))
			}
		}
	}()

	// Stdin closing means the SDK is gone.
	if stdin != nil {
		go func() {
			io.Copy(io.Discard, stdin)
			cancel()
		}()
	}
	go func() {
		<-ctx.Done()
		ln.Close()
		tu.Close()
	}()

	go func() {
		for {
			s, err := tu.Accept()
			if err != nil {
				return
			}
			go forward(cfg, s)
		}
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go dial(ctx, cfg, tu, c)
	}
}

type request struct {
	Dial   []string `json:"dial,omitempty"`
	Key    string   `json:"key,omitempty"`
	Listen string   `json:"listen,omitempty"`
	Rotate bool     `json:"rotate,omitempty"`
}

type dialReply struct {
	OK      bool   `json:"ok"`
	Key     string `json:"key,omitempty"`
	Remote  string `json:"remote,omitempty"`
	Via     string `json:"via,omitempty"`
	Address string `json:"address,omitempty"`
	Public  string `json:"public,omitempty"`
	Error   string `json:"error,omitempty"`
}

func dial(ctx context.Context, cfg Config, tu *tunnel.Tunnel, c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	line, err := br.ReadBytes('\n')
	if err != nil {
		return
	}
	c.SetReadDeadline(time.Time{})
	reply := func(r dialReply) error {
		b, _ := json.Marshal(r)
		_, err := c.Write(append(b, '\n'))
		return err
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		reply(dialReply{Error: "bad request: " + err.Error()})
		return
	}
	switch {
	case req.Listen != "":
		reply(listen(ctx, tu, req.Listen))
		return
	case req.Rotate:
		if err := tu.RotatePSK(); err != nil {
			reply(dialReply{Error: err.Error()})
			return
		}
		reply(addressReply(tu))
		return
	}
	var addrs []tunnel.Address
	for _, s := range req.Dial {
		a, err := tunnel.ParseAddress(s)
		if err != nil {
			reply(dialReply{Error: err.Error()})
			return
		}
		addrs = append(addrs, a)
	}
	if req.Key != "" {
		pub, err := wire.ParseKey(req.Key)
		if err != nil {
			reply(dialReply{Error: err.Error()})
			return
		}
		addrs = append(addrs, tunnel.Address{Key: pub})
	}
	if len(addrs) == 0 {
		reply(dialReply{Error: "nothing to dial"})
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	s, err := tu.Dial(dctx, addrs...)
	cancel()
	if err != nil {
		reply(dialReply{Error: err.Error()})
		return
	}
	defer s.Close()
	if err := reply(dialReply{OK: true, Key: addrs[0].KeyString(), Remote: s.Remote.String(), Via: s.Via()}); err != nil {
		return
	}
	splice(cfg, &bufConn{Conn: c, r: br}, s)
}

func addressReply(tu *tunnel.Tunnel) dialReply {
	r := dialReply{OK: true}
	if a := tu.Address(); len(a.Endpoints) > 0 {
		r.Address, r.Public = a.String(), a.Public().String()
	}
	return r
}

// listen adds a carrier and waits, up to 90 seconds, for it to come up.
func listen(ctx context.Context, tu *tunnel.Tunnel, spec string) dialReply {
	if err := tu.Listen(spec); err != nil {
		return dialReply{Error: err.Error()}
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		e, pending := tu.Pending()[spec]
		switch {
		case !pending:
			return addressReply(tu)
		case e != "":
			return dialReply{Error: spec + ": " + e}
		case time.Now().After(deadline):
			return dialReply{Error: spec + ": not up after 90s"}
		}
		select {
		case <-ctx.Done():
			return dialReply{Error: "shutting down"}
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func forward(cfg Config, s *tunnel.Stream) {
	defer s.Close()
	network, target := "unix", cfg.Forward
	if t, ok := strings.CutPrefix(cfg.Forward, "tcp:"); ok {
		network, target = "tcp", t
	}
	if target == "" {
		return
	}
	c, err := net.DialTimeout(network, target, 10*time.Second)
	if err != nil {
		cfg.Logf("forward to %s: %v", cfg.Forward, err)
		return
	}
	defer c.Close()
	pre, _ := json.Marshal(map[string]any{"tunnel": map[string]string{"remote": s.Remote.String()}})
	if _, err := c.Write(append(pre, '\n')); err != nil {
		return
	}
	splice(cfg, c, s)
}

// splice copies both ways between the SDK's end and the tunnel stream,
// checking that the peer's first line, if it is a hello, names the
// tunnel's key.
func splice(cfg Config, local net.Conn, s *tunnel.Stream) {
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(s, local)
		s.CloseWrite()
		done <- struct{}{}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		defer closeWrite(local)
		r := bufio.NewReaderSize(s, 64<<10)
		first, err := r.ReadSlice('\n')
		if len(first) > 0 {
			if msg := badHello(first, s); msg != "" {
				cfg.Logf("closing a stream: %s", msg)
				e, _ := wire.Encode(wire.Err{Envelope: wire.Envelope{T: wire.TErr, ID: wire.NewIDGen().New(), TS: wire.Now()}, Code: wire.ErrAuth, Detail: msg})
				s.Write(append(e, '\n'))
				s.Close()
				local.Close()
				return
			}
			if _, err := local.Write(first); err != nil {
				return
			}
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return
		}
		io.Copy(local, r)
	}()
	<-done
	<-done
}

// badHello says what is wrong with a first line that is a hello naming a
// key other than the tunnel's, or "".
func badHello(line []byte, s *tunnel.Stream) string {
	var h struct {
		T   string `json:"t"`
		Key string `json:"key"`
	}
	if json.Unmarshal(line, &h) != nil || h.T != wire.THello {
		return ""
	}
	pub, err := wire.ParseKey(h.Key)
	if err != nil {
		return "" // the SDK answers a malformed key itself
	}
	if !s.Is(pub) {
		return fmt.Sprintf("hello key %s is not the key on the other end of the tunnel", wire.ShortKey(h.Key))
	}
	return ""
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
		return
	}
	if b, ok := c.(*bufConn); ok {
		closeWrite(b.Conn)
	}
}

// LoadIdentity reads an identity file as the SDKs and the daemon write it:
// JSON with "seed", 32 bytes of base64url.
func LoadIdentity(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Seed string `json:"seed"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return SeedKey(f.Seed)
}

// SeedKey decodes a base64 seed into a key.
func SeedKey(seed string) (ed25519.PrivateKey, error) {
	raw, err := wire.DecodeB64(strings.TrimSpace(seed))
	if err != nil || len(raw) != ed25519.SeedSize {
		return nil, errors.New("identity seed must be 32 bytes of base64")
	}
	return ed25519.NewKeyFromSeed(raw), nil
}
