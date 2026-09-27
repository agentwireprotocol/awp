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
		Home:   home,
		Name:   name,
		Listen: []string{"unix:" + filepath.Join(home, "s")},
		Logf:   func(format string, args ...any) { t.Logf("[%s] %s", name, fmt.Sprintf(format, args...)) },
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
