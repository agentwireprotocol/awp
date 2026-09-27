package main

import (
	"testing"
	"time"

	"github.com/hollerprotocol/holler/internal/api"
	"github.com/hollerprotocol/holler/internal/store"
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
		{"thr_cdt3kavr", "note: 2 unread from qa-lead@sprite in this thread (47s ago): holler read thr_cdt3kavr"},
		{"thr_read", ""},
		{"thr_other", "note: 1 unread from otherKey in this thread (0s ago): holler read thr_other"},
		{"thr_missing", ""},
	} {
		if got := unreadLine(threads, peers, c.th); got != c.want {
			t.Errorf("unreadLine(%s) = %q, want %q", c.th, got, c.want)
		}
	}
}
