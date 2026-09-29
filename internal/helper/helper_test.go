package helper

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentwireprotocol/awp/node"
	"github.com/agentwireprotocol/awp/tunnel"
	"github.com/agentwireprotocol/awp/wire"
)

func tempDir(t *testing.T) string {
	d, err := os.MkdirTemp("", "awph")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func startNode(t *testing.T) *node.Node {
	home := tempDir(t)
	n, err := node.Open(node.Config{Home: home, Name: "go@test", Listen: []string{"unix:" + filepath.Join(home, "s")},
		HandshakeTimeout: 5 * time.Second, MaxBackoff: time.Second, Logf: func(f string, a ...any) { t.Logf("[node] "+f, a...) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

type sdk struct {
	t      *testing.T
	key    string
	events *bufio.Scanner
	ready  Event
	socket string
	fwd    net.Listener
}

// startHelper runs a helper in-process, as the SDK would run `awp tunnel`.
func startHelper(t *testing.T, listen ...string) *sdk {
	dir := tempDir(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	fwd, err := net.Listen("unix", filepath.Join(dir, "fwd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fwd.Close() })
	outR, outW := io.Pipe()
	inR, inW := io.Pipe()
	cfg := Config{Identity: priv, StateDir: dir, Listen: listen, Socket: filepath.Join(dir, "ctl.sock"), Forward: filepath.Join(dir, "fwd.sock"), Logf: t.Logf}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), cfg, inR, outW) }()
	t.Cleanup(func() {
		inW.Close() // stdin closing stops the helper
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("helper: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("helper did not stop when stdin closed")
		}
	})
	s := &sdk{t: t, key: wire.FormatKey(priv.Public().(ed25519.PublicKey)), events: bufio.NewScanner(outR), socket: cfg.Socket, fwd: fwd}
	s.ready = s.event()
	if s.ready.Event != "ready" || s.ready.Key != s.key {
		t.Fatalf("first event %+v", s.ready)
	}
	return s
}

func (s *sdk) event() Event {
	s.t.Helper()
	if !s.events.Scan() {
		s.t.Fatal("helper stdout ended")
	}
	var ev Event
	if err := json.Unmarshal(s.events.Bytes(), &ev); err != nil {
		s.t.Fatal(err)
	}
	return ev
}

func (s *sdk) hello() string {
	return fmt.Sprintf(`{"t":"hello","id":"%s","ts":"%s","v":%d,"key":"%s","name":"sdk@test","caps":["chat"]}`, wire.NewIDGen().New(), wire.Now(), wire.Version, s.key)
}

func readType(t *testing.T, r *bufio.Reader, want string) []byte {
	t.Helper()
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatalf("waiting for %s: %v", want, err)
	}
	env, err := wire.ParseEnvelope(line)
	if err != nil || env.T != want {
		t.Fatalf("want %s, got %s", want, line)
	}
	return line
}

func TestDialThroughHelper(t *testing.T) {
	n := startNode(t)
	s := startHelper(t)
	c, err := net.Dial("unix", s.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	fmt.Fprintf(c, `{"dial":["%s"]}`+"\n", n.Address())
	r := bufio.NewReader(c)
	line, _ := r.ReadBytes('\n')
	var rep dialReply
	if json.Unmarshal(line, &rep); !rep.OK || rep.Key != n.Key() {
		t.Fatalf("dial reply %s", line)
	}
	fmt.Fprintln(c, s.hello())
	readType(t, r, wire.THello)
	readType(t, r, wire.TResume)
	fmt.Fprintf(c, `{"t":"resume","id":"r1","ts":"%s","seen":{}}`+"\n", wire.Now())
	fmt.Fprintf(c, `{"t":"msg","id":"%s","ts":"%s","th":"t1","parts":[{"k":"text","text":"through the helper"}]}`+"\n", wire.NewIDGen().New(), wire.Now())
	readType(t, r, wire.TAck)
	if !n.Connected(s.key) {
		t.Fatal("node does not see the SDK's key")
	}

	// A bad request is answered, not hung up on.
	c2, _ := net.Dial("unix", s.socket)
	defer c2.Close()
	fmt.Fprintln(c2, `{"dial":["tcp:127.0.0.1:1"]}`)
	line, _ = bufio.NewReader(c2).ReadBytes('\n')
	if json.Unmarshal(line, &rep); rep.OK || rep.Error == "" {
		t.Fatalf("bad dial reply %s", line)
	}
}

func TestAcceptThroughHelper(t *testing.T) {
	s := startHelper(t, "unix:"+filepath.Join(tempDir(t), "h.sock"))
	addr := s.ready.Address
	if addr == "" {
		addr = s.event().Address
	}
	a, err := tunnel.ParseAddress(addr)
	if err != nil || a.PSK == nil || a.KeyString() != s.key {
		t.Fatalf("helper address %q: %v", addr, err)
	}
	if p, _ := tunnel.ParseAddress(s.ready.Public); s.ready.Public != "" && p.PSK != nil {
		t.Fatal("public address carries the psk")
	}

	n := startNode(t)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := n.Connect(ctx, addr); err != nil {
			t.Logf("connect: %v", err)
		}
	}()
	c, err := s.fwd.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	r := bufio.NewReader(c)
	fmt.Fprintln(c, s.hello())
	h := readType(t, r, wire.THello)
	if !strings.Contains(string(h), n.Key()) {
		t.Fatalf("hello %s", h)
	}
	readType(t, r, wire.TResume)
	fmt.Fprintf(c, `{"t":"resume","id":"r1","ts":"%s","seen":{}}`+"\n", wire.Now())
	deadline := time.Now().Add(10 * time.Second)
	for !n.Connected(s.key) {
		if time.Now().After(deadline) {
			t.Fatal("node never connected")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A peer whose hello names another key is refused by the helper; the
	// SDK sees the stream end without the lie.
	_, priv, _ := ed25519.GenerateKey(nil)
	tu, err := tunnel.New(tunnel.Config{Identity: priv})
	if err != nil {
		t.Fatal(err)
	}
	defer tu.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := tu.Dial(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	other, _, _ := ed25519.GenerateKey(nil)
	fmt.Fprintf(st, `{"t":"hello","id":"x","ts":"%s","v":%d,"key":"%s"}`+"\n", wire.Now(), wire.Version, wire.FormatKey(other))
	fc, err := s.fwd.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer fc.Close()
	fc.SetDeadline(time.Now().Add(20 * time.Second))
	if b, _ := io.ReadAll(fc); strings.Contains(string(b), "hello") {
		t.Fatalf("the SDK got the lying hello: %s", b)
	}
	st.SetDeadline(time.Now().Add(20 * time.Second))
	sr := bufio.NewReader(st)
	for {
		line, err := sr.ReadBytes('\n')
		if err != nil {
			t.Fatal("no err auth before close")
		}
		var e wire.Err
		if json.Unmarshal(line, &e); e.T == wire.TErr {
			if e.Code != wire.ErrAuth {
				t.Fatalf("err %+v", e)
			}
			break
		}
	}
}
