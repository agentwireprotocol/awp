package web

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/agentwireprotocol/awp/internal/api"
	"github.com/agentwireprotocol/awp/internal/control"
	"github.com/agentwireprotocol/awp/store"
	"github.com/agentwireprotocol/awp/wire"
)

func testKey(b byte) string {
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = b
	return wire.FormatKey(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
}

var (
	hostKey    = testKey(1) // ops: the host serving the page
	workerKey  = testKey(2) // connected to the host
	bossKey    = testKey(3) // heard of through worker's gossip
	builderKey = testKey(4) // shares no presence; only worker mentions it
)

// network is the host's picture: connected to worker; worker talks to
// boss (both share presence) and to builder (which does not); the host has
// a thread of its own with worker.
func network(now time.Time, bossState string) (*api.Status, []*store.Thread, *api.Network) {
	ts := wire.FormatTime(now.Add(-time.Minute))
	later := wire.FormatTime(now.Add(-30 * time.Second))
	st := &api.Status{Key: hostKey, Short: wire.ShortKey(hostKey), Name: "ops@laptop", Version: "0.2.0", Presence: true,
		About: "awp-go 0.2.0, key fingerprint SHA256:x",
		Peers: []api.PeerView{
			{Key: workerKey, Short: wire.ShortKey(workerKey), Name: "claude-code@worker", Connected: true, LastSeen: now, Via: "tailcat:tcAAA", RTTms: 42},
		}}
	local := []*store.Thread{{Peer: workerKey, Th: "thr_ops", Subject: "Status report", MyState: wire.StateOpen, TheirState: wire.StateWorking, Updated: now.Add(-2 * time.Minute)}}
	net := &api.Network{
		Self: api.AgentView{Presence: wire.Presence{Origin: hostKey, Name: "ops@laptop"}, Status: api.AgentSelf, Sharing: true},
		Agents: []api.AgentView{
			{Presence: wire.Presence{Origin: workerKey, Name: "claude-code@worker", About: "fixing calc", TS: ts,
				Peers: []wire.PresencePeer{{Key: hostKey, Up: true}, {Key: bossKey, Up: true}, {Key: builderKey, Name: "codex@builder", Up: true}},
				Threads: []wire.PresenceThread{
					{Th: "thr_ops", Peer: hostKey, Subject: "Status report", Mine: wire.StateWorking, Theirs: wire.StateOpen, Updated: ts},
					{Th: "thr_fix", Peer: bossKey, Subject: "Fix calc", Mine: wire.StateWorking, Theirs: wire.StateOpen, Updated: ts},
					{Th: "thr_build", Peer: builderKey, Subject: "Build it", Mine: wire.StateWaiting, Theirs: wire.StateWorking, Updated: ts},
				}},
				Status: api.AgentFresh, Direct: true, Via: workerKey},
			{Presence: wire.Presence{Origin: bossKey, Name: "claude-code@boss", Harness: "codex", Model: "gpt-5.5", Host: "build-box", TS: ts,
				Peers:   []wire.PresencePeer{{Key: workerKey, Up: true}},
				Threads: []wire.PresenceThread{{Th: "thr_fix", Peer: workerKey, Subject: "Fix calc", Mine: bossState, Theirs: wire.StateOpen, Updated: later}}},
				Status: api.AgentFresh, Via: workerKey, Hops: 1},
		},
	}
	return st, local, net
}

func TestBuildStateIsNetworkWide(t *testing.T) {
	now := time.Now()
	st, local, net := network(now, wire.StateOpen)
	s := buildState(st, local, net, nil, now)
	if s.Self != hostKey || s.HostName != "ops@laptop" {
		t.Errorf("self %q %q", s.Self, s.HostName)
	}
	want := []struct{ key, name, status string }{
		{hostKey, "ops@laptop", statusSelf},
		{workerKey, "claude-code@worker", statusConnected},
		{bossKey, "claude-code@boss", statusOnline},
		{builderKey, "codex@builder", statusOnline},
	}
	if len(s.Agents) != len(want) {
		t.Fatalf("%d agents: %+v", len(s.Agents), s.Agents)
	}
	for i, w := range want {
		a := s.Agents[i]
		if a.Key != w.key || a.Name != w.name || a.Status != w.status {
			t.Errorf("agent %d = %s %q %s, want %q %s", i, a.Short, a.Name, a.Status, w.name, w.status)
		}
	}
	if a := s.agent(hostKey); a.About != "" {
		t.Errorf("default hello about shown: %q", a.About)
	}
	// Declared in presence, or read from the name.
	for key, want := range map[string]string{hostKey: "", workerKey: "claude", bossKey: "codex", builderKey: "codex"} {
		if a := s.agent(key); a.Harness != want {
			t.Errorf("%s harness %q, want %q", a.Name, a.Harness, want)
		}
	}
	if a := s.agent(bossKey); a.Model != "gpt-5.5" || a.Host != "build-box" {
		t.Errorf("boss model %q host %q", a.Model, a.Host)
	}
	if a := s.agent(builderKey); a.Sharing || a.Via != workerKey || a.Hops != 2 {
		t.Errorf("builder = %+v", a)
	}
	if a := s.agent(bossKey); !a.Sharing || a.Via != workerKey || a.Hops != 2 {
		t.Errorf("boss = %+v", a)
	}
	if a := s.agent(workerKey); a.About != "fixing calc" || !a.Working || !a.Waiting || a.Threads != 3 {
		t.Errorf("worker = %+v", a)
	}
	if len(s.Links) != 3 {
		t.Errorf("links: %+v", s.Links)
	}
	if len(s.Threads) != 3 {
		t.Fatalf("threads: %+v", s.Threads)
	}
	byTh := map[string]Thread{}
	for _, th := range s.Threads {
		byTh[th.Th] = th
	}
	// Each side's own report wins: boss says open, and so does worker's
	// view of it here; worker's own state comes from worker.
	fix := byTh["thr_fix"]
	if st := stateOf(fix, workerKey); st != wire.StateWorking {
		t.Errorf("worker on thr_fix: %s", st)
	}
	ops := byTh["thr_ops"]
	if !ops.Local || ops.Peer != workerKey || stateOf(ops, hostKey) != wire.StateOpen || stateOf(ops, workerKey) != wire.StateWorking {
		t.Errorf("thr_ops = %+v", ops)
	}
	if byTh["thr_build"].Local {
		t.Error("remote thread marked local")
	}
	if s.Stats != (Stats{Agents: 4, Up: 4, Threads: 3, Active: 3, Working: 2, Waiting: 1}) {
		t.Errorf("stats %+v", s.Stats)
	}
}

func stateOf(t Thread, key string) string {
	if t.A == key {
		return t.AState
	}
	return t.BState
}

// A party's own state beats what the other side last heard of it.
func TestOwnStateWins(t *testing.T) {
	now := time.Now()
	st, local, net := network(now, wire.StateDone)
	s := buildState(st, local, net, nil, now)
	for _, th := range s.Threads {
		if th.Th == "thr_fix" && stateOf(th, bossKey) != wire.StateDone {
			t.Errorf("boss on thr_fix = %s; worker's stale view won", stateOf(th, bossKey))
		}
	}
}

func TestDiffStatesReportsOtherAgents(t *testing.T) {
	now := time.Now()
	st, local, net := network(now, wire.StateOpen)
	prev := buildState(st, local, net, nil, now)

	st, local, net = network(now, wire.StateDone)
	net.Agents[0].Threads = append(net.Agents[0].Threads, wire.PresenceThread{Th: "thr_new", Peer: bossKey, Subject: "Another", Mine: wire.StateOpen})
	// A message on the build thread: newer, same states.
	net.Agents[0].Threads[2].Updated = wire.FormatTime(now)
	// The host's own thread changes too, but that comes from the event log.
	local[0].TheirState = wire.StateDone
	net.Agents[0].Threads[0].Mine = wire.StateDone
	cur := buildState(st, local, net, nil, now.Add(time.Second))

	got := map[string]Activity{}
	for _, a := range diffStates(prev, cur) {
		got[a.Kind+" "+a.Th] = a
	}
	if len(got) != 3 {
		t.Fatalf("activity: %+v", got)
	}
	if a := got["thread thr_new"]; a.Subject != "Another" {
		t.Errorf("new thread: %+v", a)
	}
	if a := got["state thr_fix"]; a.From != bossKey || a.To != workerKey || a.State != wire.StateDone {
		t.Errorf("state change: %+v", a)
	}
	if _, ok := got["msg thr_build"]; !ok {
		t.Errorf("no message activity for the build thread: %+v", got)
	}
	if diffStates(cur, cur) != nil {
		t.Error("activity from an unchanged state")
	}
}

// fakeDaemon answers the control calls the server makes.
type fakeDaemon struct {
	mu     sync.Mutex
	now    time.Time
	boss   string
	events []api.Event
	subs   []chan api.Event
	blobs  []*store.Blob
}

func (f *fakeDaemon) Call(ctx context.Context, method string, params json.RawMessage) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, local, net := network(f.now, f.boss)
	switch method {
	case "status":
		return st, nil
	case "threads":
		return local, nil
	case "presence":
		return net, nil
	case "blobs":
		return f.blobs, nil
	case "mirrored":
		return []store.MirroredThread{{Sharer: workerKey, Of: builderKey, Th: "thr_build", Subject: "Build it", Lines: 1, Updated: f.now}}, nil
	case "read":
		var p api.ReadParams
		json.Unmarshal(params, &p)
		res := api.ReadResult{Events: []api.Event{}}
		for _, ev := range f.events {
			if ev.Dir == "mirror" && !p.Mirrors {
				continue
			}
			if (p.Th == "" || ev.Th == p.Th) && (p.Peer == "" || ev.Peer == p.Peer) {
				res.Events = append(res.Events, ev)
			}
		}
		if p.Th == "thr_ops" {
			res.Thread = local[0]
		}
		return res, nil
	}
	return nil, fmt.Errorf("unknown method %s", method)
}

func (f *fakeDaemon) Stream(ctx context.Context, method string, params json.RawMessage, emit func(any) error) (bool, error) {
	if method != "subscribe" {
		return false, nil
	}
	ch := make(chan api.Event, 16)
	f.mu.Lock()
	f.subs = append(f.subs, ch)
	f.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return true, nil
		case ev := <-ch:
			if err := emit(ev); err != nil {
				return true, err
			}
		}
	}
}

func (f *fakeDaemon) publish(ev api.Event) {
	f.mu.Lock()
	ev.Seq = int64(len(f.events) + 1)
	f.events = append(f.events, ev)
	subs := f.subs
	f.mu.Unlock()
	for _, ch := range subs {
		ch <- ev
	}
}

func msgEvent(dir, text string) api.Event {
	raw, _ := json.Marshal(wire.Msg{Envelope: wire.Envelope{T: wire.TMsg, Th: "thr_ops"}, Parts: []wire.Part{{K: wire.PartText, Text: text}}})
	return api.Event{At: time.Now(), Dir: dir, Type: wire.TMsg, Peer: workerKey, Th: "thr_ops", Subject: "Status report", Msg: raw, Acked: true}
}

func startServer(t *testing.T) (*fakeDaemon, *httptest.Server) {
	t.Helper()
	home := t.TempDir()
	fd := &fakeDaemon{now: time.Now(), boss: wire.StateOpen}
	fd.events = []api.Event{func() api.Event { e := msgEvent("in", "earlier"); e.Seq = 1; return e }()}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go control.Serve(ctx, home, fd)
	srv := &Server{C: &control.Client{Home: home}, Assets: fstest.MapFS{"index.html": {Data: []byte("<h1>awp</h1>")}, "assets/app.js": {Data: []byte("x")}}}
	for !srv.C.Running() {
		time.Sleep(10 * time.Millisecond)
	}
	go srv.Run(ctx)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if res, err := http.Get(hs.URL + "/api/state"); err == nil && res.StatusCode == 200 {
			res.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("state never ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fd, hs
}

func getJSON(t *testing.T, url string, v any) int {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	json.NewDecoder(res.Body).Decode(v)
	return res.StatusCode
}

func TestAPI(t *testing.T) {
	_, hs := startServer(t)
	var st State
	getJSON(t, hs.URL+"/api/state", &st)
	if len(st.Agents) != 4 || st.Self != hostKey {
		t.Fatalf("state: %+v", st)
	}
	var c Conversation
	if code := getJSON(t, hs.URL+"/api/thread?peer="+workerKey+"&th=thr_ops", &c); code != 200 {
		t.Fatalf("thread: %d", code)
	}
	if c.Subject != "Status report" || len(c.Messages) != 1 || c.Messages[0].Parts[0].Text != "earlier" || c.Messages[0].From != workerKey {
		t.Errorf("conversation: %+v", c)
	}
	var e map[string]string
	if code := getJSON(t, hs.URL+"/api/thread?peer="+bossKey+"&th=thr_fix", &e); code != 404 || !strings.Contains(e["error"], "private") {
		t.Errorf("remote thread: %d %v", code, e)
	}
	var act struct{ Items []Activity }
	getJSON(t, hs.URL+"/api/activity", &act)
	if len(act.Items) != 1 || act.Items[0].Text != "earlier" || !act.Items[0].Local {
		t.Errorf("activity: %+v", act.Items)
	}
	// The page, and its routes, are served from the assets.
	for _, p := range []string{"/", "/agents/x", "/assets/app.js"} {
		res, err := http.Get(hs.URL + p)
		if err != nil || res.StatusCode != 200 {
			t.Errorf("GET %s: %v %v", p, res.StatusCode, err)
		}
		res.Body.Close()
	}
	res, _ := http.Get(hs.URL + "/api/nope")
	if res.StatusCode != 404 {
		t.Errorf("unknown API call: %d", res.StatusCode)
	}
	res.Body.Close()
}

func TestEventStream(t *testing.T) {
	fd, hs := startServer(t)
	req, _ := http.NewRequest("GET", hs.URL+"/api/events", nil)
	req.Header.Set("Last-Event-ID", "0")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	events := make(chan [2]string, 64)
	go func() {
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(nil, 1<<20)
		var event string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				events <- [2]string{event, strings.TrimPrefix(line, "data: ")}
			}
		}
		close(events)
	}()
	next := func(kind string) string {
		t.Helper()
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					t.Fatalf("stream ended waiting for %s", kind)
				}
				if ev[0] == kind {
					return ev[1]
				}
			case <-ctx.Done():
				t.Fatalf("no %s event", kind)
			}
		}
	}
	next("state")
	if a := next("activity"); !strings.Contains(a, "earlier") {
		t.Errorf("missed activity not replayed: %s", a)
	}
	// Wait until the server follows the daemon's log.
	for {
		fd.mu.Lock()
		n := len(fd.subs)
		fd.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	fd.publish(msgEvent("out", "hello from ops"))
	if a := next("activity"); !strings.Contains(a, "hello from ops") || !strings.Contains(a, `"from":"`+hostKey) {
		t.Errorf("live activity: %s", a)
	}
	if m := next("message"); !strings.Contains(m, "hello from ops") || !strings.Contains(m, `"th":"thr_ops"`) {
		t.Errorf("message: %s", m)
	}
	// Another agent finishes its thread: presence changes, the state and
	// the activity follow.
	fd.mu.Lock()
	fd.boss = wire.StateDone
	fd.mu.Unlock()
	if a := next("activity"); !strings.Contains(a, `"state":"done"`) || !strings.Contains(a, `"from":"`+bossKey) {
		t.Errorf("remote state activity: %s", a)
	}
	if s := next("state"); !strings.Contains(s, `"done"`) {
		t.Errorf("state after change: %s", s)
	}
}

// A thread another agent shares with this host can be read like its own.
func TestMirroredThread(t *testing.T) {
	fd, hs := startServer(t)
	raw, _ := json.Marshal(wire.Msg{Envelope: wire.Envelope{T: wire.TMsg, ID: "01MIRROR", Th: "thr_build"}, Parts: []wire.Part{{K: wire.PartText, Text: "artifacts are up"}}})
	fd.mu.Lock()
	fd.events = append(fd.events, api.Event{Seq: 2, At: time.Now(), Dir: "mirror", Type: wire.TMsg, Peer: workerKey, Th: "thr_build", Msg: raw,
		Meta: map[string]any{"of": builderKey, "from": builderKey, "subject": "Build it"}})
	fd.mu.Unlock()

	var st State
	getJSON(t, hs.URL+"/api/state", &st)
	var build *Thread
	for i := range st.Threads {
		if st.Threads[i].Th == "thr_build" {
			build = &st.Threads[i]
		}
	}
	if build == nil || build.SharedBy != workerKey || build.Local {
		t.Fatalf("mirrored thread in state: %+v", build)
	}
	var c Conversation
	if code := getJSON(t, hs.URL+"/api/thread?peer="+workerKey+"&th=thr_build", &c); code != 200 {
		t.Fatalf("mirrored thread: %d", code)
	}
	if len(c.Messages) != 1 || c.Messages[0].From != builderKey || c.Messages[0].Parts[0].Text != "artifacts are up" {
		t.Errorf("mirrored conversation: %+v", c)
	}
	if c.AState != wire.StateWaiting || c.BState != wire.StateWorking {
		t.Errorf("states from the sharer's side: %q %q", c.AState, c.BState)
	}
}

// Both parties of a thread share with this host: every line arrives twice,
// once from each sharer. The feed shows it once, and the thread can be read
// through either sharer, with the subject and that sharer's state first.
func TestBothPartiesShare(t *testing.T) {
	fd, hs := startServer(t)
	raw, _ := json.Marshal(wire.Msg{Envelope: wire.Envelope{T: wire.TMsg, ID: "01TWICE", Th: "thr_build"}, Parts: []wire.Part{{K: wire.PartText, Text: "artifacts are up"}}})
	line := func(sharer, of string) api.Event {
		return api.Event{At: time.Now(), Dir: "mirror", Type: wire.TMsg, Peer: sharer, Th: "thr_build", ID: "01TWICE", Msg: raw,
			Meta: map[string]any{"of": of, "from": builderKey, "subject": "Build it"}}
	}
	fd.publish(line(workerKey, builderKey))
	count := func() int {
		var act struct{ Items []Activity }
		getJSON(t, hs.URL+"/api/activity", &act)
		n := 0
		for _, a := range act.Items {
			if a.Th == "thr_build" && a.Kind == wire.TMsg {
				n++
			}
		}
		return n
	}
	deadline := time.Now().Add(3 * time.Second)
	for count() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("mirrored line never reached the feed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	fd.publish(line(builderKey, workerKey))
	time.Sleep(200 * time.Millisecond)
	if n := count(); n != 1 {
		t.Errorf("the feed shows the line %d times, want once", n)
	}

	// The host recorded the worker as the sharer; asking through the
	// builder still finds the thread.
	var c Conversation
	if code := getJSON(t, hs.URL+"/api/thread?peer="+builderKey+"&th=thr_build", &c); code != 200 {
		t.Fatalf("thread through the other sharer: %d", code)
	}
	if c.Subject != "Build it" || len(c.Messages) != 1 {
		t.Errorf("conversation through the other sharer: %+v", c)
	}
	if c.AState != wire.StateWorking || c.BState != wire.StateWaiting {
		t.Errorf("states from the builder's side: %q %q", c.AState, c.BState)
	}
}

func TestActivityFromMirror(t *testing.T) {
	raw, _ := json.Marshal(wire.Msg{Envelope: wire.Envelope{T: wire.TMsg, ID: "01X", Th: "thr_build"}, Parts: []wire.Part{{K: wire.PartText, Text: "hi"}}})
	ev := api.Event{Dir: "mirror", Type: wire.TMsg, Peer: workerKey, Th: "thr_build", Msg: raw, Meta: map[string]any{"of": builderKey, "from": workerKey, "subject": "Build it"}}
	a, ok := activityFromEvent(ev, hostKey)
	if !ok || a.Local || a.From != workerKey || a.To != builderKey || a.Text != "hi" || a.Subject != "Build it" {
		t.Errorf("activity from a mirrored line: %+v", a)
	}
}

// Files this host has are served; images inline, everything else as a
// download, and nothing that is not complete or not recorded.
func TestBlobs(t *testing.T) {
	fd, hs := startServer(t)
	dir := t.TempDir()
	write := func(name, body string) string {
		p := dir + "/" + name
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	fd.mu.Lock()
	fd.blobs = []*store.Blob{
		{Peer: workerKey, Dir: "in", Ref: "png", Name: "chart.png", Mime: "image/png", Status: "complete", Path: write("chart.png", "\x89PNG fake")},
		{Peer: workerKey, Dir: "in", Ref: "svg", Name: "x.svg", Mime: "image/svg+xml", Status: "complete", Path: write("x.svg", "<svg onload=alert(1)>")},
		{Peer: workerKey, Dir: "in", Ref: "half", Name: "big.bin", Status: "pending", Path: write("half", "par")},
	}
	fd.mu.Unlock()
	get := func(ref, dir string) *http.Response {
		t.Helper()
		res, err := http.Get(hs.URL + "/api/blob?peer=" + url.QueryEscape(workerKey) + "&dir=" + dir + "&ref=" + ref)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	if res := get("png", "in"); res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/png" || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "inline") {
		t.Errorf("png: %d %q %q", res.StatusCode, res.Header.Get("Content-Type"), res.Header.Get("Content-Disposition"))
	}
	res := get("svg", "in")
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("svg must download, not render: %d %q %q", res.StatusCode, res.Header.Get("Content-Type"), res.Header.Get("Content-Disposition"))
	}
	if !strings.Contains(res.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("files are not sandboxed: %q", res.Header.Get("Content-Security-Policy"))
	}
	for _, c := range [][2]string{{"half", "in"}, {"nope", "in"}, {"png", "out"}, {"png", "sideways"}} {
		if res := get(c[0], c[1]); res.StatusCode == 200 {
			t.Errorf("%s/%s was served", c[0], c[1])
		}
	}
}

// How this host reaches its peers: transport and round trip, or why not.
func TestReachability(t *testing.T) {
	now := time.Now()
	st, local, net := network(now, wire.StateOpen)
	st.Peers = append(st.Peers, api.PeerView{Key: testKey(9), Short: wire.ShortKey(testKey(9)), Name: "gone@elsewhere", Dialing: true, DialErr: "tailcat dial: context deadline exceeded"})
	s := buildState(st, local, net, nil, now)
	if a := s.agent(workerKey); a.Transport != "tailcat" || a.RTTms != 42 || a.Unreachable != "" {
		t.Errorf("worker: transport %q rtt %d unreachable %q", a.Transport, a.RTTms, a.Unreachable)
	}
	if a := s.agent(testKey(9)); a == nil || a.Unreachable == "" || a.Status != statusOffline {
		t.Errorf("an unreachable peer: %+v", a)
	}
	for _, l := range s.Links {
		if (l.A == hostKey || l.B == hostKey) && (l.A == workerKey || l.B == workerKey) && l.RTTms != 42 {
			t.Errorf("host-worker link rtt %d", l.RTTms)
		}
	}
}
