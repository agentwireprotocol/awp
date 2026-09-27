package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollerprotocol/holler/internal/api"
	"github.com/hollerprotocol/holler/internal/node"
	"github.com/hollerprotocol/holler/wire"
)

// The agent acting counts as activity; dashboards reading does not.
func TestTouches(t *testing.T) {
	for _, c := range []struct {
		method, params string
		want           bool
	}{
		{"touch", ``, true},
		{"send", `{"peer":"x"}`, true},
		{"state", `{}`, true},
		{"read", `{"inbox":true,"unread":true,"mark":true}`, true},
		{"read", `{"peer":"x","th":"t","limit":1000}`, false},
		{"status", ``, false},
		{"presence", ``, false},
		{"threads", `{}`, false},
		{"blobs", `{}`, false},
		{"mirrored", ``, false},
	} {
		if got := touches(c.method, json.RawMessage(c.params)); got != c.want {
			t.Errorf("touches(%s %s) = %v, want %v", c.method, c.params, got, c.want)
		}
	}
}

// testDaemon serves a node listening on a Unix socket in its own home.
func testDaemon(t *testing.T, name string) *Daemon {
	t.Helper()
	home := t.TempDir()
	n, err := node.Open(node.Config{
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
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return &Daemon{n: n, cfg: Config{Home: home, Name: name}, started: time.Now()}
}

// Connecting to a peer that is already connected says so, by address and
// by name, and leaves the connection alone.
func TestConnectExisting(t *testing.T) {
	a, b := testDaemon(t, "a"), testDaemon(t, "b")
	ctx := context.Background()
	addr := b.n.Addresses()[0]
	res, err := a.connect(ctx, api.ConnectParams{Address: addr})
	if err != nil {
		t.Fatal(err)
	}
	if res.Existing || res.Peer.Key != b.n.Key() {
		t.Fatalf("first connect: %+v", res)
	}
	since := a.n.ConnInfo(b.n.Key()).Since
	for _, ref := range []string{addr, "b", b.n.Key()} {
		res, err := a.connect(ctx, api.ConnectParams{Address: ref})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Existing || !res.Peer.Connected {
			t.Fatalf("connect %s again: %+v", ref, res)
		}
	}
	if ci := a.n.ConnInfo(b.n.Key()); ci == nil || !ci.Since.Equal(since) {
		t.Fatalf("connection was replaced: %+v", ci)
	}
}

// A read that does not mark leaves the messages unread for the next one.
func TestReadNoMark(t *testing.T) {
	a, b := testDaemon(t, "a"), testDaemon(t, "b")
	ctx := context.Background()
	if _, err := a.connect(ctx, api.ConnectParams{Address: b.n.Addresses()[0]}); err != nil {
		t.Fatal(err)
	}
	res, err := a.n.Send(node.SendRequest{Peer: b.n.Key(), Subject: "Read", Parts: []wire.Part{{K: wire.PartText, Text: "needle"}}})
	if err != nil {
		t.Fatal(err)
	}
	unread := func() int {
		t.Helper()
		r, err := b.read(api.ReadParams{Th: res.Th, Inbox: true, Unread: true})
		if err != nil {
			t.Fatal(err)
		}
		return len(r.Events)
	}
	deadline := time.After(10 * time.Second)
	for unread() == 0 {
		select {
		case <-b.n.Changed():
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatal("message never arrived")
		}
	}
	if r, _ := b.read(api.ReadParams{Th: res.Th}); len(r.Events) != 1 || unread() != 1 {
		t.Fatalf("read without mark consumed the message: %+v", r.Events)
	}
	if r, _ := b.read(api.ReadParams{Th: res.Th, Mark: true}); len(r.Events) != 1 || unread() != 0 {
		t.Fatalf("read with mark left the message unread: %+v", r.Events)
	}
}

// link connects from to to and waits until both sides see it.
func link(t *testing.T, from, to *Daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := from.connect(ctx, api.ConnectParams{Address: to.n.Addresses()[0]}); err != nil {
		t.Fatalf("connect %s -> %s: %v", from.n.Name(), to.n.Name(), err)
	}
	waitFor(t, to.n, "inbound connection", func() bool { return to.n.Connected(from.n.Key()) })
}

// waitFor polls cond at every state change of n until it holds.
func waitFor(t *testing.T, n *node.Node, what string, cond func() bool) {
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

func text(s string) []wire.Part {
	return []wire.Part{{K: wire.PartText, Text: s}}
}

func counts(t *testing.T, d *Daemon) (unread, notices int) {
	t.Helper()
	st, err := d.status()
	if err != nil {
		t.Fatal(err)
	}
	return st.Unread, st.Notices
}

// Notices are counted apart from unread messages. A wait does not end on
// them: they are returned, marked read, with whatever does end it.
func TestWaitNotices(t *testing.T) {
	a, b := testDaemon(t, "a"), testDaemon(t, "b")
	// b shares its conversations with a; its hello brings a the notice.
	if err := b.n.SetShareWith([]string{a.n.Key()}); err != nil {
		t.Fatal(err)
	}
	link(t, b, a)
	waitFor(t, a.n, "shares notice at a", func() bool { _, k := counts(t, a); return k == 1 })
	if unread, _ := counts(t, a); unread != 0 {
		t.Errorf("a counts the notice as unread: %d", unread)
	}

	ctx := context.Background()
	res, err := a.wait(ctx, api.WaitParams{TimeoutMS: 300})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || len(res.Events) != 1 || res.Events[0].Type != "shares" {
		t.Fatalf("wait with a notice alone = %+v, want a timeout carrying the notice", res)
	}
	if _, k := counts(t, a); k != 0 {
		t.Errorf("the notice is still unread after the wait")
	}

	// A notice, then a message: the message ends the wait and both come back.
	b.n.Bye(a.n.Key(), "brb")
	waitFor(t, a.n, "bye notice at a", func() bool { _, k := counts(t, a); return k == 1 })
	link(t, b, a)
	go func() {
		time.Sleep(200 * time.Millisecond)
		b.n.Send(node.SendRequest{Peer: a.n.Key(), Parts: text("hello")})
	}()
	res, err = a.wait(ctx, api.WaitParams{TimeoutMS: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if res.TimedOut || len(res.Events) != 2 || res.Events[0].Type != "bye" || res.Events[1].Type != wire.TMsg {
		t.Fatalf("wait after a notice and a message = %+v, want both", res)
	}
	if unread, notices := counts(t, a); unread != 0 || notices != 0 {
		t.Errorf("after the wait: unread %d, notices %d", unread, notices)
	}
}

// A wait for a state ends on the state, or on a message or state change in
// the thread meanwhile, and says which it was.
func TestWaitState(t *testing.T) {
	a, b := testDaemon(t, "a"), testDaemon(t, "b")
	link(t, a, b)
	ctx := context.Background()
	sent, err := a.send(ctx, api.SendParams{Peer: b.n.Key(), Subject: "Task", Parts: text("do the thing")})
	if err != nil {
		t.Fatal(err)
	}
	th := sent.Th
	waitFor(t, b.n, "thread at b", func() bool { ts, _ := b.n.Store().Threads(a.n.Key(), th); return len(ts) == 1 })

	// The peer says working: that is a change in the thread, not the state asked for.
	if _, err := b.n.SetState(a.n.Key(), th, "working", "cloning"); err != nil {
		t.Fatal(err)
	}
	p := api.WaitParams{Th: th, States: []string{"done", "failed"}, TimeoutMS: 5000}
	res, err := a.wait(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if res.TimedOut || res.Matched || len(res.Events) != 1 || res.Events[0].Type != wire.TState || res.Thread.TheirState != "working" {
		t.Fatalf("wait on a state change = %+v, want the change, unmatched", res)
	}

	// A message in the thread ends the wait too.
	go func() {
		time.Sleep(200 * time.Millisecond)
		b.n.Send(node.SendRequest{Peer: a.n.Key(), Th: th, Parts: text("3 of 42 failing")})
	}()
	if res, err = a.wait(ctx, p); err != nil {
		t.Fatal(err)
	}
	if res.TimedOut || res.Matched || len(res.Events) != 1 || res.Events[0].Type != wire.TMsg || res.Thread.TheirState != "working" {
		t.Fatalf("wait on a message = %+v, want the message, unmatched", res)
	}

	// The state asked for.
	go func() {
		time.Sleep(200 * time.Millisecond)
		b.n.SetState(a.n.Key(), th, "done", "all green")
	}()
	if res, err = a.wait(ctx, p); err != nil {
		t.Fatal(err)
	}
	if res.TimedOut || !res.Matched || len(res.Events) != 1 || res.Thread.TheirState != "done" || res.Thread.TheirNote != "all green" {
		t.Fatalf("wait on done = %+v, want matched", res)
	}
	// Already there: no need to wait.
	if res, err = a.wait(ctx, p); err != nil || !res.Matched || len(res.Events) != 0 {
		t.Fatalf("wait when already done = %+v, %v", res, err)
	}
}

// On a live link send waits a moment for the ack, unless told not to. To a
// peer that is away it returns at once.
func TestSendAck(t *testing.T) {
	a, b := testDaemon(t, "a"), testDaemon(t, "b")
	link(t, a, b)
	ctx := context.Background()
	res, err := a.send(ctx, api.SendParams{Peer: b.n.Key(), Parts: text("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Connected || !res.Acked {
		t.Errorf("send on a live link = %+v, want acked", res)
	}
	if res, err = a.send(ctx, api.SendParams{Peer: b.n.Key(), Parts: text("hi"), WaitAck: -1}); err != nil || res.Acked {
		t.Errorf("send without waiting = %+v, %v, want not acked", res, err)
	}

	b.n.Close()
	waitFor(t, a.n, "b gone", func() bool { return !a.n.Connected(b.n.Key()) })
	start := time.Now()
	res, err = a.send(ctx, api.SendParams{Peer: b.n.Key(), Parts: text("later")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Connected || res.Acked || time.Since(start) > 500*time.Millisecond {
		t.Errorf("send to an absent peer = %+v after %v, want queued at once", res, time.Since(start))
	}
}
