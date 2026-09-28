package node

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/agentwireprotocol/awp/internal/store"
	"github.com/agentwireprotocol/awp/wire"
)

// SendRequest describes a msg to send.
type SendRequest struct {
	Peer    string      `json:"peer"`
	Th      string      `json:"th,omitempty"` // empty starts a new thread
	Subject string      `json:"subject,omitempty"`
	Re      string      `json:"re,omitempty"`
	Parts   []wire.Part `json:"parts,omitempty"`
	Files   []string    `json:"files,omitempty"` // attached as blobs

	cleanup []string // spooled files to delete once queued
}

// SendResult reports a queued message.
type SendResult struct {
	ID        string `json:"id"`
	Th        string `json:"th"`
	Peer      string `json:"peer"`
	NewThread bool   `json:"new_thread,omitempty"`
	Connected bool   `json:"connected"`
}

// NewThreadID returns a fresh thread id.
func NewThreadID() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "thr_" + string(b)
}

// Send queues a msg. It never fails because the peer is away: the message
// goes to the outbox on disk and is delivered on the next resume.
func (n *Node) Send(req SendRequest) (*SendResult, error) {
	defer func() {
		for _, f := range req.cleanup {
			os.RemoveAll(filepath.Dir(f))
		}
	}()
	peer, err := store.GetPeer(n.st.DB(), req.Peer)
	if err != nil {
		return nil, err
	}
	if peer == nil {
		return nil, fmt.Errorf("unknown peer %s: connect to it first", wire.ShortKey(req.Peer))
	}
	if len(req.Parts) == 0 && len(req.Files) == 0 {
		return nil, errors.New("empty message: give text, parts or files")
	}
	for i, p := range req.Parts {
		if err := checkPart(p); err != nil {
			return nil, fmt.Errorf("part %d: %w", i, err)
		}
	}
	th := req.Th
	isNew := th == ""
	if isNew {
		th = NewThreadID()
	} else if t, err := store.GetThread(n.st.DB(), req.Peer, th); err != nil {
		return nil, err
	} else if t == nil {
		isNew = true
	}
	subject := ""
	if isNew {
		subject = req.Subject
		if subject == "" {
			subject = deriveSubject(req.Parts, req.Files)
		}
	}
	now := time.Now()
	var id string
	err = n.st.Tx(func(q store.Q) error {
		parts := slices.Clone(req.Parts)
		for _, f := range req.Files {
			p, err := n.queueBlob(q, req.Peer, th, f, now)
			if err != nil {
				return err
			}
			parts = append(parts, p)
		}
		// The id is taken inside the transaction: the store has one
		// connection, so id order, outbox order and commit order agree.
		// Resume's "ids greater than seen" depends on that.
		m := wire.Msg{
			Envelope: wire.Envelope{T: wire.TMsg, ID: n.ids.New(), TS: wire.FormatTime(now), Th: th, Re: req.Re},
			Subject:  subject,
			Parts:    parts,
		}
		id = m.ID
		return n.queue(q, req.Peer, m.Envelope, m, subject, now)
	})
	if err != nil {
		return nil, err
	}
	n.bump()
	n.kick(req.Peer)
	n.wakeMirrors()
	return &SendResult{ID: id, Th: th, Peer: req.Peer, NewThread: isNew, Connected: n.Connected(req.Peer)}, nil
}

// queue writes a reliable message to the outbox and the log.
func (n *Node) queue(q store.Q, peer string, env wire.Envelope, v any, subject string, now time.Time) error {
	line, err := wire.Encode(v)
	if err != nil {
		return err
	}
	if err := store.Enqueue(q, &store.OutboxRow{Peer: peer, ID: env.ID, T: env.T, Th: env.Th, Line: line, Created: now}); err != nil {
		return err
	}
	if _, err := store.Append(q, &store.Record{Peer: peer, Dir: "out", ID: env.ID, T: env.T, Th: env.Th, Line: line, At: now}); err != nil {
		return err
	}
	if env.Th != "" {
		if err := store.TouchThread(q, peer, env.Th, subject, "us", now); err != nil {
			return err
		}
		if env.T == wire.TMsg || env.T == wire.TState {
			if err := n.mirror(q, peer, env.Th, "out", line, now); err != nil {
				return err
			}
		}
	}
	// New traffic lifts a bye: the peer will be reconnected to.
	_, err = q.Exec(`UPDATE peers SET parked = 0 WHERE key = ?`, peer)
	return err
}

func checkPart(p wire.Part) error {
	switch p.K {
	case wire.PartText:
	case wire.PartCode:
	case wire.PartData:
		if len(p.Data) == 0 || !json.Valid(p.Data) {
			return errors.New("data part needs valid JSON data")
		}
	case wire.PartBlob:
		return errors.New("blob parts are created from files")
	default:
		return fmt.Errorf("unknown part kind %q", p.K)
	}
	return nil
}

func deriveSubject(parts []wire.Part, files []string) string {
	for _, p := range parts {
		if p.K == wire.PartText && strings.TrimSpace(p.Text) != "" {
			s := strings.TrimSpace(p.Text)
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = s[:i]
			}
			if r := []rune(s); len(r) > 80 {
				s = string(r[:77]) + "..."
			}
			return s
		}
	}
	if len(files) > 0 {
		return filepath.Base(files[0])
	}
	return ""
}

// SetState sends our view of a thread's state (section 8.1).
func (n *Node) SetState(peer, th, state, note string) (*SendResult, error) {
	if state == "" {
		return nil, errors.New("state is required")
	}
	t, err := store.GetThread(n.st.DB(), peer, th)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("no thread %s with %s", th, wire.ShortKey(peer))
	}
	now := time.Now()
	var id string
	err = n.st.Tx(func(q store.Q) error {
		s := wire.State{Envelope: wire.Envelope{T: wire.TState, ID: n.ids.New(), TS: wire.FormatTime(now), Th: th}, State: state, Note: note}
		id = s.ID
		if err := n.queue(q, peer, s.Envelope, s, "", now); err != nil {
			return err
		}
		return store.SetThreadState(q, peer, th, true, state, note, now)
	})
	if err != nil {
		return nil, err
	}
	n.bump()
	n.kick(peer)
	n.wakeMirrors()
	return &SendResult{ID: id, Th: th, Peer: peer, Connected: n.Connected(peer)}, nil
}

// Grant mints a grant for sub (section 10.3), records it, and sends it if
// the peer is connected; otherwise it goes out after the next resume.
func (n *Node) Grant(sub string, caps []string, ttl time.Duration) (*store.GrantRow, error) {
	if _, err := wire.ParseKey(sub); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, errors.New("ttl must be positive")
	}
	now := time.Now()
	g, err := wire.MintGrant(n.priv, sub, caps, now.Add(ttl), "")
	if err != nil {
		return nil, err
	}
	gm := wire.GrantMsg{Envelope: wire.Envelope{T: wire.TGrant, ID: n.ids.New(), TS: wire.FormatTime(now)}, Grant: g.Raw}
	line, _ := wire.Encode(gm)
	row := grantRow(g, store.RoleIssued, "", now)
	err = n.st.Tx(func(q store.Q) error {
		if err := store.EnsurePeer(q, sub, now); err != nil {
			return err
		}
		if err := store.PutGrant(q, row); err != nil {
			return err
		}
		_, err := store.Append(q, &store.Record{Peer: sub, Dir: "out", ID: gm.ID, T: wire.TGrant, Line: line, At: now,
			Meta: map[string]any{"caps": g.Caps, "exp": g.Exp}})
		return err
	})
	if err != nil {
		return nil, err
	}
	n.withConn(sub, func(c *Conn) { c.send(gm) })
	n.logf("granted %v to %s until %s", caps, wire.ShortKey(sub), g.Exp.Format(time.RFC3339))
	n.bump()
	return row, nil
}

// Revoke forgets a grant we issued. Grants are bearer statements, so a
// copy the peer holds stays valid elsewhere until it expires; locally it
// stops being honored at once.
func (n *Node) Revoke(hash string) (int64, error) {
	return n.st.DeleteGrant(hash)
}

// Introduce hands peer `to` the identity and address of peer `intro`,
// with a grant `intro` will honor if it trusts us with introduce (section
// 10.4). The grant carries aud = intro so it confers nothing on anyone else.
func (n *Node) Introduce(to, intro string, caps []string, ttl time.Duration, th string) (*SendResult, error) {
	if to == intro {
		return nil, errors.New("cannot introduce a peer to itself")
	}
	ip, err := store.GetPeer(n.st.DB(), intro)
	if err != nil {
		return nil, err
	}
	if ip == nil || len(ip.Addrs) == 0 {
		return nil, fmt.Errorf("no known address for %s", wire.ShortKey(intro))
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	now := time.Now()
	g, err := wire.MintGrant(n.priv, to, caps, now.Add(ttl), intro)
	if err != nil {
		return nil, err
	}
	msg := wire.Introduce{
		Envelope: wire.Envelope{T: wire.TIntroduce, TS: wire.FormatTime(now), Th: th},
		Peer:     wire.IntroPeer{Key: intro, Name: ip.Name, Address: ip.Addrs[0].Addr},
		Grant:    g.Raw,
	}
	if th == "" && !n.Connected(to) {
		return nil, errors.New("not connected: pass a thread to queue the introduction")
	}
	err = n.st.Tx(func(q store.Q) error {
		msg.ID = n.ids.New()
		if err := store.PutGrant(q, grantRow(g, store.RoleIssued, intro, now)); err != nil {
			return err
		}
		if th != "" {
			return n.queue(q, to, msg.Envelope, msg, "", now)
		}
		line, _ := wire.Encode(msg)
		_, err := store.Append(q, &store.Record{Peer: to, Dir: "out", ID: msg.ID, T: wire.TIntroduce, Line: line, At: now})
		return err
	})
	if err != nil {
		return nil, err
	}
	if th == "" {
		n.withConn(to, func(c *Conn) { c.send(msg) })
	} else {
		n.kick(to)
	}
	n.bump()
	return &SendResult{ID: msg.ID, Th: th, Peer: to, Connected: n.Connected(to)}, nil
}

// Bye closes the connection to a peer gracefully (section 9.6) and parks
// it: no reconnection until something new is queued for it.
func (n *Node) Bye(peer, reason string) error {
	n.st.SetParked(peer, true)
	ok := n.withConn(peer, func(c *Conn) { c.sendBye(reason) })
	if !ok {
		return errors.New("not connected (the peer is parked all the same)")
	}
	return nil
}

func (n *Node) withConn(peer string, fn func(*Conn)) bool {
	n.mu.Lock()
	p := n.peers[peer]
	var c *Conn
	if p != nil {
		c = p.conn
	}
	n.mu.Unlock()
	if c == nil {
		return false
	}
	fn(c)
	return true
}

// ResolvePeer turns what a user typed into a key: a full key, an alias, a
// name, or a unique prefix of the key's base64 body.
func (n *Node) ResolvePeer(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("no peer given")
	}
	if strings.HasPrefix(s, wire.KeyPrefix) {
		if _, err := wire.ParseKey(s); err != nil {
			return "", err
		}
		return s, nil
	}
	peers, err := n.st.Peers()
	if err != nil {
		return "", err
	}
	var exact, prefix []string
	for _, p := range peers {
		body := strings.TrimPrefix(p.Key, wire.KeyPrefix)
		switch {
		case p.Alias == s || p.Name == s:
			exact = append(exact, p.Key)
		case len(s) >= 4 && strings.HasPrefix(body, s):
			prefix = append(prefix, p.Key)
		}
	}
	for _, m := range [][]string{exact, prefix} {
		switch len(m) {
		case 0:
			continue
		case 1:
			return m[0], nil
		default:
			short := make([]string, len(m))
			for i, k := range m {
				short[i] = wire.ShortKey(k)
			}
			return "", fmt.Errorf("%q matches several peers (%s); use more of the key", s, strings.Join(short, ", "))
		}
	}
	return "", fmt.Errorf("no peer matches %q (see `awp peers`)", s)
}

// ResolveThread finds which peer a thread id belongs to.
func (n *Node) ResolveThread(th string) (string, error) {
	ts, err := n.st.Threads("", th)
	if err != nil {
		return "", err
	}
	switch len(ts) {
	case 0:
		return "", fmt.Errorf("no thread %q", th)
	case 1:
		return ts[0].Peer, nil
	}
	return "", fmt.Errorf("thread %q exists with several peers; name the peer", th)
}
