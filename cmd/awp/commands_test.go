package main

import (
	"testing"
	"time"

	"github.com/agentwireprotocol/awp/internal/api"
	"github.com/agentwireprotocol/awp/internal/store"
)

func TestUnreadLine(t *testing.T) {
	key := "ed25519:qaLeadKeyBodyXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
	threads := []*store.Thread{
		{Peer: key, Th: "thr_cdt3kavr", Unread: 2, Updated: time.Now().Add(-47 * time.Second)},
		{Peer: key, Th: "thr_read", Unread: 0, Updated: time.Now()},
		{Peer: "ed25519:otherKey", Th: "thr_other", Unread: 1, Updated: time.Now()},
	}
	peers := []api.PeerView{{Key: key, Name: "qa-lead@sprite"}}
	for _, c := range []struct{ th, want string }{
		{"thr_cdt3kavr", "note: 2 unread from qa-lead@sprite in this thread (47s ago): awp read thr_cdt3kavr"},
		{"thr_read", ""},
		{"thr_other", "note: 1 unread from otherKey in this thread (0s ago): awp read thr_other"},
		{"thr_missing", ""},
	} {
		if got := unreadLine(threads, peers, c.th); got != c.want {
			t.Errorf("unreadLine(%s) = %q, want %q", c.th, got, c.want)
		}
	}
}

// Notices are counted apart from unread messages, and only when there are some.
func TestCountsLine(t *testing.T) {
	for _, c := range []struct {
		unread, notices, queued int
		want                    string
	}{
		{0, 0, 0, "unread 0, queued 0"},
		{3, 0, 1, "unread 3, queued 1"},
		{0, 1, 0, "unread 0, 1 notice, queued 0"},
		{0, 2, 0, "unread 0, 2 notices, queued 0"},
	} {
		if got := countsLine(c.unread, c.notices, c.queued); got != c.want {
			t.Errorf("countsLine(%d, %d, %d) = %q, want %q", c.unread, c.notices, c.queued, got, c.want)
		}
	}
}

// A sent message is delivered once acked, delivering while the link is up,
// and queued when the peer is away. Only an explicit wait reports its lapse.
func TestSendStatus(t *testing.T) {
	for _, c := range []struct {
		res    api.SendResult
		waited bool
		want   string
	}{
		{api.SendResult{Connected: true, Acked: true}, false, "delivered"},
		{api.SendResult{Connected: true, Acked: true}, true, "delivered"},
		{api.SendResult{Connected: true}, false, "delivering"},
		{api.SendResult{Connected: true}, true, "not acknowledged yet"},
		{api.SendResult{}, false, "queued until the peer is reachable"},
		{api.SendResult{}, true, "queued until the peer is reachable"},
	} {
		if got := sendStatus(c.res, c.waited); got != c.want {
			t.Errorf("sendStatus(%+v, %v) = %q, want %q", c.res, c.waited, got, c.want)
		}
	}
}

// After a wait for a state, the last line says whether it was reached.
func TestStateLine(t *testing.T) {
	th := &store.Thread{Th: "thr_cdt3kavr", TheirState: "working"}
	if got, want := stateLine("qa-alpha@sprite", th, false), "qa-alpha@sprite is still working on thr_cdt3kavr"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	th = &store.Thread{Th: "thr_cdt3kavr", TheirState: "done", TheirNote: "all green"}
	if got, want := stateLine("qa-alpha@sprite", th, true), "qa-alpha@sprite is done on thr_cdt3kavr (all green)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
