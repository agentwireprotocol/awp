package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hollerprotocol/holler/internal/api"
	"github.com/hollerprotocol/holler/internal/control"
	"github.com/hollerprotocol/holler/internal/node"
	"github.com/hollerprotocol/holler/internal/store"
	"github.com/hollerprotocol/holler/internal/transport"
	"github.com/hollerprotocol/holler/internal/version"
	"github.com/hollerprotocol/holler/wire"
)

// Daemon serves the control API for one node.
type Daemon struct {
	n       *node.Node
	cfg     Config
	started time.Time
	stop    context.CancelFunc
}

// Run starts the node and serves the control socket until SIGINT, SIGTERM
// or a "shutdown" call.
func Run(cfg Config) error {
	if err := os.MkdirAll(cfg.Home, 0o700); err != nil {
		return err
	}
	unlock, err := lock(filepath.Join(cfg.Home, "daemon.lock"))
	if err != nil {
		return err
	}
	defer unlock()

	logger := log.New(os.Stderr, "", log.LstdFlags)
	nc, err := cfg.nodeConfig()
	if err != nil {
		return err
	}
	nc.Logf = logger.Printf
	if cfg.Verbose {
		nc.TailcatLogf = func(format string, args ...any) { logger.Printf("tailcat: "+format, args...) }
	}
	n, err := node.Open(nc)
	if err != nil {
		return err
	}
	defer n.Close()
	if err := n.Start(); err != nil {
		return err
	}
	pidFile := filepath.Join(cfg.Home, "daemon.pid")
	os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
	defer os.Remove(pidFile)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	d := &Daemon{n: n, cfg: cfg, started: time.Now(), stop: stop}
	logger.Printf("holler daemon %s up as %s (%s), home %s", version.String(), n.Name(), n.Key(), cfg.Home)
	err = control.Serve(ctx, cfg.Home, d)
	logger.Printf("holler daemon stopping")
	return err
}

func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another holler daemon is running with home %s", filepath.Dir(path))
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func decode[T any](params json.RawMessage) (T, error) {
	var v T
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &v); err != nil {
			return v, fmt.Errorf("bad params: %w", err)
		}
	}
	return v, nil
}

// Call implements control.Handler.
// agentCalls are the control calls an agent makes when it acts; dashboards
// only read. A read that marks messages read (an agent consuming its inbox,
// as hooks do) counts too; see touches.
var agentCalls = map[string]bool{
	"touch": true, "send": true, "state": true, "connect": true, "listen": true, "grant": true, "revoke": true,
	"introduce": true, "bye": true, "private": true, "set_model": true, "set_share": true, "alias": true,
}

// touches reports whether a call is the agent acting.
func touches(method string, params json.RawMessage) bool {
	if agentCalls[method] {
		return true
	}
	if method == "read" {
		var p struct {
			Mark bool `json:"mark"`
		}
		json.Unmarshal(params, &p)
		return p.Mark
	}
	return false
}

func (d *Daemon) Call(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if touches(method, params) {
		d.n.Touch()
	}
	switch method {
	case "touch":
		return map[string]bool{"ok": true}, nil
	case "status":
		return d.status()
	case "mirrored":
		return d.n.Store().MirroredThreads()
	case "set_share":
		p, err := decode[api.ShareParams](params)
		if err != nil {
			return nil, err
		}
		var keys []string
		for _, ref := range p.With {
			key, err := d.n.ResolvePeer(ref)
			if err != nil {
				// A key the node has not met is fine: it may connect later.
				if _, kerr := wire.ParseKey(ref); kerr != nil {
					return nil, err
				}
				key = ref
			}
			keys = append(keys, key)
		}
		if err := d.n.SetShareWith(keys); err != nil {
			return nil, err
		}
		return d.shareRefs(), nil
	case "private":
		p, err := decode[api.PrivateParams](params)
		if err != nil {
			return nil, err
		}
		key, err := d.resolve(ctx, p.Peer, p.Th)
		if err != nil {
			return nil, err
		}
		if err := d.n.MakePrivate(key, p.Th); err != nil {
			return nil, err
		}
		return map[string]string{"peer": key, "th": p.Th}, nil
	case "set_model":
		p, err := decode[api.ModelParams](params)
		if err != nil {
			return nil, err
		}
		changed, err := d.n.SetModel(p.Model)
		if err != nil {
			return nil, err
		}
		return api.ModelResult{Model: d.n.Model(), Changed: changed}, nil
	case "peers":
		return d.peerViews()
	case "connect":
		p, err := decode[api.ConnectParams](params)
		if err != nil {
			return nil, err
		}
		return d.connect(ctx, p)
	case "send":
		p, err := decode[api.SendParams](params)
		if err != nil {
			return nil, err
		}
		return d.send(ctx, p)
	case "state":
		p, err := decode[api.StateParams](params)
		if err != nil {
			return nil, err
		}
		return d.state(p)
	case "read":
		p, err := decode[api.ReadParams](params)
		if err != nil {
			return nil, err
		}
		return d.read(p)
	case "wait":
		p, err := decode[api.WaitParams](params)
		if err != nil {
			return nil, err
		}
		return d.wait(ctx, p)
	case "threads":
		p, err := decode[api.PeerParams](params)
		if err != nil {
			return nil, err
		}
		return d.threads(p)
	case "grant":
		p, err := decode[api.GrantParams](params)
		if err != nil {
			return nil, err
		}
		return d.grant(p)
	case "grants":
		return d.n.Store().Grants(store.GrantQuery{})
	case "revoke":
		p, err := decode[api.PeerParams](params)
		if err != nil {
			return nil, err
		}
		k, err := d.n.Revoke(p.Hash)
		if err == nil && k == 0 {
			err = fmt.Errorf("no grant %q", p.Hash)
		}
		return map[string]int64{"removed": k}, err
	case "introduce":
		p, err := decode[api.IntroduceParams](params)
		if err != nil {
			return nil, err
		}
		return d.introduce(p)
	case "bye":
		p, err := decode[api.PeerParams](params)
		if err != nil {
			return nil, err
		}
		key, err := d.n.ResolvePeer(p.Peer)
		if err != nil {
			return nil, err
		}
		return map[string]string{"peer": key}, d.n.Bye(key, p.Reason)
	case "alias":
		p, err := decode[api.PeerParams](params)
		if err != nil {
			return nil, err
		}
		key, err := d.n.ResolvePeer(p.Peer)
		if err != nil {
			return nil, err
		}
		return map[string]string{"peer": key, "alias": p.Alias}, d.n.Store().SetAlias(key, p.Alias)
	case "blobs":
		p, err := decode[api.PeerParams](params)
		if err != nil {
			return nil, err
		}
		key := ""
		if p.Peer != "" {
			if key, err = d.n.ResolvePeer(p.Peer); err != nil {
				return nil, err
			}
		}
		return d.n.Store().Blobs(key)
	case "presence":
		return d.presence()
	case "shutdown":
		go func() {
			time.Sleep(100 * time.Millisecond)
			d.stop()
		}()
		return map[string]bool{"ok": true}, nil
	}
	return nil, fmt.Errorf("unknown method %q", method)
}

// Stream implements control.Handler.
func (d *Daemon) Stream(ctx context.Context, method string, params json.RawMessage, emit func(any) error) (bool, error) {
	if method != "subscribe" {
		return false, nil
	}
	p, err := decode[api.SubscribeParams](params)
	if err != nil {
		return true, err
	}
	return true, d.subscribe(ctx, p, emit)
}

func (d *Daemon) status() (*api.Status, error) {
	n := d.n
	st := n.Store()
	tc, tcErr := n.TailcatAddress()
	peers, err := d.peerViews()
	if err != nil {
		return nil, err
	}
	inbox, _ := st.Query(store.Filter{Inbox: true, UnreadOnly: true, Limit: 100000})
	var unread, notices int
	for _, r := range inbox {
		if notice(r) {
			notices++
		} else {
			unread++
		}
	}
	last, waiting := n.Activity()
	var active *time.Time
	if !last.IsZero() {
		active = &last
	}
	outbox, _ := st.OutboxCount("")
	pub, _ := wire.ParseKey(n.Key())
	s := &api.Status{
		Key:         n.Key(),
		Short:       wire.ShortKey(n.Key()),
		Name:        n.Name(),
		About:       d.cfg.About,
		Harness:     n.Harness(),
		Model:       n.Model(),
		ShareWith:   d.shareRefs(),
		Waiting:     waiting,
		Active:      active,
		Host:        hostname(),
		Fingerprint: wire.Fingerprint(pub),
		Version:     version.String(),
		Home:        d.cfg.Home,
		PID:         os.Getpid(),
		Started:     d.started,
		Addresses:   n.Addresses(),
		Tailcat:     tc,
		TailcatErr:  tcErr,
		TailcatWant: slices.Contains(d.cfg.Listen, "tailcat"),
		Presence:    n.SharesPresence(),
		Serve:       d.cfg.Policy.Serve,
		Accept:      d.cfg.Policy.Accept,
		Peers:       peers,
		Unread:      unread,
		Notices:     notices,
		Outbox:      outbox,
	}
	if s.Accept == "" {
		s.Accept = node.AcceptAny
	}
	return s, nil
}

// notice reports whether an inbox record is a local notice (a connection,
// sharing or file event, dir sys) rather than something the peer sent.
// Notices are shown, but they neither count as unread nor end a wait.
func notice(r store.Record) bool { return r.Dir == "sys" }

func (d *Daemon) peerView(p *store.Peer, threads []*store.Thread) api.PeerView {
	n := d.n
	v := api.PeerView{
		Key: p.Key, Short: wire.ShortKey(p.Key), Name: p.Name, Alias: p.Alias, About: p.About,
		Caps: p.Caps, Parked: p.Parked, LastSeen: p.LastSeen, Addrs: p.Addrs,
		Granted: n.Caps(p.Key),
	}
	if ci := n.ConnInfo(p.Key); ci != nil {
		v.Connected, v.Via, v.Outbound, v.Since = true, ci.Via, ci.Outbound, ci.Since
		if ci.RTT > 0 {
			v.RTTms = max(1, ci.RTT.Milliseconds())
		}
	}
	v.Dialing, v.DialErr = n.Dialing(p.Key)
	v.Outbox, _ = n.Store().OutboxCount(p.Key)
	for _, t := range threads {
		if t.Peer != p.Key {
			continue
		}
		if t.Open() {
			v.OpenThreads++
		}
		v.Unread += t.Unread
	}
	return v
}

func (d *Daemon) peerViews() ([]api.PeerView, error) {
	peers, err := d.n.Store().Peers()
	if err != nil {
		return nil, err
	}
	threads, _ := d.n.Store().Threads("", "")
	out := []api.PeerView{}
	for _, p := range peers {
		if p.Name == "" && p.LastSeen.IsZero() && len(p.Addrs) == 0 {
			continue // a key we only granted to or heard of, never met
		}
		out = append(out, d.peerView(p, threads))
	}
	slices.SortStableFunc(out, func(a, b api.PeerView) int {
		if a.Connected != b.Connected {
			if a.Connected {
				return -1
			}
			return 1
		}
		return 0
	})
	return out, nil
}

func (d *Daemon) view(key string) api.PeerView {
	p, err := store.GetPeer(d.n.Store().DB(), key)
	if err != nil || p == nil {
		return api.PeerView{Key: key, Short: wire.ShortKey(key)}
	}
	threads, _ := d.n.Store().Threads(key, "")
	return d.peerView(p, threads)
}

func (d *Daemon) connect(ctx context.Context, p api.ConnectParams) (*api.ConnectResult, error) {
	start := time.Now()
	timeout := time.Duration(p.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addr := p.Address
	// A known peer's name or key reconnects to its best known address.
	if _, err := transport.Parse(addr); err != nil {
		key, rerr := d.n.ResolvePeer(addr)
		if rerr != nil {
			return nil, fmt.Errorf("%v; and %v", err, rerr)
		}
		pv := d.view(key)
		if pv.Connected {
			return &api.ConnectResult{Peer: pv, Existing: true}, nil
		}
		if len(pv.Addrs) == 0 {
			return nil, fmt.Errorf("no known address for %s", pv.Label())
		}
		var lastErr error
		for _, a := range pv.Addrs {
			if k, err := d.n.Connect(ctx, a.Addr); err == nil && k == key {
				return d.connected(k, start), nil
			} else if err != nil {
				lastErr = err
			}
		}
		return nil, lastErr
	}
	key, err := d.n.Connect(ctx, addr)
	if err != nil {
		return nil, err
	}
	return d.connected(key, start), nil
}

// connected reports key's connection, noting whether it was already up
// before start: the node reuses a live connection rather than dialing.
func (d *Daemon) connected(key string, start time.Time) *api.ConnectResult {
	res := &api.ConnectResult{Peer: d.view(key)}
	if ci := d.n.ConnInfo(key); ci != nil && ci.Since.Before(start) {
		res.Existing = true
	}
	return res
}

// resolve turns a peer argument (key, name, alias, prefix or address) and
// optional thread into a key.
func (d *Daemon) resolve(ctx context.Context, peer, th string) (string, error) {
	if peer == "" {
		if th == "" {
			return "", errors.New("name a peer or a thread")
		}
		return d.n.ResolveThread(th)
	}
	if _, err := transport.Parse(peer); err == nil {
		res, err := d.connect(ctx, api.ConnectParams{Address: peer})
		if err != nil {
			return "", err
		}
		return res.Peer.Key, nil
	}
	return d.n.ResolvePeer(peer)
}

func (d *Daemon) send(ctx context.Context, p api.SendParams) (*api.SendResult, error) {
	key, err := d.resolve(ctx, p.Peer, p.Th)
	if err != nil {
		return nil, err
	}
	for _, f := range p.Files {
		if !filepath.IsAbs(f) {
			return nil, fmt.Errorf("file path %q must be absolute", f)
		}
	}
	res, err := d.n.Send(node.SendRequest{Peer: key, Th: p.Th, Subject: p.Subject, Re: p.Re, Parts: p.Parts, Files: p.Files})
	if err != nil {
		return nil, err
	}
	out := &api.SendResult{ID: res.ID, Th: res.Th, Thread: res.Th, Peer: key, PeerName: d.view(key).Label(), NewThread: res.NewThread, Connected: res.Connected}
	wait := time.Duration(p.WaitAck) * time.Millisecond
	if p.WaitAck == 0 && res.Connected {
		wait = time.Second // on a live link the ack lands well within that
	}
	if wait > 0 {
		out.Acked = d.waitAck(ctx, key, res.ID, wait)
		out.Connected = d.n.Connected(key)
	}
	return out, nil
}

func (d *Daemon) waitAck(ctx context.Context, peer, id string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		ch := d.n.Changed()
		var acked bool
		d.n.Store().DB().QueryRow(`SELECT acked FROM log WHERE peer = ? AND dir = 'out' AND id = ?`, peer, id).Scan(&acked)
		if acked {
			return true
		}
		select {
		case <-ch:
		case <-deadline:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func (d *Daemon) state(p api.StateParams) (*api.SendResult, error) {
	key, err := d.resolve(context.Background(), p.Peer, p.Th)
	if err != nil {
		return nil, err
	}
	res, err := d.n.SetState(key, p.Th, p.State, p.Note)
	if err != nil {
		return nil, err
	}
	return &api.SendResult{ID: res.ID, Th: res.Th, Thread: res.Th, Peer: key, PeerName: d.view(key).Label(), Connected: res.Connected}, nil
}

func (d *Daemon) filterPeer(peer string) (string, error) {
	if peer == "" {
		return "", nil
	}
	return d.n.ResolvePeer(peer)
}

func (d *Daemon) read(p api.ReadParams) (*api.ReadResult, error) {
	key, err := d.filterPeer(p.Peer)
	if err != nil {
		return nil, err
	}
	f := store.Filter{Peer: key, Th: p.Th, Inbox: p.Inbox, UnreadOnly: p.Unread, AfterSeq: p.Since, Limit: p.Limit, Mirrors: p.Mirrors}
	if f.Limit <= 0 {
		f.Limit = 200
	}
	if !p.Unread && p.Since == 0 {
		f.Last = true // history: the most recent records
	}
	recs, err := d.n.Store().Query(f)
	if err != nil {
		return nil, err
	}
	res := &api.ReadResult{Events: d.events(recs), Cursor: p.Since}
	if len(recs) > 0 {
		res.Cursor = recs[len(recs)-1].Seq
	}
	if p.Mark {
		d.markRead(recs)
	}
	if p.Th != "" {
		if ts, _ := d.n.Store().Threads(key, p.Th); len(ts) == 1 {
			res.Thread = ts[0]
		}
	}
	return res, nil
}

func (d *Daemon) markRead(recs []store.Record) {
	var seqs []int64
	for _, r := range recs {
		if !r.Read && r.Dir != "out" {
			seqs = append(seqs, r.Seq)
		}
	}
	d.n.Store().MarkRead(seqs)
}

func (d *Daemon) wait(ctx context.Context, p api.WaitParams) (*api.ReadResult, error) {
	done := d.n.Waiting()
	defer done()
	key := ""
	var err error
	if p.Peer != "" || p.Th != "" {
		if key, err = d.resolve(ctx, p.Peer, p.Th); err != nil {
			return nil, err
		}
	}
	if len(p.States) > 0 && p.Th == "" {
		return nil, errors.New("waiting for a state needs a thread")
	}
	timeout := time.Duration(p.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	deadline := time.After(timeout)
	for {
		ch := d.n.Changed()
		recs, err := d.n.Store().Query(store.Filter{Peer: key, Th: p.Th, Inbox: true, UnreadOnly: true})
		if err != nil {
			return nil, err
		}
		var thread *store.Thread
		if p.Th != "" {
			if ts, _ := d.n.Store().Threads(key, p.Th); len(ts) == 1 {
				thread = ts[0]
			}
		}
		// Something the peer sent ends the wait, and so does the awaited
		// state. Notices alone do not: they come back with whatever does.
		done := slices.ContainsFunc(recs, func(r store.Record) bool { return !notice(r) })
		matched := len(p.States) > 0 && thread != nil && slices.Contains(p.States, thread.TheirState)
		if done || matched {
			d.markRead(recs)
			return &api.ReadResult{Events: d.events(recs), Cursor: lastSeq(recs), Matched: matched, Thread: thread}, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			d.markRead(recs)
			return &api.ReadResult{Events: d.events(recs), Cursor: lastSeq(recs), TimedOut: true, Thread: thread}, nil
		}
	}
}

func lastSeq(recs []store.Record) int64 {
	if len(recs) == 0 {
		return 0
	}
	return recs[len(recs)-1].Seq
}

func (d *Daemon) subscribe(ctx context.Context, p api.SubscribeParams, emit func(any) error) error {
	key, err := d.filterPeer(p.Peer)
	if err != nil {
		return err
	}
	var cursor int64
	if p.Since != nil {
		cursor = *p.Since
	} else {
		cursor, _ = d.n.Store().MaxSeq()
	}
	for {
		ch := d.n.Changed()
		recs, err := d.n.Store().Query(store.Filter{Peer: key, Th: p.Th, Inbox: p.Inbox, AfterSeq: cursor, Limit: 500, Mirrors: p.Mirrors})
		if err != nil {
			return err
		}
		for _, ev := range d.events(recs) {
			if err := emit(ev); err != nil {
				return err
			}
		}
		if len(recs) > 0 {
			cursor = recs[len(recs)-1].Seq
			if p.Mark {
				d.markRead(recs)
			}
			continue
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil
		}
	}
}

// events turns log records into client events, adding peer names, thread
// subjects and local blob state.
func (d *Daemon) events(recs []store.Record) []api.Event {
	st := d.n.Store()
	names := map[string]string{}
	subjects := map[string]string{}
	label := func(key string) string {
		if l, ok := names[key]; ok {
			return l
		}
		l := wire.ShortKey(key)
		if p, _ := store.GetPeer(st.DB(), key); p != nil {
			switch {
			case p.Alias != "":
				l = p.Alias
			case p.Name != "":
				l = p.Name
			}
		}
		names[key] = l
		return l
	}
	subject := func(peer, th string) string {
		k := peer + "\x00" + th
		if s, ok := subjects[k]; ok {
			return s
		}
		s := ""
		if t, _ := store.GetThread(st.DB(), peer, th); t != nil {
			s = t.Subject
		}
		subjects[k] = s
		return s
	}
	out := make([]api.Event, 0, len(recs))
	for _, r := range recs {
		ev := api.Event{Seq: r.Seq, At: r.At, Dir: r.Dir, Type: r.T, Peer: r.Peer, PeerName: label(r.Peer), Th: r.Th, ID: r.ID, Acked: r.Acked, Meta: r.Meta}
		if r.Dir != "sys" {
			ev.Msg = json.RawMessage(r.Line)
		} else {
			ev.ID = ""
		}
		if r.Th != "" {
			ev.Subject = subject(r.Peer, r.Th)
		}
		if r.T == wire.TMsg {
			dir := "in"
			if r.Dir == "out" {
				dir = "out"
			}
			for _, p := range ev.Parts() {
				if p.K != wire.PartBlob {
					continue
				}
				if b, _ := store.GetBlob(st.DB(), r.Peer, dir, p.Ref); b != nil {
					if ev.Blobs == nil {
						ev.Blobs = map[string]api.BlobInfo{}
					}
					ev.Blobs[p.Ref] = api.BlobInfo{Name: b.Name, Mime: b.Mime, Size: b.Size, Received: b.Received, Status: b.Status, Path: b.Path}
				}
			}
		}
		out = append(out, ev)
	}
	return out
}

func (d *Daemon) threads(p api.PeerParams) ([]*store.Thread, error) {
	key, err := d.filterPeer(p.Peer)
	if err != nil {
		return nil, err
	}
	return d.n.Store().Threads(key, "")
}

func (d *Daemon) grant(p api.GrantParams) (*store.GrantRow, error) {
	key, err := d.n.ResolvePeer(p.Peer)
	if err != nil {
		return nil, err
	}
	ttl := time.Hour
	if p.TTL != "" {
		if ttl, err = time.ParseDuration(p.TTL); err != nil {
			return nil, fmt.Errorf("ttl: %w", err)
		}
	}
	for _, c := range p.Caps {
		if strings.TrimSpace(c) == "" {
			return nil, errors.New("empty capability name")
		}
	}
	return d.n.Grant(key, p.Caps, ttl)
}

func (d *Daemon) introduce(p api.IntroduceParams) (*api.SendResult, error) {
	to, err := d.n.ResolvePeer(p.To)
	if err != nil {
		return nil, err
	}
	intro, err := d.n.ResolvePeer(p.Peer)
	if err != nil {
		return nil, err
	}
	ttl := time.Hour
	if p.TTL != "" {
		if ttl, err = time.ParseDuration(p.TTL); err != nil {
			return nil, fmt.Errorf("ttl: %w", err)
		}
	}
	res, err := d.n.Introduce(to, intro, p.Caps, ttl, p.Th)
	if err != nil {
		return nil, err
	}
	return &api.SendResult{ID: res.ID, Th: res.Th, Thread: res.Th, Peer: to, PeerName: d.view(to).Label(), Connected: res.Connected}, nil
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// shareRefs names the hosts this agent shares its conversations with.
func (d *Daemon) shareRefs() []api.PeerRef {
	refs := []api.PeerRef{}
	for _, k := range d.n.ShareWith() {
		ref := api.PeerRef{Key: k}
		if p, err := store.GetPeer(d.n.Store().DB(), k); err == nil && p != nil {
			ref.Name = p.Name
		}
		refs = append(refs, ref)
	}
	return refs
}
