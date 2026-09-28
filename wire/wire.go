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
	"time"
)

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

// Recommended thread states from section 8.1.
const (
	StateOpen    = "open"
	StateWorking = "working"
	StateWaiting = "waiting"
	StateDone    = "done"
	StateFailed  = "failed"
	StateClosed  = "closed"
)

// Envelope holds the fields every line carries.
type Envelope struct {
	T  string `json:"t"`
	ID string `json:"id"`
	TS string `json:"ts"`
	Th string `json:"th,omitempty"`
	Re string `json:"re,omitempty"`
}

// Hello is the first line each side sends (section 7.1).
type Hello struct {
	Envelope
	V     int      `json:"v"`
	Key   string   `json:"key"`
	Name  string   `json:"name,omitempty"`
	Nonce string   `json:"nonce"`
	Caps  []string `json:"caps,omitempty"`
	About string   `json:"about,omitempty"`

	// Addr is an extension: an address at which the sender can be reached,
	// so that either side can reconnect (section 4.3). Peers that do not
	// know it ignore it, as section 5 requires.
	Addr string `json:"addr,omitempty"`

	// Shares is an extension (mirror.go): the keys of the hosts this agent
	// mirrors its conversations to, so the other party knows.
	Shares []string `json:"shares,omitempty"`
}

// Auth proves key possession (section 7.2).
type Auth struct {
	Envelope
	Sig    string            `json:"sig"`
	Grants []json.RawMessage `json:"grants,omitempty"`
}

// Resume lists, per thread, the last id the sender has durably received.
type Resume struct {
	Envelope
	Seen map[string]string `json:"seen"`
}

// Msg is one turn in a thread (section 9.1).
type Msg struct {
	Envelope
	Subject string `json:"subject,omitempty"`
	Parts   []Part `json:"parts"`
}

// Part is one piece of a message. Which fields are meaningful depends on K.
type Part struct {
	K    string          `json:"k"`
	Text string          `json:"text,omitempty"`
	Lang string          `json:"lang,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
	Mime string          `json:"mime,omitempty"`
	Ref  string          `json:"ref,omitempty"`
	Name string          `json:"name,omitempty"`
	Size int64           `json:"size,omitempty"`
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

// State is a thread's soft state as seen by the sender (section 8.1).
type State struct {
	Envelope
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
}

// Ack says the message named by Re is durably received (section 9.2).
type Ack struct {
	Envelope
}

// Chunk carries part of a blob (section 9.3). Chunks carry th when the blob
// belongs to a thread, which lets resume replay them like any other threaded
// message.
type Chunk struct {
	Envelope
	Ref  string `json:"ref"`
	N    int    `json:"n"`
	Last bool   `json:"last"`
	Data string `json:"data"`
}

// Ping and Pong are liveness probes (section 9.4).
type Ping struct {
	Envelope
}

// Pong answers a Ping; Re names the ping.
type Pong struct {
	Envelope
}

// Bye is a graceful close (section 9.6).
type Bye struct {
	Envelope
	Reason string `json:"reason,omitempty"`
}

// Err reports a problem (section 9.7). Ref names a blob for blob_refused.
type Err struct {
	Envelope
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
	Ref    string `json:"ref,omitempty"`
}

// GrantMsg delivers a grant after the handshake (section 10.3).
type GrantMsg struct {
	Envelope
	Grant json.RawMessage `json:"grant"`
}

// Introduce hands the recipient another peer's identity and address plus a
// grant issued by the introducer (section 10.4).
type Introduce struct {
	Envelope
	Peer  IntroPeer       `json:"peer"`
	Grant json.RawMessage `json:"grant,omitempty"`
}

// IntroPeer identifies the introduced peer.
type IntroPeer struct {
	Key     string `json:"key"`
	Name    string `json:"name,omitempty"`
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
