// Package wire implements the wire format of the Agent Wire Protocol
// (AWP): NDJSON framing, the message envelope, the typed messages of SPEC.md sections 6 to 10, Ed25519
// key encoding, canonical JSON and signed grants.
//
// The package has no I/O policy of its own. It is shared by the daemon, the
// tests and anyone who wants to write an AWP peer in Go.
package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"
)

// The JSON Schema in schema/v0/awp.schema.json and its reference page
// schema/v0/schema.mdx are generated from this package. `make schema`
// regenerates them; a test fails when they are stale.
//
//go:generate go run ../internal/schemagen

// Protocol constants.
const (
	// Version is the protocol major version sent in hello.v.
	Version = 0

	// MaxLine is the largest line a peer accepts, excluding the newline.
	MaxLine = 1 << 20

	// ChunkSize is the recommended chunk payload size before encoding.
	ChunkSize = 256 << 10

	// AuthContext prefixes the byte string signed in auth.
	AuthContext = "awp-auth-v0"
)

// Message types.
const (
	THello     = "hello"
	TAuth      = "auth"
	TResume    = "resume"
	TMsg       = "msg"
	TState     = "state"
	TAck       = "ack"
	TChunk     = "chunk"
	TPing      = "ping"
	TPong      = "pong"
	TBye       = "bye"
	TErr       = "err"
	TGrant     = "grant"
	TIntroduce = "introduce"
)

// Message pairs a message type with the Go type that holds it.
type Message struct {
	T    string
	Type reflect.Type
}

// Messages lists every message type this package defines, with its Go
// type: the core messages of SPEC.md first, in the order the spec defines
// them, then the extensions.
func Messages() []Message {
	return []Message{
		{THello, reflect.TypeFor[Hello]()},
		{TAuth, reflect.TypeFor[Auth]()},
		{TResume, reflect.TypeFor[Resume]()},
		{TState, reflect.TypeFor[State]()},
		{TMsg, reflect.TypeFor[Msg]()},
		{TAck, reflect.TypeFor[Ack]()},
		{TChunk, reflect.TypeFor[Chunk]()},
		{TPing, reflect.TypeFor[Ping]()},
		{TPong, reflect.TypeFor[Pong]()},
		{TBye, reflect.TypeFor[Bye]()},
		{TErr, reflect.TypeFor[Err]()},
		{TGrant, reflect.TypeFor[GrantMsg]()},
		{TIntroduce, reflect.TypeFor[Introduce]()},
		{TPresence, reflect.TypeFor[PresenceMsg]()},
		{TMirror, reflect.TypeFor[Mirror]()},
		{TPrivate, reflect.TypeFor[Private]()},
	}
}

// Extensions lists the message types that are not in SPEC.md: this
// implementation speaks them, peers that do not know them ignore them.
func Extensions() []string {
	return []string{TPresence, TMirror, TPrivate}
}

// Error codes from section 9.7.
const (
	ErrBadFrame    = "bad_frame"
	ErrVersion     = "version"
	ErrAuth        = "auth"
	ErrUnsupported = "unsupported"
	ErrForbidden   = "forbidden"
	ErrBlobRefused = "blob_refused"
	ErrTooLarge    = "too_large"
	ErrInternal    = "internal"
)

// ErrCodes lists the error codes of section 9.7, in the spec's order.
func ErrCodes() []string {
	return []string{ErrBadFrame, ErrVersion, ErrAuth, ErrUnsupported, ErrForbidden, ErrBlobRefused, ErrTooLarge, ErrInternal}
}

// ErrCloses reports whether an err with this code closes the connection.
// Section 9.7 lists forbidden, unsupported, blob_refused and internal as
// non-closing. Unknown codes are treated as non-closing too, so a newer peer
// cannot tear down a connection just by inventing a code.
func ErrCloses(code string) bool {
	switch code {
	case ErrBadFrame, ErrVersion, ErrAuth, ErrTooLarge:
		return true
	}
	return false
}

// Part kinds.
const (
	PartText = "text"
	PartCode = "code"
	PartData = "data"
	PartBlob = "blob"
)

// PartKind says which fields a part of one kind carries (section 9.1):
// Required are always present, Optional only when set. Part.MarshalJSON
// follows this table, and the JSON Schema is generated from it.
type PartKind struct {
	K        string
	Doc      string
	Required []string
	Optional []string
}

// PartKinds lists the part kinds of section 9.1.
func PartKinds() []PartKind {
	return []PartKind{
		{PartText, "Text, markdown by convention.", []string{"text"}, nil},
		{PartCode, "Fenced code without the fence; lang names the language.", []string{"text"}, []string{"lang"}},
		{PartData, "Inline JSON, with its MIME type. Requests (section 10.1) are data parts of a vnd.awp type.", []string{"data"}, []string{"mime"}},
		{PartBlob, "A blob sent in chunk messages before or after this message, named by ref.", []string{"ref", "size"}, []string{"name", "mime"}},
	}
}

// Recommended thread states from section 8.1.
const (
	StateOpen    = "open"
	StateWorking = "working"
	StateWaiting = "waiting"
	StateDone    = "done"
	StateFailed  = "failed"
	StateClosed  = "closed"
)

// ThreadStates lists the recommended states of section 8.1. Peers may use
// others.
func ThreadStates() []string {
	return []string{StateOpen, StateWorking, StateWaiting, StateDone, StateFailed, StateClosed}
}

// Patterns for the encoded fields, as the JSON Schema states them: keys are
// "ed25519:" and 43 characters of unpadded base64url (section 7.1); nonces
// and signatures are unpadded base64url; chunk data is standard base64
// (section 9.3). Receivers are more liberal (DecodeB64).
const (
	KeyPattern    = "^ed25519:[A-Za-z0-9_-]{43}$"
	B64URLPattern = "^[A-Za-z0-9_-]+$"
	B64Pattern    = "^[A-Za-z0-9+/]*={0,2}$"
)

// Envelope holds the fields every line carries (section 6). The
// type-specific fields sit beside them at the top level.
type Envelope struct {
	// T is the message type.
	T string `json:"t"`
	// ID is unique per sender. A ULID or UUIDv7 is recommended, so that ids
	// sort by time; resume compares ids as strings.
	ID string `json:"id" jsonschema:"minLength=1"`
	// TS is an RFC 3339 UTC timestamp.
	TS string `json:"ts" jsonschema:"format=date-time"`
	// Th is the thread id. Required for msg, state and ack.
	Th string `json:"th,omitempty"`
	// Re is the id of the message this one responds to.
	Re string `json:"re,omitempty"`
}

// Hello is the first line each side sends, at once, without waiting for
// the other (section 7.1).
type Hello struct {
	Envelope
	// V is the protocol major version. A peer that sees a version it does
	// not speak sends err version and closes.
	V int `json:"v" jsonschema:"minimum=0"`
	// Key is the long-term Ed25519 public key. Its bytes are the peer's
	// identity.
	Key string `json:"key" jsonschema:"pattern=^ed25519:[A-Za-z0-9_-]{43}$"`
	// Name is how the peer calls itself, "harness@host" by convention.
	Name string `json:"name,omitempty"`
	// Nonce is 32 random bytes, base64url. The auth signature covers the
	// whole hello line, nonce included.
	Nonce string `json:"nonce" jsonschema:"pattern=^[A-Za-z0-9_-]+$"`
	// Caps lists the supported message families beyond the mandatory chat
	// and resume: blob, grant, introduce, and any extension.
	Caps []string `json:"caps,omitempty"`
	// About is free text for the other agent's context. It is not
	// authenticated until auth completes.
	About string `json:"about,omitempty"`

	// Addr is an extension: an address at which the sender can be reached,
	// so that either side can reconnect (section 4.3). Peers that do not
	// know it ignore it, as section 5 requires.
	Addr string `json:"addr,omitempty"`

	// Shares is an extension (mirror.go): the keys of the hosts this agent
	// mirrors its conversations to, so the other party knows.
	Shares []string `json:"shares,omitempty"`
}

// Auth proves possession of the key sent in hello (section 7.2).
type Auth struct {
	Envelope
	// Sig is the Ed25519 signature, base64url, over "awp-auth-v0" || 0x00 ||
	// my hello line || 0x00 || peer hello line, the lines as sent and
	// received without the newline.
	Sig string `json:"sig" jsonschema:"pattern=^[A-Za-z0-9_-]+$"`
	// Grants are grant objects (section 10.2) the sender presents.
	Grants []json.RawMessage `json:"grants,omitempty"`
}

// Resume is sent by both sides after every handshake (section 9.5). The
// receiver replays its outbox messages the sender has not seen.
type Resume struct {
	Envelope
	// Seen maps each thread id to the last message id the sender has
	// durably received in it. Empty on a first connection.
	Seen map[string]string `json:"seen"`
}

// Msg is one turn in a thread (section 9.1). The first msg with a new th
// creates the thread.
type Msg struct {
	Envelope
	// Subject is the thread's title, meaningful on its first message.
	Subject string `json:"subject,omitempty"`
	// Parts are the message's pieces, in order.
	Parts []Part `json:"parts"`
}

// Part is one piece of a message. Which fields are meaningful depends on K
// (PartKinds).
type Part struct {
	// K is the part kind: text, code, data or blob.
	K string `json:"k"`
	// Text is the text of a text or code part.
	Text string `json:"text,omitempty"`
	// Lang names a code part's language, as a fenced block would.
	Lang string `json:"lang,omitempty"`
	// Data is a data part's inline JSON.
	Data json.RawMessage `json:"data,omitempty"`
	// Mime is the media type of a data or blob part.
	Mime string `json:"mime,omitempty"`
	// Ref names the blob a blob part refers to, as the chunks carry it.
	Ref string `json:"ref,omitempty"`
	// Name is a blob part's file name.
	Name string `json:"name,omitempty"`
	// Size is a blob part's size in bytes, before encoding.
	Size int64 `json:"size,omitempty" jsonschema:"minimum=0"`
}

// MarshalJSON emits exactly the fields that belong to the part's kind, so a
// zero-length blob still carries "size":0 and a text part never grows a
// stray mime.
func (p Part) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"k":`)
	writeJSON(&buf, p.K)
	field := func(name string, v any) {
		buf.WriteString(`,"` + name + `":`)
		writeJSON(&buf, v)
	}
	switch p.K {
	case PartText:
		field("text", p.Text)
	case PartCode:
		if p.Lang != "" {
			field("lang", p.Lang)
		}
		field("text", p.Text)
	case PartData:
		if p.Mime != "" {
			field("mime", p.Mime)
		}
		data := p.Data
		if len(data) == 0 {
			data = json.RawMessage("null")
		}
		buf.WriteString(`,"data":`)
		buf.Write(data)
	case PartBlob:
		field("ref", p.Ref)
		if p.Name != "" {
			field("name", p.Name)
		}
		if p.Mime != "" {
			field("mime", p.Mime)
		}
		field("size", p.Size)
	default:
		// Unknown kind: emit whatever we have.
		type plain Part
		b, err := json.Marshal(plain(p))
		if err != nil {
			return nil, err
		}
		return b, nil
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func writeJSON(buf *bytes.Buffer, v any) {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	buf.Truncate(buf.Len() - 1) // Encode appends a newline
}

// State is the sender's view of a thread's soft state (section 8.1). The
// two sides can disagree.
type State struct {
	Envelope
	// State is the thread state: open, working, waiting, done, failed or
	// closed by convention; peers may use others.
	State string `json:"state" jsonschema:"example=open,example=working,example=waiting,example=done,example=failed,example=closed"`
	// Note says more, such as why a thread failed.
	Note string `json:"note,omitempty"`
}

// Ack says the msg or state named by re is durably received (section
// 9.2). Acks drive outbox pruning and are not acked themselves.
type Ack struct {
	Envelope
}

// Chunk carries part of a blob (section 9.3). Chunks of one blob arrive in
// order; chunks of different blobs may interleave. A chunk carries th when
// the blob belongs to a thread, so that resume replays it like any other
// threaded message.
type Chunk struct {
	Envelope
	// Ref names the blob, as the sender chose it.
	Ref string `json:"ref"`
	// N is the chunk index, from 0.
	N int `json:"n" jsonschema:"minimum=0"`
	// Last is true on the final chunk.
	Last bool `json:"last"`
	// Data is the chunk's bytes, standard base64 (B64Pattern; the tag
	// syntax cannot carry the padding).
	Data string `json:"data"`
}

// Ping is a liveness probe (section 9.4): sent when idle for 30 seconds by
// convention; two missed pongs mean the connection is dead.
type Ping struct {
	Envelope
}

// Pong answers a ping; re names the ping.
type Pong struct {
	Envelope
}

// Bye is a graceful close (section 9.6). After sending it a peer sends
// nothing else and closes after the other side's bye or after 5 seconds.
type Bye struct {
	Envelope
	// Reason says why, for the log.
	Reason string `json:"reason,omitempty"`
}

// Err reports a problem (section 9.7). Whether it closes the connection
// depends on the code: bad_frame, version, auth and too_large do,
// unsupported, forbidden, blob_refused and internal do not.
type Err struct {
	Envelope
	// Code is the error code.
	Code string `json:"code"`
	// Detail is human readable.
	Detail string `json:"detail,omitempty"`
	// Ref names the blob a blob_refused is about.
	Ref string `json:"ref,omitempty"`
}

// GrantMsg delivers a grant after the handshake (section 10.3).
type GrantMsg struct {
	Envelope
	// Grant is the grant object (section 10.2).
	Grant json.RawMessage `json:"grant"`
}

// Introduce hands the recipient another peer's identity and address plus a
// grant issued by the introducer (section 10.4). The introduced peer honors
// the grant only if it trusts the introducer with introduce.
type Introduce struct {
	Envelope
	// Peer is the introduced peer.
	Peer IntroPeer `json:"peer"`
	// Grant is issued by the introducer to the recipient, for the
	// introduced peer to honor.
	Grant json.RawMessage `json:"grant,omitempty"`
}

// IntroPeer identifies the introduced peer.
type IntroPeer struct {
	// Key is the introduced peer's public key.
	Key string `json:"key" jsonschema:"pattern=^ed25519:[A-Za-z0-9_-]{43}$"`
	// Name is what the introduced peer calls itself.
	Name string `json:"name,omitempty"`
	// Address is where the introduced peer listens, a hint: the key is the
	// identity.
	Address string `json:"address,omitempty"`
}

// Encode marshals a message to one line, without the trailing newline and
// without HTML escaping, so that text reads naturally under cat.
func Encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	b = b[:len(b)-1]
	if len(b) > MaxLine {
		return nil, fmt.Errorf("%w: encoded line is %d bytes", ErrLineTooLong, len(b))
	}
	return b, nil
}

// ErrNotObject means a line is not a JSON object (bad_frame).
var ErrNotObject = errors.New("line is not a JSON object")

// ParseEnvelope decodes the envelope of a line. It fails with ErrNotObject
// when the line is not a JSON object, which the caller answers with
// bad_frame.
func ParseEnvelope(line []byte) (Envelope, error) {
	var env Envelope
	trimmed := bytes.TrimLeft(line, " \t\r")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return env, ErrNotObject
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return env, fmt.Errorf("%w: %v", ErrNotObject, err)
	}
	return env, nil
}

// Decode unmarshals a full line into v (one of the message structs).
func Decode(line []byte, v any) error {
	return json.Unmarshal(line, v)
}

// Now returns the current time as an RFC 3339 UTC timestamp with
// millisecond precision.
func Now() string {
	return FormatTime(time.Now())
}

// FormatTime formats t the way awp timestamps are written.
func FormatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// ParseTime accepts any RFC 3339 timestamp.
func ParseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}
