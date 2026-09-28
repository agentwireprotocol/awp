package wire

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// TPresence is the presence extension: a signed summary of what an agent is
// doing, gossiped across the network so that any connected host can watch
// it (NOTES.md, "Presence gossip"). It is also the hello cap that asks for
// it: presence is only sent to peers that list it.
const TPresence = "presence"

// Presence limits.
const (
	// MaxPresenceBytes caps a signed presence document.
	MaxPresenceBytes = 64 << 10
	// PresenceMaxHops bounds how far a document is forwarded.
	PresenceMaxHops = 8
)

// PresenceMsg carries one signed presence document. Doc travels unchanged
// from relay to relay; the envelope and Hops are per hop.
type PresenceMsg struct {
	Envelope
	Doc  json.RawMessage `json:"doc"`
	Hops int             `json:"hops"`
}

// Presence is what an agent says about itself. It is signed by Origin over
// the canonical JSON of the document without "sig", like a grant.
type Presence struct {
	Origin  string   `json:"origin"`
	Name    string   `json:"name,omitempty"`
	About   string   `json:"about,omitempty"`
	Version string   `json:"version,omitempty"`
	Harness string   `json:"harness,omitempty"` // the agent harness: claude, codex, cursor, ...
	Model   string   `json:"model,omitempty"`   // the model the agent runs on, as it reports it
	Host    string   `json:"host,omitempty"`    // the hostname of the machine it runs on
	Shares  []string `json:"shares,omitempty"`  // keys of the hosts it mirrors its conversations to
	// Active is when the agent last did something through awp, to 30
	// seconds; Waiting says it is blocked waiting for a message.
	Active  string           `json:"active,omitempty"`
	Waiting bool             `json:"waiting,omitempty"`
	Seq     int64            `json:"seq"`
	TS      string           `json:"ts"`
	Peers   []PresencePeer   `json:"peers,omitempty"`
	Threads []PresenceThread `json:"threads,omitempty"`
	Outbox  int              `json:"outbox,omitempty"`
	Unread  int              `json:"unread,omitempty"`
	Sig     string           `json:"sig,omitempty"`
}

// PresencePeer is one of the origin's peers.
type PresencePeer struct {
	Key  string `json:"key"`
	Name string `json:"name,omitempty"`
	Up   bool   `json:"up,omitempty"` // connected right now
	// RTT is the connection's last round trip time in milliseconds, an
	// extension to show latency across the network; 0 if unknown.
	RTT int64 `json:"rtt,omitempty"`
}

// PresenceThread is one of the origin's threads.
type PresenceThread struct {
	Th      string `json:"th"`
	Peer    string `json:"peer"`
	Subject string `json:"subject,omitempty"`
	Mine    string `json:"mine,omitempty"`   // the origin's state
	Theirs  string `json:"theirs,omitempty"` // the peer's state, as the origin last heard it
	Updated string `json:"updated,omitempty"`
	Unread  int    `json:"unread,omitempty"`
}

// Time parses the document's timestamp.
func (p *Presence) Time() time.Time {
	t, _ := ParseTime(p.TS)
	return t
}

// SignPresence fills in Origin and Sig and returns the document's wire form.
func SignPresence(priv ed25519.PrivateKey, p *Presence) (json.RawMessage, error) {
	p.Origin = FormatKey(priv.Public().(ed25519.PublicKey))
	p.Sig = ""
	body, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	obj, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	delete(obj, "sig")
	canon, err := CanonicalValue(obj)
	if err != nil {
		return nil, err
	}
	p.Sig = EncodeB64(ed25519.Sign(priv, canon))
	obj["sig"] = p.Sig
	raw, err := CanonicalValue(obj)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxPresenceBytes {
		return nil, fmt.Errorf("presence document is %d bytes, over the %d byte cap", len(raw), MaxPresenceBytes)
	}
	return raw, nil
}

// ErrPresenceInvalid means a presence document failed verification.
var ErrPresenceInvalid = errors.New("invalid presence document")

// ParsePresence verifies a signed presence document and decodes it.
func ParsePresence(raw []byte) (*Presence, error) {
	if len(raw) > MaxPresenceBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrPresenceInvalid, len(raw))
	}
	obj, err := decodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPresenceInvalid, err)
	}
	var p Presence
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPresenceInvalid, err)
	}
	origin, err := ParseKey(p.Origin)
	if err != nil {
		return nil, fmt.Errorf("%w: origin: %v", ErrPresenceInvalid, err)
	}
	sig, err := DecodeB64(p.Sig)
	if err != nil || p.Sig == "" {
		return nil, fmt.Errorf("%w: sig missing or malformed", ErrPresenceInvalid)
	}
	delete(obj, "sig")
	canon, err := CanonicalValue(obj)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPresenceInvalid, err)
	}
	if !ed25519.Verify(origin, canon, sig) {
		return nil, fmt.Errorf("%w: %v", ErrPresenceInvalid, ErrBadSignature)
	}
	if _, err := ParseTime(p.TS); err != nil {
		return nil, fmt.Errorf("%w: ts: %v", ErrPresenceInvalid, err)
	}
	return &p, nil
}

func decodeObject(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("not a JSON object")
	}
	return obj, nil
}
