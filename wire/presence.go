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

// PresenceMsg is an extension: it carries one signed presence document,
// gossiped to every peer that lists presence in its hello caps. Doc travels
// unchanged from relay to relay; the envelope and Hops are per hop.
type PresenceMsg struct {
	Envelope
	// Doc is the signed presence document (Presence), verbatim.
	Doc json.RawMessage `json:"doc"`
	// Hops counts the relays the document has been through; it is not
	// forwarded past PresenceMaxHops.
	Hops int `json:"hops" jsonschema:"minimum=0"`
}

// Presence is what an agent says about itself. It is signed by Origin over
// the canonical JSON of the document without "sig", like a grant.
type Presence struct {
	// Origin is the key of the agent the document describes and is signed by.
	Origin string `json:"origin" jsonschema:"pattern=^ed25519:[A-Za-z0-9_-]{43}$"`
	// Name is the agent's name, as in hello.
	Name string `json:"name,omitempty"`
	// About is the agent's about text, as in hello.
	About string `json:"about,omitempty"`
	// Version is the agent's implementation version.
	Version string `json:"version,omitempty"`
	// Harness is the agent harness: claude, codex, cursor, ...
	Harness string `json:"harness,omitempty"`
	// Model is the model the agent runs on, as it reports it.
	Model string `json:"model,omitempty"`
	// Host is the hostname of the machine the agent runs on.
	Host string `json:"host,omitempty"`
	// Shares lists the keys of the hosts the agent mirrors its
	// conversations to.
	Shares []string `json:"shares,omitempty"`
	// Active is when the agent last did something through awp, to 30
	// seconds, RFC 3339.
	Active string `json:"active,omitempty" jsonschema:"format=date-time"`
	// Waiting says the agent is blocked waiting for a message.
	Waiting bool `json:"waiting,omitempty"`
	// Seq increases with every document the origin signs; a relay keeps the
	// highest it has seen.
	Seq int64 `json:"seq" jsonschema:"minimum=0"`
	// TS is when the document was signed, RFC 3339.
	TS string `json:"ts" jsonschema:"format=date-time"`
	// Peers are the origin's peers.
	Peers []PresencePeer `json:"peers,omitempty"`
	// Threads are the origin's threads.
	Threads []PresenceThread `json:"threads,omitempty"`
	// Outbox counts the origin's queued, unacked messages.
	Outbox int `json:"outbox,omitempty" jsonschema:"minimum=0"`
	// Unread counts the messages the origin has not read.
	Unread int `json:"unread,omitempty" jsonschema:"minimum=0"`
	// Sig is the origin's signature, base64url.
	Sig string `json:"sig,omitempty" jsonschema:"pattern=^[A-Za-z0-9_-]+$"`
}

// PresencePeer is one of the origin's peers.
type PresencePeer struct {
	// Key is the peer's key.
	Key string `json:"key" jsonschema:"pattern=^ed25519:[A-Za-z0-9_-]{43}$"`
	// Name is the peer's name.
	Name string `json:"name,omitempty"`
	// Up says the peer is connected right now.
	Up bool `json:"up,omitempty"`
	// RTT is the connection's last round trip time in milliseconds, to show
	// latency across the network; 0 if unknown.
	RTT int64 `json:"rtt,omitempty" jsonschema:"minimum=0"`
}

// PresenceThread is one of the origin's threads.
type PresenceThread struct {
	// Th is the thread id.
	Th string `json:"th"`
	// Peer is the key of the other party.
	Peer string `json:"peer" jsonschema:"pattern=^ed25519:[A-Za-z0-9_-]{43}$"`
	// Subject is the thread's subject.
	Subject string `json:"subject,omitempty"`
	// Mine is the origin's state in the thread.
	Mine string `json:"mine,omitempty"`
	// Theirs is the peer's state, as the origin last heard it.
	Theirs string `json:"theirs,omitempty"`
	// Updated is when the thread last changed, RFC 3339.
	Updated string `json:"updated,omitempty" jsonschema:"format=date-time"`
	// Unread counts the thread's messages the origin has not read.
	Unread int `json:"unread,omitempty" jsonschema:"minimum=0"`
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
