package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentwireprotocol/awp/store"
	"github.com/agentwireprotocol/awp/wire"
)

// testNode starts a node listening on a Unix socket in its own home.
func testNode(t *testing.T, name string, mod ...func(*Config)) *Node {
	t.Helper()
	home := t.TempDir()
	cfg := Config{
		Home:             home,
		Name:             name,
		Listen:           []string{"unix:" + filepath.Join(home, "s")},
		PingInterval:     2 * time.Second,
		HandshakeTimeout: 5 * time.Second,
		MaxBackoff:       500 * time.Millisecond,
		DialGrace:        300 * time.Millisecond,
		Logf: func(format string, args ...any) {
			t.Logf("[%s] %s", name, fmt.Sprintf(format, args...))
		},
	}
	for _, m := range mod {
		m(&cfg)
	}
	n, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

// restart closes n and opens a new node on the same home and config.
func restart(t *testing.T, n *Node) *Node {
	t.Helper()
	cfg := n.cfg
	n.Close()
	m, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func addr(n *Node) string { return n.Addresses()[0] }

// waitFor polls cond at every state change of n until it holds.
func waitFor(t *testing.T, n *Node, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		ch := n.Changed()
		if cond() {
			return
		}
		select {
		case <-ch:
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func connect(t *testing.T, from, to *Node) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	key, err := from.Connect(ctx, addr(to))
	if err != nil {
		t.Fatalf("connect %s -> %s: %v", from.Name(), to.Name(), err)
	}
	if key != to.Key() {
		t.Fatalf("connected to %s, want %s", key, to.Key())
	}
	waitFor(t, to, "inbound connection", func() bool { return to.Connected(from.Key()) })
}

func received(t *testing.T, n *Node, from string, types ...string) []store.Record {
	t.Helper()
	recs, err := n.Store().Query(store.Filter{Peer: from, Dirs: []string{"in"}, Types: types})
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func text(t *testing.T, s string) []wire.Part {
	return []wire.Part{{K: wire.PartText, Text: s}}
}

func outboxLen(n *Node, peer string) int {
	k, _ := n.Store().OutboxCount(peer)
	return k
}

func TestMessageRoundTrip(t *testing.T) {
	a, b := testNode(t, "a"), testNode(t, "b")
	connect(t, a, b)

	res, err := a.Send(SendRequest{
		Peer:    b.Key(),
		Subject: "Run integration suite",
		Parts: []wire.Part{
			{K: wire.PartText, Text: "Please run `make integration`."},
			{K: wire.PartCode, Lang: "diff", Text: "--- a/x\n+++ b/x"},
			{K: wire.PartData, Mime: "application/json", Data: json.RawMessage(`{"commit":"a1b2c3"}`)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.NewThread || !strings.HasPrefix(res.Th, "thr_") {
		t.Fatalf("send result %+v", res)
	}
	waitFor(t, b, "msg at b", func() bool { return len(received(t, b, a.Key(), "msg")) == 1 })
	waitFor(t, a, "ack at a", func() bool { return outboxLen(a, b.Key()) == 0 })

	var got wire.Msg
	json.Unmarshal(received(t, b, a.Key(), "msg")[0].Line, &got)
	if got.Subject != "Run integration suite" || len(got.Parts) != 3 || got.Parts[2].Mime != "application/json" {
		t.Fatalf("received %+v", got)
	}
	th, _ := b.Store().Threads(a.Key(), res.Th)
	if len(th) != 1 || th[0].Subject != "Run integration suite" || th[0].Origin != "them" {
		t.Fatalf("thread at b: %+v", th)
	}

	// Reply and state in the same thread flow the other way.
	if _, err := b.SetState(a.Key(), res.Th, wire.StateWorking, "cloning"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Send(SendRequest{Peer: a.Key(), Th: res.Th, Re: res.ID, Parts: text(t, "3 of 42 failing")}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, "reply at a", func() bool { return len(received(t, a, b.Key(), "msg", "state")) == 2 })
	ta, _ := a.Store().Threads(b.Key(), res.Th)
	if ta[0].TheirState != wire.StateWorking || ta[0].TheirNote != "cloning" {
		t.Fatalf("thread at a: %+v", ta[0])
	}
	waitFor(t, b, "acks at b", func() bool { return outboxLen(b, a.Key()) == 0 })
	// Sent messages are marked acked in the log.
	out, _ := a.Store().Query(store.Filter{Peer: b.Key(), Dirs: []string{"out"}, Types: []string{"msg"}})
	if len(out) != 1 || !out[0].Acked {
		t.Fatalf("sender log %+v", out)
	}
}

// TestQueuedWhileAway checks that sending never fails when the peer is
// gone, and that the queue is delivered in order when it comes back.
func TestQueuedWhileAway(t *testing.T) {
	a, b := testNode(t, "a"), testNode(t, "b")
	connect(t, a, b)
	bKey := b.Key()
	b.Close()
	waitFor(t, a, "disconnect", func() bool { return !a.Connected(bKey) })

	res, err := a.Send(SendRequest{Peer: bKey, Parts: text(t, "one")})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"two", "three"} {
		if _, err := a.Send(SendRequest{Peer: bKey, Th: res.Th, Parts: text(t, s)}); err != nil {
			t.Fatal(err)
		}
	}
	if res.Connected {
		t.Fatal("send reported connected while the peer was down")
	}
	b = restart(t, b)
	waitFor(t, b, "three messages", func() bool { return len(received(t, b, a.Key(), "msg")) == 3 })
	var order []string
	for _, r := range received(t, b, a.Key(), "msg") {
		var m wire.Msg
		json.Unmarshal(r.Line, &m)
		order = append(order, m.Parts[0].Text)
	}
	if !slices.Equal(order, []string{"one", "two", "three"}) {
		t.Fatalf("order %v", order)
	}
	waitFor(t, a, "outbox drained", func() bool { return outboxLen(a, bKey) == 0 })
}

// TestResumeAfterCrash kills the receiver repeatedly while a stream of
// messages is in flight and checks that exactly the sent set arrives, in
// order, with no duplicates.
func TestResumeAfterCrash(t *testing.T) {
	a, b := testNode(t, "a"), testNode(t, "b")
	connect(t, a, b)
	const total = 300
	th := ""
	bKey := b.Key()
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i := 0; i < total; i++ {
			res, err := a.Send(SendRequest{Peer: bKey, Th: th, Parts: text(t, fmt.Sprint(i))})
			if err != nil {
				t.Error(err)
				return
			}
			th = res.Th
		}
	}()
	for i := 0; i < 3; i++ {
		time.Sleep(time.Duration(50+i*40) * time.Millisecond)
		b = restart(t, b)
	}
	<-sent
	waitFor(t, b, "all messages", func() bool { return len(received(t, b, a.Key(), "msg")) >= total })
	waitFor(t, a, "outbox drained", func() bool { return outboxLen(a, b.Key()) == 0 })
	recs := received(t, b, a.Key(), "msg")
	if len(recs) != total {
		t.Fatalf("got %d messages, want %d", len(recs), total)
	}
	for i, r := range recs {
		var m wire.Msg
		json.Unmarshal(r.Line, &m)
		if m.Parts[0].Text != fmt.Sprint(i) {
			t.Fatalf("message %d is %q", i, m.Parts[0].Text)
		}
	}
}

func TestBlob(t *testing.T) {
	a := testNode(t, "a")
	b := testNode(t, "b", func(c *Config) { c.BlobLimit = 2 << 20 })
	connect(t, a, b)

	data := make([]byte, 1300*1024+17) // five chunks, the last one short
	rand.Read(data)
	path := filepath.Join(t.TempDir(), "build.log")
	os.WriteFile(path, data, 0o600)
	res, err := a.Send(SendRequest{Peer: b.Key(), Parts: text(t, "log attached"), Files: []string{path}})
	if err != nil {
		t.Fatal(err)
	}
	var blob *store.Blob
	waitFor(t, b, "blob complete", func() bool {
		bs, _ := b.Store().Blobs(a.Key())
		if len(bs) == 1 && bs[0].Status == "complete" && bs[0].Name != "" {
			blob = bs[0]
			return true
		}
		return false
	})
	got, err := os.ReadFile(blob.Path)
	if err != nil || sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatalf("blob content differs (%d bytes, err %v)", len(got), err)
	}
	if blob.Name != "build.log" || blob.Size != int64(len(data)) || blob.Th != res.Th {
		t.Fatalf("blob metadata %+v", blob)
	}
	waitFor(t, b, "blob event", func() bool {
		recs, _ := b.Store().Query(store.Filter{Peer: a.Key(), Types: []string{"blob"}})
		return len(recs) == 1 && recs[0].Meta["name"] == "build.log" && recs[0].Th == res.Th
	})
	// The msg's ack retires the chunks queued ahead of it.
	waitFor(t, a, "outbox drained", func() bool { return outboxLen(a, b.Key()) == 0 })

	// Over the receiver's limit: refused, and the sender stops sending.
	big := filepath.Join(t.TempDir(), "big.bin")
	os.WriteFile(big, make([]byte, 3<<20), 0o600)
	if _, err := a.Send(SendRequest{Peer: b.Key(), Th: res.Th, Files: []string{big}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, "blob_refused", func() bool {
		return slices.ContainsFunc(received(t, a, b.Key(), "err"), func(r store.Record) bool { return r.Meta["code"] == wire.ErrBlobRefused })
	})
	waitFor(t, a, "outbox drained after refusal", func() bool { return outboxLen(a, b.Key()) == 0 })
}

// TestBlobSentOnAck checks that the sender's record of an outgoing blob
// turns from queued to sent when the peer acks the msg that carries it.
func TestBlobSentOnAck(t *testing.T) {
	a, b := testNode(t, "a"), testNode(t, "b")
	connect(t, a, b)
	bKey := b.Key()
	b.Close()
	waitFor(t, a, "disconnect", func() bool { return !a.Connected(bKey) })

	path := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(path, []byte("queued while b is away"), 0o600)
	if _, err := a.Send(SendRequest{Peer: bKey, Parts: text(t, "file attached"), Files: []string{path}}); err != nil {
		t.Fatal(err)
	}
	bs, _ := a.Store().Blobs(bKey)
	if len(bs) != 1 || bs[0].Status != "queued" {
		t.Fatalf("blobs before delivery: %+v", bs)
	}
	queued := bs[0].Updated

	b = restart(t, b)
	waitFor(t, a, "blob sent", func() bool {
		bs, _ = a.Store().Blobs(bKey)
		return len(bs) == 1 && bs[0].Status == "sent"
	})
	if !bs[0].Updated.After(queued) || outboxLen(a, bKey) != 0 {
		t.Fatalf("blob %+v with %d lines still queued", bs[0], outboxLen(a, bKey))
	}
}

// rawPeer is a minimal hand-driven peer for negative tests.
type rawPeer struct {
	t    *testing.T
	c    net.Conn
	r    *wire.Reader
	priv ed25519.PrivateKey
	key  string
}

func dialRaw(t *testing.T, n *Node) *rawPeer {
	t.Helper()
	c, err := net.Dial("unix", strings.TrimPrefix(addr(n), "unix:"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	pub, priv, _ := ed25519.GenerateKey(nil)
	c.SetDeadline(time.Now().Add(10 * time.Second))
	return &rawPeer{t: t, c: c, r: wire.NewReader(c), priv: priv, key: wire.FormatKey(pub)}
}

func (p *rawPeer) send(line string) {
	p.t.Helper()
	if _, err := p.c.Write([]byte(line + "\n")); err != nil {
		p.t.Fatal(err)
	}
}

func (p *rawPeer) read() (wire.Envelope, []byte) {
	p.t.Helper()
	line, err := p.r.ReadLine()
	if err != nil {
		p.t.Fatalf("read: %v", err)
	}
	env, err := wire.ParseEnvelope(line)
	if err != nil {
		p.t.Fatalf("parse %q: %v", line, err)
	}
	return env, line
}

// expectErr reads until an err line with code arrives, then expects EOF if
// the code closes the connection.
func (p *rawPeer) expectErr(code string) {
	p.t.Helper()
	for {
		env, line := p.read()
		if env.T != wire.TErr {
			continue
		}
		var e wire.Err
		json.Unmarshal(line, &e)
		if e.Code != code {
			p.t.Fatalf("err code %q (%s), want %q", e.Code, e.Detail, code)
		}
		break
	}
	if wire.ErrCloses(code) {
		if _, err := p.r.ReadLine(); err == nil {
			p.t.Fatalf("connection still open after err %s", code)
		}
	}
}

func (p *rawPeer) hello(v int) string {
	return fmt.Sprintf(`{"t":"hello","id":"%s","ts":"%s","v":%d,"key":"%s","name":"raw","nonce":"%s","caps":["chat"]}`, wire.NewIDGen().New(), wire.Now(), v, p.key, wire.Nonce(32))
}

// handshake completes hello, auth and resume like a real peer would.
func (p *rawPeer) handshake() {
	p.t.Helper()
	my := p.hello(0)
	p.send(my)
	env, theirs := p.read()
	if env.T != wire.THello {
		p.t.Fatalf("first line %s", env.T)
	}
	p.send(fmt.Sprintf(`{"t":"auth","id":"x2","ts":"%s","sig":"%s"}`, wire.Now(), wire.SignAuth(p.priv, []byte(my), theirs)))
	if env, _ := p.read(); env.T != wire.TAuth {
		p.t.Fatalf("expected auth, got %s", env.T)
	}
	if env, _ := p.read(); env.T != wire.TResume {
		p.t.Fatalf("expected resume, got %s", env.T)
	}
	p.send(fmt.Sprintf(`{"t":"resume","id":"x3","ts":"%s","seen":{}}`, wire.Now()))
}

func TestBadAuth(t *testing.T) {
	n := testNode(t, "n")
	p := dialRaw(t, n)
	p.send(p.hello(0))
	p.read() // their hello
	// Sign the wrong transcript.
	p.send(fmt.Sprintf(`{"t":"auth","id":"x","ts":"%s","sig":"%s"}`, wire.Now(), wire.SignAuth(p.priv, []byte("forged"), []byte("transcript"))))
	p.read() // their auth
	p.expectErr(wire.ErrAuth)
}

func TestVersionMismatch(t *testing.T) {
	n := testNode(t, "n")
	p := dialRaw(t, n)
	p.send(p.hello(1))
	p.read()
	p.expectErr(wire.ErrVersion)
}

func TestBadFrame(t *testing.T) {
	n := testNode(t, "n")
	p := dialRaw(t, n)
	p.handshake()
	p.send(`this is not json`)
	p.expectErr(wire.ErrBadFrame)
}

func TestOversizedLine(t *testing.T) {
	n := testNode(t, "n")
	p := dialRaw(t, n)
	p.handshake()
	p.send(`{"t":"msg","pad":"` + strings.Repeat("x", wire.MaxLine) + `"}`)
	p.expectErr(wire.ErrTooLarge)
}

func TestUnknownTypesAndFieldsIgnored(t *testing.T) {
	n := testNode(t, "n")
	p := dialRaw(t, n)
	p.handshake()
	p.send(`{"t":"future_thing","id":"u1","ts":"x","whatever":[1,2,3]}`)
	p.send(`{"t":"msg","id":"01J9ZZZZZZZZZZZZZZZZZZZZZZ","ts":"x","th":"t1","parts":[{"k":"text","text":"hi","mood":"sunny"},{"k":"hologram","uri":"x"}],"priority":"high"}`)
	for {
		env, _ := p.read()
		if env.T == wire.TAck {
			if env.Re != "01J9ZZZZZZZZZZZZZZZZZZZZZZ" || env.Th != "t1" {
				t.Fatalf("ack %+v", env)
			}
			break
		}
	}
	// A replay of the same id is acked again but stored once.
	p.send(`{"t":"msg","id":"01J9ZZZZZZZZZZZZZZZZZZZZZZ","ts":"x","th":"t1","parts":[{"k":"text","text":"hi"}]}`)
	for {
		if env, _ := p.read(); env.T == wire.TAck {
			break
		}
	}
	if k := len(received(t, n, p.key, "msg")); k != 1 {
		t.Fatalf("stored %d copies", k)
	}
	// Still connected: ping gets a pong.
	p.send(`{"t":"ping","id":"p1","ts":"x"}`)
	for {
		env, _ := p.read()
		if env.T == wire.TPong {
			if env.Re != "p1" {
				t.Fatalf("pong re %q", env.Re)
			}
			break
		}
	}
}

func TestDeadConnectionDetected(t *testing.T) {
	n := testNode(t, "n", func(c *Config) { c.PingInterval = 150 * time.Millisecond })
	p := dialRaw(t, n)
	p.handshake()
	waitFor(t, n, "connected", func() bool { return n.Connected(p.key) })
	// Go silent: read nothing, answer nothing.
	start := time.Now()
	waitFor(t, n, "dead connection", func() bool { return !n.Connected(p.key) })
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("took %v to notice", el)
	}
	recs, _ := n.Store().Query(store.Filter{Peer: p.key, Dirs: []string{"sys"}, Types: []string{"disconnected"}})
	if len(recs) == 0 || recs[0].Meta["reason"] != "two missed pongs" {
		t.Fatalf("disconnect records %+v", recs)
	}
}

func TestGrantAndServedExec(t *testing.T) {
	a := testNode(t, "a")
	root := t.TempDir()
	b := testNode(t, "b", func(c *Config) { c.Policy.Serve = []string{CapExec, CapFSRead}; c.Policy.Root = root })
	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hello from b"), 0o644)
	connect(t, a, b)
	execReq := wire.Part{K: wire.PartData, Mime: "application/vnd.awp.exec+json", Data: json.RawMessage(`{"cmd":["sh","-c","echo out; echo err >&2; exit 3"]}`)}

	// Without a grant: forbidden, and not executed.
	res, err := a.Send(SendRequest{Peer: b.Key(), Parts: []wire.Part{execReq}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, "forbidden", func() bool {
		return slices.ContainsFunc(received(t, a, b.Key(), "err"), func(r store.Record) bool { return r.Meta["code"] == wire.ErrForbidden })
	})
	recs := received(t, b, a.Key(), "msg")
	if reqs, _ := recs[0].Meta["requests"].([]any); len(reqs) != 1 || reqs[0].(map[string]any)["allowed"] != false {
		t.Fatalf("request annotation %+v", recs[0].Meta)
	}

	// B grants A exec; the grant reaches A as a held grant.
	if _, err := b.Grant(a.Key(), []string{CapExec}, time.Hour); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, "held grant", func() bool { return len(received(t, a, b.Key(), "grant")) == 1 })
	if caps := b.Caps(a.Key()); !slices.Equal(caps, []string{CapExec}) {
		t.Fatalf("caps %v", caps)
	}

	if _, err := a.Send(SendRequest{Peer: b.Key(), Th: res.Th, Parts: []wire.Part{execReq}}); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	waitFor(t, a, "exec result", func() bool {
		for _, r := range received(t, a, b.Key(), "msg") {
			var m wire.Msg
			json.Unmarshal(r.Line, &m)
			for _, p := range m.Parts {
				if p.Mime == MimeExecResult {
					json.Unmarshal(p.Data, &result)
					return true
				}
			}
		}
		return false
	})
	if result["exit"] != float64(3) || result["stdout"] != "out\n" || result["stderr"] != "err\n" {
		t.Fatalf("exec result %+v", result)
	}

	// fs:read is served but not granted: forbidden. After a grant it works
	// and cannot escape the root.
	b.Grant(a.Key(), []string{CapFSRead}, time.Hour)
	read := func(path string) map[string]any {
		before := len(received(t, a, b.Key(), "msg"))
		a.Send(SendRequest{Peer: b.Key(), Th: res.Th, Parts: []wire.Part{{K: wire.PartData, Mime: "application/vnd.awp.fs-read+json", Data: json.RawMessage(fmt.Sprintf(`{"path":%q}`, path))}}})
		var out map[string]any
		waitFor(t, a, "fs-read result", func() bool {
			msgs := received(t, a, b.Key(), "msg")
			if len(msgs) <= before {
				return false
			}
			var m wire.Msg
			json.Unmarshal(msgs[len(msgs)-1].Line, &m)
			json.Unmarshal(m.Parts[1].Data, &out)
			return true
		})
		return out
	}
	if got := read("notes.txt"); got["content"] != "hello from b" {
		t.Fatalf("fs:read %+v", got)
	}
	if got := read("../../etc/passwd"); got["error"] == nil {
		t.Fatalf("fs:read escaped the root: %+v", got)
	}
}

// TestIntroduce: B introduces A to C. A only admits allow-listed keys and
// keys with grants it honors, and it trusts B with introduce.
func TestIntroduce(t *testing.T) {
	b := testNode(t, "b")
	a := testNode(t, "a", func(c *Config) {
		c.Policy.Accept = AcceptAllowlist
		c.Policy.Allow = []string{b.Key()}
	})
	c := testNode(t, "c")
	connect(t, b, a)
	connect(t, c, b)

	// Without an introduction A refuses C.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Connect(ctx, addr(a)); err == nil || !strings.Contains(err.Error(), "allow list") {
		t.Fatalf("uninvited connect: %v", err)
	}

	if _, err := a.Grant(b.Key(), []string{CapIntroduce, CapExec}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Introduce(c.Key(), a.Key(), []string{CapExec, CapFSWrite}, time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, c, "introduction", func() bool { return len(received(t, c, b.Key(), "introduce")) == 1 })
	p, _ := store.GetPeer(c.Store().DB(), a.Key())
	if p == nil || len(p.Addrs) == 0 || p.Addrs[0].Addr != addr(a) {
		t.Fatalf("introduced peer at c: %+v", p)
	}
	// C dials the introduced address and presents the grant.
	if _, err := c.Connect(ctx, p.Addrs[0].Addr); err != nil {
		t.Fatalf("introduced connect: %v", err)
	}
	// Delegation is attenuated: B holds exec (and introduce) from A, so C
	// gets exec but not fs:write.
	if caps := a.Caps(c.Key()); !slices.Equal(caps, []string{CapExec}) {
		t.Fatalf("delegated caps %v", caps)
	}
	// The introduction grant is bound to A: it gives C nothing on B.
	if caps := b.Caps(c.Key()); len(caps) != 0 {
		t.Fatalf("introduction leaked caps on the introducer: %v", caps)
	}
}

func TestByeParksPeer(t *testing.T) {
	a, b := testNode(t, "a"), testNode(t, "b")
	connect(t, a, b)
	res, _ := a.Send(SendRequest{Peer: b.Key(), Parts: text(t, "hi")})
	waitFor(t, a, "ack", func() bool { return outboxLen(a, b.Key()) == 0 })
	a.SetState(b.Key(), res.Th, wire.StateClosed, "")
	waitFor(t, a, "ack", func() bool { return outboxLen(a, b.Key()) == 0 })
	if err := a.Bye(b.Key(), "done"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, "closed", func() bool { return !a.Connected(b.Key()) && !b.Connected(a.Key()) })
	if recs, _ := b.Store().Query(store.Filter{Peer: a.Key(), Types: []string{"bye"}}); len(recs) != 1 || recs[0].Meta["reason"] != "done" {
		t.Fatalf("bye at b: %+v", recs)
	}
	time.Sleep(time.Second)
	if a.Connected(b.Key()) {
		t.Fatal("reconnected after bye")
	}
	// New traffic lifts the bye.
	a.Send(SendRequest{Peer: b.Key(), Parts: text(t, "one more thing")})
	waitFor(t, a, "reconnected", func() bool { return a.Connected(b.Key()) && outboxLen(a, b.Key()) == 0 })
}

// TestListenerReconnectsViaHello: the dialer goes away for good, and the
// listener, which learned the dialer's address from hello, reconnects to
// deliver what it has queued.
func TestListenerReconnectsViaHello(t *testing.T) {
	a, b := testNode(t, "a"), testNode(t, "b")
	connect(t, a, b) // a dialed b
	aKey := a.Key()
	a.Close()
	waitFor(t, b, "disconnect", func() bool { return !b.Connected(aKey) })
	if _, err := b.Send(SendRequest{Peer: aKey, Parts: text(t, "results are in")}); err != nil {
		t.Fatal(err)
	}
	// Bring a back without any reason of its own to dial b.
	a2 := restart(t, a)
	waitFor(t, a2, "b dialed back", func() bool { return len(received(t, a2, b.Key(), "msg")) == 1 })
}

func TestDuplicateConnectionReplaced(t *testing.T) {
	a, b := testNode(t, "a"), testNode(t, "b")
	connect(t, a, b)
	// A second dial to the same address reuses the connection.
	key, err := a.Connect(context.Background(), addr(b))
	if err != nil || key != b.Key() {
		t.Fatal(err)
	}
	// A raw second connection from a's key is not possible without a's
	// private key, so exercise replacement by dialing again directly.
	if _, err := a.dial(context.Background(), addr(b)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, b, "one active connection", func() bool { return b.Connected(a.Key()) && a.Connected(b.Key()) })
	if _, err := a.Send(SendRequest{Peer: b.Key(), Parts: text(t, "still works")}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, b, "msg", func() bool { return len(received(t, b, a.Key(), "msg")) == 1 })
}

func TestPlainTCPRefusedToPublicAddresses(t *testing.T) {
	a := testNode(t, "a")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := a.Connect(ctx, "tcp:1.1.1.1:7777")
	if err == nil || !strings.Contains(err.Error(), "refusing plain TCP") {
		t.Fatalf("public TCP dial: %v", err)
	}
	// Loopback is fine (the dial fails only because nothing listens).
	_, err = a.Connect(ctx, "tcp:127.0.0.1:1")
	if err == nil || strings.Contains(err.Error(), "refusing") {
		t.Fatalf("loopback TCP dial: %v", err)
	}
}

// An explicit harness is remembered and beats what the environment of a
// later start suggests; a detected one is only a fallback, never remembered.
func TestHarnessPrecedence(t *testing.T) {
	open := func(home string, cfg Config) *Node {
		t.Helper()
		cfg.Home = home
		n, err := Open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	home := t.TempDir()
	n := open(home, Config{Harness: "codex", DetectedHarness: "claude"})
	if n.Harness() != "codex" {
		t.Errorf("explicit harness: %q", n.Harness())
	}
	n.Close()
	n = open(home, Config{DetectedHarness: "claude"})
	if n.Harness() != "codex" {
		t.Errorf("after a restart from another harness's shell: %q, want the remembered codex", n.Harness())
	}
	n.Close()

	home = t.TempDir()
	n = open(home, Config{DetectedHarness: "claude"})
	if n.Harness() != "claude" {
		t.Errorf("detected harness: %q", n.Harness())
	}
	n.Close()
	n = open(home, Config{})
	defer n.Close()
	if n.Harness() != "" {
		t.Errorf("a detected harness was remembered: %q", n.Harness())
	}
}

// TestStateSince checks that each side's since timestamp moves when its
// state or note changes, and only then, at both ends of the thread.
func TestStateSince(t *testing.T) {
	a, b := testNode(t, "a"), testNode(t, "b")
	connect(t, a, b)
	res, err := a.Send(SendRequest{Peer: b.Key(), Subject: "Since", Parts: text(t, "hi")})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, b, "msg at b", func() bool { return len(received(t, b, a.Key(), "msg")) == 1 })
	thread := func(n *Node, peer string) *store.Thread {
		t.Helper()
		ts, err := n.Store().Threads(peer, res.Th)
		if err != nil || len(ts) != 1 {
			t.Fatalf("thread at %s: %v %v", n.Name(), ts, err)
		}
		return ts[0]
	}
	states := func(n int) func() bool {
		return func() bool { return len(received(t, a, b.Key(), "state")) == n }
	}
	// A new thread is open on both sides since it was created.
	if th := thread(b, a.Key()); !th.MySince.Equal(th.Created) || !th.TheirSince.Equal(th.Created) {
		t.Fatalf("new thread at b: %+v", th)
	}
	time.Sleep(10 * time.Millisecond) // timestamps are milliseconds
	if _, err := b.SetState(a.Key(), res.Th, wire.StateWorking, "cloning"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, "state at a", states(1))
	tb, ta := thread(b, a.Key()), thread(a, b.Key())
	if !tb.MySince.After(tb.Created) || !tb.TheirSince.Equal(tb.Created) {
		t.Fatalf("after state at b: %+v", tb)
	}
	if ta.TheirState != wire.StateWorking || !ta.TheirSince.After(ta.Created) || !ta.MySince.Equal(ta.Created) {
		t.Fatalf("after state at a: %+v", ta)
	}
	// Saying the same again does not restart the clock on either side.
	time.Sleep(10 * time.Millisecond)
	if _, err := b.SetState(a.Key(), res.Th, wire.StateWorking, "cloning"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, "repeat at a", states(2))
	if th := thread(b, a.Key()); !th.MySince.Equal(tb.MySince) {
		t.Fatalf("repeat at b: %+v", th)
	}
	if th := thread(a, b.Key()); !th.TheirSince.Equal(ta.TheirSince) {
		t.Fatalf("repeat at a: %+v", th)
	}
	// A new note does.
	time.Sleep(10 * time.Millisecond)
	if _, err := b.SetState(a.Key(), res.Th, wire.StateWorking, "testing"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, "new note at a", states(3))
	if th := thread(b, a.Key()); !th.MySince.After(tb.MySince) {
		t.Fatalf("new note at b: %+v", th)
	}
	if th := thread(a, b.Key()); th.TheirNote != "testing" || !th.TheirSince.After(ta.TheirSince) {
		t.Fatalf("new note at a: %+v", th)
	}
}
