// Package api defines the types exchanged between the awp daemon and its
// local clients over the control socket.
package api

import (
	"encoding/json"
	"time"

	"github.com/agentwireprotocol/awp/internal/store"
	"github.com/agentwireprotocol/awp/wire"
)

// Status describes the running daemon.
type Status struct {
	Key         string     `json:"key"`
	Short       string     `json:"short"`
	Name        string     `json:"name"`
	About       string     `json:"about"`
	Harness     string     `json:"harness,omitempty"`    // the agent harness this daemon runs in
	ShareWith   []PeerRef  `json:"share_with,omitempty"` // hosts this agent mirrors its conversations to
	Model       string     `json:"model,omitempty"`      // the model the agent runs on, as last reported
	Active      *time.Time `json:"active,omitempty"`     // when the agent last acted through awp
	Waiting     bool       `json:"waiting,omitempty"`    // blocked in awp wait now
	Host        string     `json:"host,omitempty"`       // this machine's hostname
	Fingerprint string     `json:"fingerprint"`
	Version     string     `json:"version"`
	Home        string     `json:"home"`
	PID         int        `json:"pid"`
	Started     time.Time  `json:"started"`
	Addresses   []string   `json:"addresses"`
	Tailcat     string     `json:"tailcat,omitempty"` // bare tc... address to share
	TailcatErr  string     `json:"tailcat_error,omitempty"`
	TailcatWant bool       `json:"tailcat_enabled"`
	Presence    bool       `json:"presence,omitempty"` // publishing presence (see NOTES.md)
	Serve       []string   `json:"serve,omitempty"`
	Accept      string     `json:"accept"`
	Peers       []PeerView `json:"peers"`
	Unread      int        `json:"unread"`  // messages and states from peers not yet read
	Notices     int        `json:"notices"` // unread local notices (connection, sharing and file events)
	Outbox      int        `json:"outbox"`
}

// ShareAddress is the address to hand to another agent: the tailcat
// address when there is one, else the first listen address.
func (s *Status) ShareAddress() string {
	if s.Tailcat != "" {
		return s.Tailcat
	}
	if len(s.Addresses) > 0 {
		return s.Addresses[0]
	}
	return ""
}

// PeerView is a peer as the daemon sees it right now.
type PeerView struct {
	Key         string       `json:"key"`
	Short       string       `json:"short"`
	Name        string       `json:"name,omitempty"`
	Alias       string       `json:"alias,omitempty"`
	About       string       `json:"about,omitempty"`
	Caps        []string     `json:"caps,omitempty"`    // message families from its hello
	Granted     []string     `json:"granted,omitempty"` // capabilities it holds on us
	Connected   bool         `json:"connected"`
	Via         string       `json:"via,omitempty"`
	Outbound    bool         `json:"outbound,omitempty"`
	Since       time.Time    `json:"since,omitzero"`
	Dialing     bool         `json:"dialing,omitempty"`
	DialErr     string       `json:"dial_error,omitempty"`
	RTTms       int64        `json:"rtt_ms,omitempty"` // the connection\'s last ping round trip
	Parked      bool         `json:"parked,omitempty"`
	Outbox      int          `json:"outbox"`
	OpenThreads int          `json:"open_threads"`
	Unread      int          `json:"unread"`
	LastSeen    time.Time    `json:"last_seen,omitzero"`
	Addrs       []store.Addr `json:"addrs,omitempty"`
}

// Label is how a peer is shown to people: alias, else name, else short key.
func (p PeerView) Label() string {
	switch {
	case p.Alias != "":
		return p.Alias
	case p.Name != "":
		return p.Name
	}
	return p.Short
}

// Event is one log record as clients see it. Msg is the wire object for
// sent and received messages; Meta holds local annotations (capability
// checks, blob paths, connection details for sys events).
type Event struct {
	Seq      int64               `json:"seq"`
	At       time.Time           `json:"at"`
	Dir      string              `json:"dir"`  // in, out or sys
	Type     string              `json:"type"` // msg, state, grant, introduce, err, blob, bye, connected, ...
	Peer     string              `json:"peer"`
	PeerName string              `json:"peer_name,omitempty"`
	Th       string              `json:"th,omitempty"`
	Subject  string              `json:"subject,omitempty"` // the thread's subject
	ID       string              `json:"id,omitempty"`
	Acked    bool                `json:"acked,omitempty"`
	Msg      json.RawMessage     `json:"msg,omitempty"`
	Meta     map[string]any      `json:"meta,omitempty"`
	Blobs    map[string]BlobInfo `json:"blobs,omitempty"`
}

// BlobInfo is the local state of a blob referenced by a message.
type BlobInfo struct {
	Name     string `json:"name,omitempty"`
	Mime     string `json:"mime,omitempty"`
	Size     int64  `json:"size"`
	Received int64  `json:"received"`
	Status   string `json:"status"`
	Path     string `json:"path,omitempty"`
}

// Parts decodes the message parts of a msg event.
func (e *Event) Parts() []wire.Part {
	var m wire.Msg
	json.Unmarshal(e.Msg, &m)
	return m.Parts
}

// ConnectParams for "connect".
type ConnectParams struct {
	Address   string `json:"address"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

// ConnectResult for "connect". Existing says the connection was already
// up before the call.
type ConnectResult struct {
	Peer     PeerView `json:"peer"`
	Existing bool     `json:"existing,omitempty"`
}

// SendParams for "send". Peer may be a key, name, alias, key prefix or an
// address (which is connected to first). With Th set, Peer may be empty.
type SendParams struct {
	Peer    string      `json:"peer,omitempty"`
	Th      string      `json:"th,omitempty"`
	Subject string      `json:"subject,omitempty"`
	Re      string      `json:"re,omitempty"`
	Parts   []wire.Part `json:"parts,omitempty"`
	Files   []string    `json:"files,omitempty"` // absolute paths
	// WaitAck is how long to wait for the peer's ack, in milliseconds. Zero
	// means the default: one second when the peer is connected, none
	// otherwise. A negative value does not wait.
	WaitAck int `json:"wait_ack_ms,omitempty"`
}

// SendResult for "send" and "state".
type SendResult struct {
	ID        string `json:"id"`
	Th        string `json:"th"`
	Thread    string `json:"thread"` // th again, under the name the --thread flag uses
	Peer      string `json:"peer"`
	PeerName  string `json:"peer_name,omitempty"`
	NewThread bool   `json:"new_thread,omitempty"`
	Connected bool   `json:"connected"`
	Acked     bool   `json:"acked"`
}

// StateParams for "state".
type StateParams struct {
	Peer  string `json:"peer,omitempty"`
	Th    string `json:"th"`
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
}

// ReadParams for "read": history (Inbox false) or unread inbox (Inbox and
// Unread true).
type ReadParams struct {
	Peer   string `json:"peer,omitempty"`
	Th     string `json:"th,omitempty"`
	Inbox  bool   `json:"inbox,omitempty"`  // only received messages and notable local events
	Unread bool   `json:"unread,omitempty"` // only records not yet marked read
	Since  int64  `json:"since,omitempty"`  // after this seq
	Limit  int    `json:"limit,omitempty"`
	Mark   bool   `json:"mark,omitempty"` // mark returned records read
	// Mirrors includes lines other agents share with this host.
	Mirrors bool `json:"mirrors,omitempty"`
}

// ReadResult for "read" and "wait".
type ReadResult struct {
	Events   []Event       `json:"events"`
	Cursor   int64         `json:"cursor"`
	TimedOut bool          `json:"timed_out,omitempty"`
	Matched  bool          `json:"matched,omitempty"` // wait: the thread reached one of the states asked for
	Thread   *store.Thread `json:"thread,omitempty"`
}

// WaitParams for "wait": block until unread inbox events arrive (optionally
// in one thread or from one peer). With States, also until the thread's
// remote state is one of them; Matched in the result says which it was.
// Notices alone do not end a wait: they are returned with whatever does.
type WaitParams struct {
	Peer      string   `json:"peer,omitempty"`
	Th        string   `json:"th,omitempty"`
	States    []string `json:"states,omitempty"`
	TimeoutMS int      `json:"timeout_ms,omitempty"`
}

// SubscribeParams for the "subscribe" stream. Since nil means "from now".
type SubscribeParams struct {
	Peer  string `json:"peer,omitempty"`
	Th    string `json:"th,omitempty"`
	Since *int64 `json:"since,omitempty"`
	Inbox bool   `json:"inbox,omitempty"`
	Mark  bool   `json:"mark,omitempty"`
	// Mirrors includes lines other agents share with this host.
	Mirrors bool `json:"mirrors,omitempty"`
}

// GrantParams for "grant".
type GrantParams struct {
	Peer string   `json:"peer"`
	Caps []string `json:"caps"`
	TTL  string   `json:"ttl,omitempty"` // Go duration, default 1h
}

// IntroduceParams for "introduce".
type IntroduceParams struct {
	To   string   `json:"to"`
	Peer string   `json:"peer"`
	Caps []string `json:"caps,omitempty"`
	TTL  string   `json:"ttl,omitempty"`
	Th   string   `json:"th,omitempty"`
}

// PeerParams names a peer (bye, alias, threads...).
type PeerParams struct {
	Peer   string `json:"peer,omitempty"`
	Reason string `json:"reason,omitempty"`
	Alias  string `json:"alias,omitempty"`
	Hash   string `json:"hash,omitempty"`
}

// ModelParams for "set_model": the model the agent now runs on.
type ModelParams struct {
	Model string `json:"model"`
}

// ModelResult says whether the model changed.
type ModelResult struct {
	Model   string `json:"model"`
	Changed bool   `json:"changed"`
}

// PeerRef names a peer.
type PeerRef struct {
	Key  string `json:"key"`
	Name string `json:"name,omitempty"`
}

// ShareParams for "set_share": the hosts to mirror this agent's
// conversations to (names, aliases, keys); empty stops sharing.
type ShareParams struct {
	With []string `json:"with"`
}

// PrivateParams for "private": keep a thread out of conversation sharing.
type PrivateParams struct {
	Peer string `json:"peer,omitempty"`
	Th   string `json:"th"`
}
