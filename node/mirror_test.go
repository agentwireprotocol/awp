package node

import (
	"testing"

	"github.com/agentwireprotocol/awp/store"
	"github.com/agentwireprotocol/awp/wire"
)

// A shares its conversations with W (a dashboard host). A's thread with B is
// mirrored to W in both directions; B is told; B can make the thread
// private, and W then forgets it and gets no more of it.
func TestConversationSharing(t *testing.T) {
	w := testNode(t, "w")
	a := testNode(t, "a", func(c *Config) { c.ShareWith = []string{w.Key()} })
	b := testNode(t, "b")
	connect(t, a, w)
	connect(t, a, b)

	mirrored := func(th string) []store.Record {
		recs, err := w.Store().Query(store.Filter{Dirs: []string{"mirror"}, Th: th})
		if err != nil {
			t.Fatal(err)
		}
		return recs
	}

	res, err := a.Send(SendRequest{Peer: b.Key(), Subject: "Fix calc", Parts: text(t, "please fix calc")})
	if err != nil {
		t.Fatal(err)
	}
	th := res.Th
	waitFor(t, b, "a's msg at b", func() bool { ts, _ := b.Store().Threads(a.Key(), ""); return len(ts) == 1 })
	if _, err := b.Send(SendRequest{Peer: a.Key(), Th: th, Parts: text(t, "on it")}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SetState(a.Key(), th, wire.StateWorking, "reading calc.py"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, w, "three mirrored lines at w", func() bool { return len(mirrored(th)) == 3 })
	recs := mirrored(th)
	if recs[0].Peer != a.Key() || recs[0].Meta["of"] != b.Key() || recs[0].Meta["from"] != a.Key() || recs[0].Meta["subject"] != "Fix calc" {
		t.Errorf("a's own msg mirrored as %+v", recs[0])
	}
	if recs[1].Meta["from"] != b.Key() || recs[2].T != wire.TState {
		t.Errorf("b's lines mirrored as %+v / %+v", recs[1], recs[2])
	}
	// Mirrored lines are other agents' threads: not in w's normal log.
	if all, _ := w.Store().Query(store.Filter{Th: th}); len(all) != 0 {
		t.Errorf("mirrored lines leaked into the default query: %d", len(all))
	}

	// B was told that A shares, and with whom.
	waitFor(t, b, "a's shares notice at b", func() bool {
		recs, _ := b.Store().Query(store.Filter{Peer: a.Key(), Dirs: []string{"sys"}, Types: []string{"shares"}})
		return len(recs) == 1
	})
	notice, _ := b.Store().Query(store.Filter{Peer: a.Key(), Inbox: true, Types: []string{"shares"}})
	if len(notice) != 1 {
		t.Errorf("the shares notice is not in b's inbox: %d", len(notice))
	}

	// B keeps the thread private: A is told, W forgets it.
	if err := b.MakePrivate(a.Key(), th); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, "b's private request at a", func() bool {
		recs, _ := a.Store().Query(store.Filter{Peer: b.Key(), Dirs: []string{"sys"}, Types: []string{"private"}})
		return len(recs) == 1
	})
	waitFor(t, w, "w forgets the thread", func() bool { return len(mirrored(th)) == 0 })

	// Later lines of the thread are no longer mirrored.
	if _, err := a.Send(SendRequest{Peer: b.Key(), Th: th, Parts: text(t, "thanks")}); err != nil {
		t.Fatal(err)
	}
	other, err := a.Send(SendRequest{Peer: b.Key(), Subject: "Another", Parts: text(t, "a new thread")})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, w, "the other thread is still shared", func() bool { return len(mirrored(other.Th)) == 1 })
	if n := len(mirrored(th)); n != 0 {
		t.Errorf("a private thread was mirrored again: %d lines", n)
	}
}

// Sharing can be stopped at run time, and is remembered across restarts.
func TestShareWithRemembered(t *testing.T) {
	w := testNode(t, "w")
	home := t.TempDir()
	open := func(cfg Config) *Node {
		t.Helper()
		cfg.Home = home
		n, err := Open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	n := open(Config{ShareWith: []string{w.Key()}})
	if got := n.ShareWith(); len(got) != 1 || got[0] != w.Key() {
		t.Fatalf("share with %v", got)
	}
	n.Close()
	n = open(Config{})
	if got := n.ShareWith(); len(got) != 1 {
		t.Fatalf("not remembered: %v", got)
	}
	if err := n.SetShareWith(nil); err != nil {
		t.Fatal(err)
	}
	n.Close()
	n = open(Config{})
	defer n.Close()
	if got := n.ShareWith(); len(got) != 0 {
		t.Fatalf("stopping was not remembered: %v", got)
	}
	if err := n.SetShareWith([]string{n.Key()}); err == nil {
		t.Error("shared with itself")
	}
}

// Mirror lines are queued like msgs: a dashboard that is away gets them on
// its next connection.
func TestMirrorsWaitForTheirHost(t *testing.T) {
	w := testNode(t, "w")
	a := testNode(t, "a", func(c *Config) { c.ShareWith = []string{w.Key()} })
	b := testNode(t, "b")
	connect(t, a, b)
	res, err := a.Send(SendRequest{Peer: b.Key(), Subject: "Queued", Parts: text(t, "while w is away")})
	if err != nil {
		t.Fatal(err)
	}
	connect(t, a, w)
	waitFor(t, w, "the queued mirror at w", func() bool {
		recs, _ := w.Store().Query(store.Filter{Dirs: []string{"mirror"}, Th: res.Th})
		return len(recs) == 1
	})
}
