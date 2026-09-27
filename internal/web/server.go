// Package web serves the holler web dashboard: the page (built from web/
// in the repository and embedded here) and a JSON and server-sent-events
// API over the local daemon's control socket.
//
// The dashboard is network-wide. Presence gossip brings every agent's
// peers, threads and states to this host, so the server merges them into
// one picture, and keeps one activity log: first-hand events from the
// daemon, plus what the other agents did as their presence changed.
// Conversations stay private: only threads this host is part of can be
// read. The server never marks anything read and never starts a daemon.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hollerprotocol/holler/internal/api"
	"github.com/hollerprotocol/holler/internal/control"
	"github.com/hollerprotocol/holler/internal/store"
)

//go:embed all:dist
var dist embed.FS

// Assets is the built page, or nil when this binary was built without it
// (make web builds it).
func Assets() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}

const (
	pollEvery = 1500 * time.Millisecond
	keepItems = 2000
	// keepMirrorIDs bounds the mirrored line ids remembered for
	// deduplication (seenMirrorLocked).
	keepMirrorIDs = 4096
	historySeed   = 300
)

// Server is the dashboard's backend for one daemon.
type Server struct {
	C      *control.Client
	Assets fs.FS // nil: API only
	Log    func(format string, args ...any)

	mu     sync.Mutex
	state  *State
	err    string // the daemon is unreachable
	items  []Activity
	seq    int64
	subs   map[chan sseMsg]struct{}
	nudge  chan struct{}
	seeded bool
	// Mirrored line ids already in the feed, in arrival order. When both
	// parties of a thread share with this host, every line arrives twice,
	// once from each sharer, and the feed shows it once.
	mirrored    map[string]struct{}
	mirrorOrder []string
}

type sseMsg struct {
	event string
	id    int64
	data  []byte
}

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

// Run keeps the picture current until ctx ends.
func (s *Server) Run(ctx context.Context) {
	s.mu.Lock()
	if s.subs == nil {
		s.subs = map[chan sseMsg]struct{}{}
	}
	s.nudge = make(chan struct{}, 1)
	s.mu.Unlock()
	go s.follow(ctx)
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		s.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.nudge:
			// Let a burst of events settle before looking.
			select {
			case <-ctx.Done():
				return
			case <-time.After(150 * time.Millisecond):
			}
		}
	}
}

func (s *Server) poll(ctx context.Context) {
	cur, err := s.snapshot(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if s.err == "" {
			s.logf("daemon unreachable: %v", err)
		}
		s.err = err.Error()
		return
	}
	s.err = ""
	prev := s.state
	if prev.same(cur) {
		s.state.At = cur.At
		return
	}
	for _, a := range diffStates(prev, cur) {
		s.addLocked(a)
	}
	s.state = cur
	s.broadcastLocked("state", 0, cur)
}

func (s *Server) snapshot(ctx context.Context) (*State, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var st api.Status
	if err := s.C.Call(ctx, "status", nil, &st); err != nil {
		return nil, err
	}
	var threads []*store.Thread
	if err := s.C.Call(ctx, "threads", api.PeerParams{}, &threads); err != nil {
		return nil, err
	}
	var net api.Network
	if err := s.C.Call(ctx, "presence", nil, &net); err != nil {
		net = api.Network{} // a daemon from before presence
	}
	var mirrored []store.MirroredThread
	if err := s.C.Call(ctx, "mirrored", nil, &mirrored); err != nil {
		mirrored = nil // a daemon from before conversation sharing
	}
	return buildState(&st, threads, &net, mirrored, time.Now()), nil
}

// follow streams the daemon's event log, across daemon restarts.
func (s *Server) follow(ctx context.Context) {
	var after int64
	for ctx.Err() == nil {
		self := s.selfKey(ctx)
		if self == "" {
			sleep(ctx, 2*time.Second)
			continue
		}
		if !s.seeded {
			var res api.ReadResult
			if err := s.C.Call(ctx, "read", api.ReadParams{Limit: historySeed, Mirrors: true}, &res); err == nil {
				s.mu.Lock()
				for _, ev := range res.Events {
					if s.seenMirrorLocked(ev) {
						continue
					}
					if a, ok := activityFromEvent(ev, self); ok {
						s.addLocked(a)
					}
					after = max(after, ev.Seq)
				}
				s.seeded = true
				s.mu.Unlock()
			}
		}
		err := s.C.Stream(ctx, "subscribe", api.SubscribeParams{Since: &after, Mirrors: true}, func(raw json.RawMessage) error {
			var ev api.Event
			if err := json.Unmarshal(raw, &ev); err != nil {
				return err
			}
			after = max(after, ev.Seq)
			s.onEvent(ev, self)
			return nil
		})
		if ctx.Err() == nil && err != nil {
			s.logf("event stream: %v", err)
		}
		sleep(ctx, 2*time.Second)
	}
}

func (s *Server) selfKey(ctx context.Context) string {
	s.mu.Lock()
	st := s.state
	s.mu.Unlock()
	if st != nil {
		return st.Self
	}
	var status api.Status
	if err := s.C.Call(ctx, "status", nil, &status); err != nil {
		return ""
	}
	return status.Key
}

func (s *Server) onEvent(ev api.Event, self string) {
	s.mu.Lock()
	if s.seenMirrorLocked(ev) {
		s.mu.Unlock()
		return
	}
	if a, ok := activityFromEvent(ev, self); ok {
		s.addLocked(a)
	}
	if ev.Th != "" {
		if m, ok := messageFromEvent(ev, self); ok {
			s.broadcastLocked("message", 0, map[string]any{"peer": ev.Peer, "th": ev.Th, "message": m})
		}
	}
	s.mu.Unlock()
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// seenMirrorLocked reports whether a mirrored line is already in the feed,
// and remembers it if not. Other events are never duplicates. The caller
// holds mu.
func (s *Server) seenMirrorLocked(ev api.Event) bool {
	if ev.Dir != "mirror" || ev.ID == "" {
		return false
	}
	if _, ok := s.mirrored[ev.ID]; ok {
		return true
	}
	if s.mirrored == nil {
		s.mirrored = map[string]struct{}{}
	}
	s.mirrored[ev.ID] = struct{}{}
	s.mirrorOrder = append(s.mirrorOrder, ev.ID)
	if len(s.mirrorOrder) > keepMirrorIDs {
		drop := len(s.mirrorOrder) - keepMirrorIDs
		for _, id := range s.mirrorOrder[:drop] {
			delete(s.mirrored, id)
		}
		s.mirrorOrder = append([]string(nil), s.mirrorOrder[drop:]...)
	}
	return false
}

func (s *Server) addLocked(a Activity) {
	s.seq++
	a.Seq = s.seq
	s.items = append(s.items, a)
	if len(s.items) > keepItems {
		s.items = append([]Activity(nil), s.items[len(s.items)-keepItems:]...)
	}
	s.broadcastLocked("activity", a.Seq, a)
}

func (s *Server) broadcastLocked(event string, id int64, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	msg := sseMsg{event, id, data}
	for ch := range s.subs {
		select {
		case ch <- msg:
		default: // a stalled client misses this; the next state catches it up
		}
	}
}

// Handler is the HTTP API, plus the page when there are assets.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/activity", s.handleActivity)
	mux.HandleFunc("GET /api/thread", s.handleThread)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/blob", s.handleBlob)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		httpError(w, http.StatusNotFound, "no such API call")
	})
	if s.Assets != nil {
		mux.Handle("/", spa(s.Assets))
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintln(w, "This holler was built without the web page (make web builds it). The API is at /api/state.")
		})
	}
	return securityHeaders(mux)
}

func (s *Server) current() (*State, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.err
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	st, errText := s.current()
	if st == nil {
		if errText == "" {
			errText = "starting"
		}
		httpError(w, http.StatusServiceUnavailable, errText)
		return
	}
	writeJSON(w, st)
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > keepItems {
		limit = 300
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	s.mu.Lock()
	items := s.since(after)
	if len(items) > limit {
		items = items[len(items)-limit:]
	}
	items = append([]Activity{}, items...)
	s.mu.Unlock()
	writeJSON(w, map[string]any{"items": items})
}

// since returns the items after seq. The caller holds mu.
func (s *Server) since(seq int64) []Activity {
	for i, a := range s.items {
		if a.Seq > seq {
			return s.items[i:]
		}
	}
	return nil
}

// Conversation is a local thread's messages.
type Conversation struct {
	Peer     string    `json:"peer"`
	Th       string    `json:"th"`
	Subject  string    `json:"subject"`
	AState   string    `json:"a_state"`
	BState   string    `json:"b_state"`
	Messages []Message `json:"messages"`
}

func (s *Server) handleThread(w http.ResponseWriter, r *http.Request) {
	peer, th := r.URL.Query().Get("peer"), r.URL.Query().Get("th")
	if peer == "" || th == "" {
		httpError(w, http.StatusBadRequest, "peer and th are required")
		return
	}
	st, _ := s.current()
	if st == nil {
		httpError(w, http.StatusServiceUnavailable, "starting")
		return
	}
	// A thread this host is part of, or else one another agent (peer)
	// shares with it.
	var res api.ReadResult
	if err := s.C.Call(r.Context(), "read", api.ReadParams{Peer: peer, Th: th, Limit: 1000, Mirrors: true}, &res); err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	if res.Thread == nil && len(res.Events) == 0 {
		httpError(w, http.StatusNotFound, "this host is not part of that thread; its conversation is private to the two agents")
		return
	}
	c := Conversation{Peer: peer, Th: th, Messages: []Message{}}
	if t := res.Thread; t != nil {
		c.Subject, c.AState, c.BState = t.Subject, t.MyState, t.TheirState
	} else {
		// Mirrored: peer may be either party, since the host records only
		// the first sharer of a thread both parties share. Its state comes
		// first, as the page expects the host's side first.
		for _, t := range st.Threads {
			if t.Th == th && (t.SharedBy == peer || t.A == peer || t.B == peer) {
				c.Subject, c.AState, c.BState = t.Subject, t.AState, t.BState
				if t.B == peer {
					c.AState, c.BState = t.BState, t.AState
				}
			}
		}
	}
	for _, ev := range res.Events {
		if c.Subject == "" {
			c.Subject = ev.Subject
		}
		if m, ok := messageFromEvent(ev, st.Self); ok {
			c.Messages = append(c.Messages, m)
		}
	}
	writeJSON(w, c)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")

	ch := make(chan sseMsg, 256)
	s.mu.Lock()
	if s.subs == nil {
		s.subs = map[chan sseMsg]struct{}{}
	}
	s.subs[ch] = struct{}{}
	st := s.state
	// A reconnecting page says what it saw last; send it what it missed.
	var missed []Activity
	if last, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64); err == nil {
		missed = append(missed, s.since(last)...)
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	send := func(m sseMsg) error {
		var b strings.Builder
		if m.id > 0 {
			fmt.Fprintf(&b, "id: %d\n", m.id)
		}
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", m.event, m.data)
		if _, err := w.Write([]byte(b.String())); err != nil {
			return err
		}
		fl.Flush()
		return nil
	}
	w.Write([]byte("retry: 2000\n\n"))
	if st != nil {
		data, _ := json.Marshal(st)
		if send(sseMsg{"state", 0, data}) != nil {
			return
		}
	}
	for _, a := range missed {
		data, _ := json.Marshal(a)
		if send(sseMsg{"activity", a.Seq, data}) != nil {
			return
		}
	}
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case m := <-ch:
			if send(m) != nil {
				return
			}
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// spa serves the built page, with index.html for any path that is not a
// file, so client-side routes survive a reload.
func spa(assets fs.FS) http.Handler {
	files := http.FileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if _, err := fs.Stat(assets, p); err == nil {
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, assets, "index.html")
	})
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		// Messages come from other agents: nothing on the page may load or
		// run anything from elsewhere.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; font-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// Raster images are shown inline; every other file downloads. Files come
// from other agents, so nothing that can carry script (HTML, SVG, PDF) is
// ever rendered from this origin.
var inlineImages = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/avif": true}

// handleBlob serves a file this host has: one it received completely, or
// one it sent. Only files the daemon recorded can be served.
func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	peer, dir, ref := q.Get("peer"), q.Get("dir"), q.Get("ref")
	if peer == "" || ref == "" || (dir != "in" && dir != "out") {
		httpError(w, http.StatusBadRequest, "peer, dir (in or out) and ref are required")
		return
	}
	var blobs []*store.Blob
	if err := s.C.Call(r.Context(), "blobs", api.PeerParams{Peer: peer}, &blobs); err != nil {
		// Mostly an unknown peer; either way, there is no such file here.
		httpError(w, http.StatusNotFound, "no such file on this host")
		return
	}
	var b *store.Blob
	for _, x := range blobs {
		if x.Dir == dir && x.Ref == ref {
			b = x
		}
	}
	if b == nil || b.Path == "" || (dir == "in" && b.Status != "complete") {
		httpError(w, http.StatusNotFound, "no such file on this host")
		return
	}
	f, err := os.Open(b.Path)
	if err != nil {
		httpError(w, http.StatusNotFound, "the file is no longer there")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		httpError(w, http.StatusNotFound, "the file is no longer there")
		return
	}
	mime := strings.ToLower(strings.TrimSpace(strings.Split(b.Mime, ";")[0]))
	name := b.Name
	if name == "" {
		name = ref
	}
	h := w.Header()
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "private, max-age=3600")
	if inlineImages[mime] {
		h.Set("Content-Type", mime)
		h.Set("Content-Disposition", "inline; filename*=UTF-8''"+url.PathEscape(name))
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	}
	http.ServeContent(w, r, "", fi.ModTime(), f)
}
