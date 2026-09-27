package main

import (
	"fmt"
	"io"
	"time"

	"github.com/hollerprotocol/holler/internal/api"
	"github.com/hollerprotocol/holler/internal/render"
	"github.com/hollerprotocol/holler/wire"
)

// printer writes events for people, printing a thread banner whenever the
// thread changes.
type printer struct {
	w          io.Writer
	showThread bool
	lastTh     string
}

func (p *printer) event(ev api.Event) {
	if p.showThread && ev.Th != "" && ev.Th != p.lastTh {
		subject := ""
		if ev.Subject != "" {
			subject = fmt.Sprintf(" %q", ev.Subject)
		}
		fmt.Fprintf(p.w, "── %s%s with %s\n", ev.Th, subject, ev.PeerName)
		p.lastTh = ev.Th
	}
	fmt.Fprint(p.w, render.Event(ev, render.Options{Clock: true}))
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// age is how long ago t was, compactly: 5s, 2m, 3h, 4d.
func age(t time.Time) string {
	d := max(time.Since(t), 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	if time.Since(t) < 48*time.Hour {
		return age(t) + " ago"
	}
	return t.Local().Format("Jan 2")
}

// stateAge is a thread side's state and how long it has been in it,
// "working 2h", or just the state when that is not known.
func stateAge(state string, since time.Time) string {
	if since.IsZero() {
		return state
	}
	return state + " " + age(since)
}

// nameKey is a peer's label with its key prefix, "b@host (XuQyk9ovuN)",
// or the prefix alone for a key without a name.
func nameKey(names map[string]string, key string) string {
	short := wire.ShortKey(key)
	if name := names[key]; name != "" {
		return name + " (" + short + ")"
	}
	return short
}

func humanSize(n int64) string { return render.HumanSize(n) }
