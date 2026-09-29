package web

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/agentwireprotocol/awp/internal/api"
	"github.com/agentwireprotocol/awp/internal/harness"
	"github.com/agentwireprotocol/awp/store"
	"github.com/agentwireprotocol/awp/wire"
)

// The JSON shapes here are the web API; web/src/lib/types.ts is their
// TypeScript twin and the two change together.

// Agent statuses.
const (
	statusSelf      = "self"
	statusConnected = "connected"
	statusOnline    = "online"
	statusStale     = "stale"
	statusOffline   = "offline"
)

var statusRank = map[string]int{statusSelf: 0, statusConnected: 1, statusOnline: 2, statusStale: 3, statusOffline: 4}

// Agent is one agent on the network.
type Agent struct {
	Key     string `json:"key"`
	Short   string `json:"short"`
	Name    string `json:"name"`
	About   string `json:"about,omitempty"`
	Version string `json:"version,omitempty"`
	Harness string `json:"harness,omitempty"` // claude, codex, ...: declared, else guessed from the name
	Model   string `json:"model,omitempty"`   // the model it runs on, as it reports it
	Host    string `json:"host,omitempty"`    // the hostname of the machine it runs on
	// LastActive is when it last did something through awp (to 30
	// seconds for other hosts); Listening says it is blocked in awp wait.
	LastActive *time.Time `json:"last_active,omitempty"`
	// How this host reaches it, for its direct peers: the transport of the
	// live connection (tailcat, tcp, unix) and its round trip; or, when
	// this host is trying and failing to reconnect, why.
	Transport   string     `json:"transport,omitempty"`
	RTTms       int64      `json:"rtt_ms,omitempty"`
	Unreachable string     `json:"unreachable,omitempty"`
	Listening   bool       `json:"listening,omitempty"`
	Status      string     `json:"status"`
	Sharing     bool       `json:"sharing"`
	Direct      bool       `json:"direct"`
	Via         string     `json:"via,omitempty"`
	Hops        int        `json:"hops"`
	Seen        *time.Time `json:"seen,omitempty"`
	Threads     int        `json:"threads"`
	Active      int        `json:"active"`
	Working     bool       `json:"working"`
	Waiting     bool       `json:"waiting"`
}

// Link is a connection between two agents, as either of them reports it.
type Link struct {
	A  string `json:"a"`
	B  string `json:"b"`
	Up bool   `json:"up"`
	// RTTms is the connection's round trip time as either end last
	// measured it (milliseconds; 0 unknown).
	RTTms int64 `json:"rtt_ms,omitempty"`
}

// Thread is a conversation between two agents, merged from both sides'
// reports: each side is the authority on its own state.
type Thread struct {
	ID      string     `json:"id"`
	Th      string     `json:"th"`
	A       string     `json:"a"`
	B       string     `json:"b"`
	AState  string     `json:"a_state"`
	BState  string     `json:"b_state"`
	Subject string     `json:"subject"`
	Updated *time.Time `json:"updated,omitempty"`
	Local   bool       `json:"local"`
	Peer    string     `json:"peer,omitempty"`
	// SharedBy is the agent that shares this thread with this host
	// (wire/mirror.go), when this host is not part of it: its
	// conversation can be read too.
	SharedBy string `json:"shared_by,omitempty"`
	Unread   int    `json:"unread,omitempty"`
}

// Stats are the headline numbers.
type Stats struct {
	Agents  int `json:"agents"`
	Up      int `json:"up"`
	Threads int `json:"threads"`
	Active  int `json:"active"`
	Working int `json:"working"`
	Waiting int `json:"waiting"`
}

// State is the whole network as this host sees it.
type State struct {
	At       time.Time `json:"at"`
	Version  string    `json:"version"`
	Self     string    `json:"self"`
	HostName string    `json:"host_name"`
	Presence bool      `json:"presence"`
	Address  string    `json:"address,omitempty"`
	// Listeners are the addresses this host accepts connections on, and
	// TailcatError why its tailcat listener is not up, if it is not.
	Listeners    []string `json:"listeners"`
	TailcatError string   `json:"tailcat_error,omitempty"`
	Agents       []Agent  `json:"agents"`
	Links        []Link   `json:"links"`
	Threads      []Thread `json:"threads"`
	Stats        Stats    `json:"stats"`
}

// same reports whether two states differ only in their time.
func (s *State) same(o *State) bool {
	if s == nil || o == nil {
		return s == o
	}
	a, b := *s, *o
	a.At, b.At = time.Time{}, time.Time{}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func (s *State) agent(key string) *Agent {
	for i := range s.Agents {
		if s.Agents[i].Key == key {
			return &s.Agents[i]
		}
	}
	return nil
}

func threadID(th, x, y string) (id, a, b string) {
	a, b = x, y
	if b < a {
		a, b = b, a
	}
	return th + ":" + a + ":" + b, a, b
}

// realAbout drops the about text awp puts in hello when the agent set
// none ("awp-go 0.2.0, key fingerprint SHA256:..."): it says nothing
// about what the agent is doing.
func realAbout(s string) string {
	if strings.HasPrefix(s, "awp-") && strings.Contains(s, "key fingerprint") {
		return ""
	}
	return s
}

// buildState merges the host's own view (status and threads) with every
// agent's presence.
func buildState(st *api.Status, local []*store.Thread, net *api.Network, mirrored []store.MirroredThread, now time.Time) *State {
	s := &State{At: now, Version: st.Version, Self: st.Key, HostName: st.Name, Presence: st.Presence, Address: st.ShareAddress(),
		Listeners: append([]string{}, st.Addresses...), TailcatError: st.TailcatErr,
		Agents: []Agent{}, Links: []Link{}, Threads: []Thread{}}
	if net == nil {
		net = &api.Network{}
	}

	names := map[string]string{}
	for _, a := range net.Agents {
		for _, p := range a.Peers {
			if p.Name != "" {
				names[p.Key] = p.Name
			}
		}
	}
	for _, p := range st.Peers {
		if l := p.Label(); l != "" && l != p.Short {
			names[p.Key] = l
		}
	}
	for _, a := range net.Agents {
		if a.Name != "" {
			names[a.Origin] = a.Name
		}
	}
	names[st.Key] = st.Name

	agents := map[string]*Agent{}
	add := func(key string) *Agent {
		if a, ok := agents[key]; ok {
			return a
		}
		a := &Agent{Key: key, Short: wire.ShortKey(key), Name: names[key], Status: statusOffline, Hops: -1}
		agents[key] = a
		return a
	}
	seen := func(t time.Time) *time.Time {
		if t.IsZero() {
			return nil
		}
		return &t
	}

	me := add(st.Key)
	me.Status, me.Sharing, me.Version, me.Hops, me.Seen = statusSelf, st.Presence, st.Version, 0, seen(now)
	me.About = cmp.Or(realAbout(net.Self.About), realAbout(st.About))
	me.Harness, me.Model = st.Harness, st.Model
	me.LastActive, me.Listening = st.Active, st.Waiting
	// A daemon older than the host field: the page is served from the same
	// machine, so its hostname is this one.
	me.Host = st.Host
	if me.Host == "" {
		me.Host, _ = os.Hostname()
	}

	links := map[[2]string]*Link{}
	link := func(x, y string, up bool, rtt int64) {
		if x == y {
			return
		}
		if y < x {
			x, y = y, x
		}
		k := [2]string{x, y}
		if l, ok := links[k]; ok {
			l.Up = l.Up || up
			if rtt > 0 && (l.RTTms == 0 || rtt < l.RTTms) {
				l.RTTms = rtt
			}
			return
		}
		links[k] = &Link{A: x, B: y, Up: up, RTTms: rtt}
	}

	for _, p := range st.Peers {
		a := add(p.Key)
		a.Direct, a.Hops = true, 1
		a.About = cmp.Or(a.About, realAbout(p.About))
		if p.Connected {
			a.Status = statusConnected
			a.Transport, _, _ = strings.Cut(p.Via, ":")
			a.RTTms = p.RTTms
		} else if p.Dialing && p.DialErr != "" {
			a.Unreachable = p.DialErr
		}
		a.Seen = seen(p.LastSeen)
		link(st.Key, p.Key, p.Connected, p.RTTms)
	}
	for _, v := range net.Agents {
		a := add(v.Origin)
		a.Sharing, a.Version = true, v.Version
		a.Harness = cmp.Or(harness.Normalize(v.Harness), a.Harness)
		a.Model = cmp.Or(v.Model, a.Model)
		a.Host = cmp.Or(v.Host, a.Host)
		if at, err := wire.ParseTime(v.Active); err == nil {
			a.LastActive, a.Listening = &at, v.Waiting
		}
		a.About = cmp.Or(realAbout(v.About), a.About)
		if a.Status != statusConnected {
			if v.Status == api.AgentStale {
				a.Status = statusStale
			} else {
				a.Status = statusOnline
			}
		}
		if !a.Direct {
			a.Via, a.Hops = v.Via, v.Hops+1
		}
		if t := v.Time(); a.Seen == nil || t.After(*a.Seen) {
			a.Seen = seen(t)
		}
		for _, p := range v.Peers {
			pa := add(p.Key)
			if p.Up && pa.Status == statusOffline {
				pa.Status = statusOnline
			}
			if pa.Hops < 0 {
				pa.Hops = a.Hops + 1
				pa.Via = v.Origin
			}
			link(v.Origin, p.Key, p.Up && v.Status != api.AgentStale, p.RTT)
		}
	}

	threads := map[string]*Thread{}
	// own records whose states came from the party itself: its own report
	// is definitive, the other side's report of it only a hint.
	own := map[string]bool{}
	side := func(th, subject, who, other, mine, theirs string, updated time.Time) *Thread {
		id, a, b := threadID(th, who, other)
		t, ok := threads[id]
		if !ok {
			t = &Thread{ID: id, Th: th, A: a, B: b, Subject: subject}
			threads[id] = t
		}
		if t.Subject == "" {
			t.Subject = subject
		}
		if !updated.IsZero() && (t.Updated == nil || updated.After(*t.Updated)) {
			u := updated
			t.Updated = &u
		}
		set := func(party, state string, definitive bool) {
			k := id + "\x00" + party
			if state == "" || (own[k] && !definitive) {
				return
			}
			if party == t.A {
				t.AState = state
			} else {
				t.BState = state
			}
			own[k] = own[k] || definitive
		}
		set(who, mine, true)
		set(other, theirs, false)
		return t
	}
	for _, lt := range local {
		t := side(lt.Th, lt.Subject, st.Key, lt.Peer, lt.MyState, lt.TheirState, lt.Updated)
		t.Local, t.Peer, t.Unread = true, lt.Peer, lt.Unread
		add(lt.Peer)
	}
	for _, v := range net.Agents {
		for _, pt := range v.Threads {
			u, _ := wire.ParseTime(pt.Updated)
			side(pt.Th, pt.Subject, v.Origin, pt.Peer, pt.Mine, pt.Theirs, u)
			add(pt.Peer)
		}
	}

	for _, m := range mirrored {
		t := side(m.Th, m.Subject, m.Sharer, m.Of, "", "", m.Updated)
		if !t.Local {
			t.SharedBy = m.Sharer
		}
		add(m.Sharer)
		add(m.Of)
	}

	for _, t := range threads {
		if t.AState == "" {
			t.AState = wire.StateOpen
		}
		if t.BState == "" {
			t.BState = wire.StateOpen
		}
		s.Threads = append(s.Threads, *t)
		open := !store.Terminal(t.AState) && !store.Terminal(t.BState)
		for _, k := range []struct{ key, state string }{{t.A, t.AState}, {t.B, t.BState}} {
			a := add(k.key)
			a.Threads++
			if open {
				a.Active++
				a.Working = a.Working || k.state == wire.StateWorking
				a.Waiting = a.Waiting || k.state == wire.StateWaiting
			}
		}
	}
	slices.SortFunc(s.Threads, func(x, y Thread) int {
		var tx, ty time.Time
		if x.Updated != nil {
			tx = *x.Updated
		}
		if y.Updated != nil {
			ty = *y.Updated
		}
		if c := ty.Compare(tx); c != 0 {
			return c
		}
		return strings.Compare(x.ID, y.ID)
	})

	for _, a := range agents {
		if a.Hops < 0 {
			a.Hops = 0
		}
		if a.Name == "" {
			a.Name = names[a.Key]
		}
		if a.Harness == "" {
			a.Harness = harness.FromName(a.Name)
		}
		s.Agents = append(s.Agents, *a)
	}
	slices.SortFunc(s.Agents, func(x, y Agent) int {
		if c := cmp.Compare(statusRank[x.Status], statusRank[y.Status]); c != 0 {
			return c
		}
		if c := strings.Compare(strings.ToLower(x.Name), strings.ToLower(y.Name)); c != 0 {
			return c
		}
		return strings.Compare(x.Key, y.Key)
	})
	for _, l := range links {
		s.Links = append(s.Links, *l)
	}
	slices.SortFunc(s.Links, func(x, y Link) int {
		return cmp.Or(strings.Compare(x.A, y.A), strings.Compare(x.B, y.B))
	})

	s.Stats.Agents = len(s.Agents)
	for _, a := range s.Agents {
		if a.Status == statusSelf || a.Status == statusConnected || a.Status == statusOnline {
			s.Stats.Up++
		}
		if a.Working {
			s.Stats.Working++
		}
		if a.Waiting {
			s.Stats.Waiting++
		}
	}
	s.Stats.Threads = len(s.Threads)
	for _, t := range s.Threads {
		if !store.Terminal(t.AState) && !store.Terminal(t.BState) {
			s.Stats.Active++
		}
	}
	return s
}

// Activity is one thing that happened on the network.
type Activity struct {
	Seq     int64     `json:"seq"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	From    string    `json:"from"`
	To      string    `json:"to,omitempty"`
	Th      string    `json:"th,omitempty"`
	Subject string    `json:"subject,omitempty"`
	State   string    `json:"state,omitempty"`
	Text    string    `json:"text,omitempty"`
	Local   bool      `json:"local"`
}

// diffStates finds what agents did between two states on threads this
// host is not part of: its own threads come first-hand from the event
// log. The result has no sequence numbers yet.
func diffStates(prev, cur *State) []Activity {
	if prev == nil || cur == nil {
		return nil
	}
	before := map[string]Thread{}
	for _, t := range prev.Threads {
		before[t.ID] = t
	}
	var out []Activity
	for _, t := range slices.Backward(cur.Threads) { // oldest first
		if t.Local || t.A == cur.Self || t.B == cur.Self || t.SharedBy != "" {
			continue // first-hand, or mirrored: the lines themselves are activity
		}
		at := cur.At
		if t.Updated != nil {
			at = *t.Updated
		}
		o, ok := before[t.ID]
		if !ok {
			out = append(out, Activity{At: at, Kind: "thread", From: t.A, To: t.B, Th: t.Th, Subject: t.Subject})
			continue
		}
		changed := false
		for _, side := range []struct{ who, other, was, now string }{{t.A, t.B, o.AState, t.AState}, {t.B, t.A, o.BState, t.BState}} {
			if side.was != side.now {
				changed = true
				out = append(out, Activity{At: at, Kind: "state", From: side.who, To: side.other, Th: t.Th, Subject: t.Subject, State: side.now})
			}
		}
		if !changed && t.Updated != nil && (o.Updated == nil || t.Updated.After(*o.Updated)) {
			out = append(out, Activity{At: at, Kind: "msg", From: t.A, To: t.B, Th: t.Th, Subject: t.Subject, Text: "exchanged a message"})
		}
	}
	return out
}

// activityFromEvent turns a local log event into activity.
func activityFromEvent(ev api.Event, self string) (Activity, bool) {
	a := Activity{At: ev.At, Kind: ev.Type, From: ev.Peer, To: self, Th: ev.Th, Subject: ev.Subject, Local: true}
	if ev.Dir == "out" {
		a.From, a.To = self, ev.Peer
	}
	if ev.Dir == "mirror" {
		// A line another agent (ev.Peer) shares from its thread with "of".
		a.Local = false
		a.From, a.To = mirrorSides(ev)
		if s, _ := ev.Meta["subject"].(string); s != "" {
			a.Subject = s
		}
	}
	meta := func(k string) string {
		if v, ok := ev.Meta[k]; ok && v != nil {
			return fmt.Sprint(v)
		}
		return ""
	}
	switch ev.Type {
	case wire.TMsg:
		a.Text = snippet(ev.Parts(), 200)
	case wire.TState:
		var s wire.State
		json.Unmarshal(ev.Msg, &s)
		a.State, a.Text = s.State, s.Note
	case "connected":
		a.Text = meta("via")
	case "disconnected":
		a.Text = meta("reason")
	case "blob":
		a.Text = meta("name")
	case "presence":
		a.Kind, a.To = meta("event"), ""
		if a.Kind != "joined" && a.Kind != "left" {
			return a, false
		}
		if via := meta("via"); a.Kind == "joined" && via != "" && via != ev.Peer {
			a.To = via
		}
	case wire.TGrant:
		caps, _ := ev.Meta["caps"].([]any)
		var cs []string
		for _, c := range caps {
			cs = append(cs, fmt.Sprint(c))
		}
		a.Text = strings.Join(cs, ", ")
	case wire.TIntroduce:
		a.Text = meta("name")
	case wire.TErr, "refused":
		a.Kind = "error"
		a.Text = strings.TrimPrefix(meta("code")+": "+meta("detail"), ": ")
		if ev.Type == "refused" {
			a.Text = meta("reason")
		}
	case wire.TBye:
		a.Text = meta("reason")
	default:
		return a, false
	}
	return a, true
}

// snippet is a one-line preview of message parts.
func snippet(parts []wire.Part, max int) string {
	var out []string
	for _, p := range parts {
		switch p.K {
		case wire.PartText:
			out = append(out, strings.Join(strings.Fields(p.Text), " "))
		case wire.PartCode:
			out = append(out, "[code "+p.Lang+"]")
		case wire.PartData:
			out = append(out, "[data]")
		case wire.PartBlob:
			out = append(out, "[file "+p.Name+"]")
		}
	}
	s := strings.Join(out, " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

// Message is one line of a local conversation.
type Message struct {
	Seq   int64     `json:"seq"`
	At    time.Time `json:"at"`
	From  string    `json:"from"`
	Kind  string    `json:"kind"`
	Parts []WebPart `json:"parts,omitempty"`
	State string    `json:"state,omitempty"`
	Note  string    `json:"note,omitempty"`
	Acked *bool     `json:"acked,omitempty"`
}

// WebPart is a message part as the page shows it.
type WebPart struct {
	Type   string          `json:"type"`
	Text   string          `json:"text,omitempty"`
	Lang   string          `json:"lang,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	Name   string          `json:"name,omitempty"`
	Mime   string          `json:"mime,omitempty"`
	Size   *int64          `json:"size,omitempty"`
	Status string          `json:"status,omitempty"`
	// URL fetches a file this host has (awp web's /api/blob): received
	// and complete, or sent by this host.
	URL string `json:"url,omitempty"`
}

// messageFromEvent turns a local thread's event into a conversation line.
func messageFromEvent(ev api.Event, self string) (Message, bool) {
	m := Message{Seq: ev.Seq, At: ev.At, From: ev.Peer, Kind: ev.Type}
	if ev.Dir == "mirror" {
		m.From, _ = mirrorSides(ev)
	} else if ev.Dir == "out" {
		m.From = self
		acked := ev.Acked
		m.Acked = &acked
	}
	switch ev.Type {
	case wire.TMsg:
		for _, p := range ev.Parts() {
			wp := WebPart{Type: p.K, Text: p.Text, Lang: p.Lang, Data: p.Data, Name: p.Name, Mime: p.Mime}
			if p.K == wire.PartBlob {
				size := p.Size
				wp.Size = &size
				if b, ok := ev.Blobs[p.Ref]; ok {
					wp.Status = b.Status
					if ev.Dir == "out" || b.Status == "complete" {
						wp.URL = blobURL(ev.Peer, ev.Dir, p.Ref)
					}
				}
			}
			m.Parts = append(m.Parts, wp)
		}
	case wire.TState:
		var s wire.State
		json.Unmarshal(ev.Msg, &s)
		m.State, m.Note = s.State, s.Note
	case wire.TErr:
		m.Kind = "error"
		m.Note = fmt.Sprint(ev.Meta["code"], ": ", ev.Meta["detail"])
	case wire.TGrant, wire.TIntroduce, wire.TBye, "blob":
	default:
		return m, false
	}
	return m, true
}

// mirrorSides says who wrote a mirrored line, and to whom.
func mirrorSides(ev api.Event) (from, to string) {
	of, _ := ev.Meta["of"].(string)
	from, _ = ev.Meta["from"].(string)
	if from == "" {
		from = ev.Peer
	}
	if from == ev.Peer {
		return from, of
	}
	return from, ev.Peer
}

func blobURL(peer, dir, ref string) string {
	return "/api/blob?" + url.Values{"peer": {peer}, "dir": {dir}, "ref": {ref}}.Encode()
}
