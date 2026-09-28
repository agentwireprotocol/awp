package main

import (
	"testing"
	"time"

	"github.com/agentwireprotocol/awp/wire"
)

func TestAge(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		t    time.Time
		want string
	}{
		{now.Add(-5 * time.Second), "5s"},
		{now.Add(-2 * time.Minute), "2m"},
		{now.Add(-3 * time.Hour), "3h"},
		{now.Add(-47 * time.Hour), "47h"},
		{now.Add(-3 * 24 * time.Hour), "3d"},
		{now.Add(time.Minute), "0s"}, // a clock ahead of ours
	} {
		if got := age(c.t); got != c.want {
			t.Errorf("age(%v) = %q, want %q", c.t, got, c.want)
		}
	}
	if got := ago(now.Add(-2 * time.Minute)); got != "2m ago" {
		t.Errorf("ago = %q", got)
	}
	if got := ago(time.Time{}); got != "-" {
		t.Errorf("ago(zero) = %q", got)
	}
	if got := stateAge("working", now.Add(-2*time.Hour)); got != "working 2h" {
		t.Errorf("stateAge = %q", got)
	}
	if got := stateAge("open", time.Time{}); got != "open" {
		t.Errorf("stateAge(zero) = %q", got)
	}
}

func TestNameKey(t *testing.T) {
	known := "ed25519:XuQyk9ovuNCWeA1r-rd7njyEs4jyikp1bIBNcQ73Pow"
	other := "ed25519:Q6R7jmCGCTRsogxDN0Q5yfFZodsFXsBuTSnSF_plovw"
	names := map[string]string{known: "b@fix"}
	if got := nameKey(names, known); got != "b@fix (XuQyk9ovuN)" {
		t.Errorf("nameKey(known) = %q", got)
	}
	if got := nameKey(names, other); got != wire.ShortKey(other) {
		t.Errorf("nameKey(other) = %q", got)
	}
}
