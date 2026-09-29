package schema

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agentwireprotocol/awp/wire"
)

// TestSpecExamples validates every JSON example in SPEC.md: a message
// against its definition, a grant against Grant, a presence document
// against Presence. A block is one object, or one object per line.
func TestSpecExamples(t *testing.T) {
	src, err := os.ReadFile("../SPEC.md")
	if err != nil {
		t.Fatal(err)
	}
	if m := specVersion.FindSubmatch(src); m != nil && !strings.Contains(ID, "/v"+string(m[1])+"/") {
		t.Skipf("SPEC.md is protocol v%s; %s tracks an earlier draft until the wire package follows", m[1], ID)
	}
	v, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	blocks := jsonBlocks(string(src))
	if len(blocks) < 10 {
		t.Fatalf("found only %d json blocks in SPEC.md", len(blocks))
	}
	checked := 0
	for _, b := range blocks {
		docs := [][]byte{[]byte(b.text)}
		if !json.Valid(docs[0]) {
			docs = nil
			for _, l := range strings.Split(b.text, "\n") {
				if strings.TrimSpace(l) != "" {
					docs = append(docs, []byte(l))
				}
			}
		}
		for _, doc := range docs {
			var obj map[string]any
			if err := json.Unmarshal(doc, &obj); err != nil {
				t.Errorf("SPEC.md:%d: not a JSON object: %v", b.line, err)
				continue
			}
			checked++
			var err error
			switch {
			case obj["t"] != nil:
				err = v.Line(doc)
			case obj["iss"] != nil:
				err = v.Object("Grant", doc)
			case obj["origin"] != nil:
				err = v.Object("Presence", doc)
			default:
				err = errors.New("cannot tell what this example is")
			}
			if err != nil {
				t.Errorf("SPEC.md:%d: %v\n%s", b.line, err, doc)
			}
		}
	}
	t.Logf("checked %d examples", checked)
}

type block struct {
	line int
	text string
}

var fence = regexp.MustCompile("(?ms)^```json\n(.*?)^```")

// specVersion finds the protocol version in the spec's first hello example.
var specVersion = regexp.MustCompile(`"t":"hello"[^\n]*?"v":(\d+)`)

func jsonBlocks(src string) []block {
	var out []block
	for _, m := range fence.FindAllStringSubmatchIndex(src, -1) {
		out = append(out, block{line: strings.Count(src[:m[0]], "\n") + 2, text: src[m[2]:m[3]]})
	}
	return out
}

// TestGoMessages validates one of every message the wire package encodes,
// so that the schema accepts what the reference implementation sends.
func TestGoMessages(t *testing.T) {
	v, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	key := wire.FormatKey(pub)
	other, _, _ := ed25519.GenerateKey(nil)
	otherKey := wire.FormatKey(other)
	ids := wire.NewIDGen()
	env := func(t string, th, re string) wire.Envelope {
		return wire.Envelope{T: t, ID: ids.New(), TS: wire.Now(), Th: th, Re: re}
	}
	grant, err := wire.MintGrant(priv, otherKey, []string{"exec"}, time.Now().Add(time.Hour), key)
	if err != nil {
		t.Fatal(err)
	}
	presence, err := wire.SignPresence(priv, &wire.Presence{
		Name: "go@test", Seq: 3, TS: wire.Now(), Active: wire.Now(), Harness: "claude",
		Peers:   []wire.PresencePeer{{Key: otherKey, Name: "x", Up: true, RTT: 12}},
		Threads: []wire.PresenceThread{{Th: "t1", Peer: otherKey, Mine: "working", Updated: wire.Now(), Unread: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := wire.Msg{Envelope: env("msg", "t1", ""), Subject: "s", Parts: []wire.Part{
		{K: wire.PartText, Text: "hi"},
		{K: wire.PartCode, Lang: "go", Text: "package x"},
		{K: wire.PartData, Mime: "application/json", Data: json.RawMessage(`{"a":1}`)},
		{K: wire.PartBlob, Ref: "b1", Name: "f.bin", Mime: "application/octet-stream", Size: 0},
		{K: wire.PartData, Data: nil}, // encodes as data: null
	}}
	msgLine, _ := wire.Encode(msg)
	messages := map[string]any{
		wire.THello: wire.Hello{Envelope: env("hello", "", ""), V: wire.Version, Key: key, Name: "go@test", Nonce: wire.Nonce(32),
			Caps: []string{"chat", "blob"}, About: "test", Addr: "unix:/tmp/x", Shares: []string{otherKey}},
		wire.TAuth:      wire.Auth{Envelope: env("auth", "", ""), Sig: wire.SignAuth(priv, []byte("a"), []byte("b")), Grants: []json.RawMessage{grant.Raw}},
		wire.TResume:    wire.Resume{Envelope: env("resume", "", ""), Seen: map[string]string{"t1": ids.New()}},
		wire.TMsg:       msg,
		wire.TState:     wire.State{Envelope: env("state", "t1", ""), State: "working", Note: "n"},
		wire.TAck:       wire.Ack{Envelope: env("ack", "t1", msg.ID)},
		wire.TChunk:     wire.Chunk{Envelope: env("chunk", "t1", ""), Ref: "b1", N: 0, Last: true, Data: "aGk="},
		wire.TPing:      wire.Ping{Envelope: env("ping", "", "")},
		wire.TPong:      wire.Pong{Envelope: env("pong", "", ids.New())},
		wire.TBye:       wire.Bye{Envelope: env("bye", "", ""), Reason: "done"},
		wire.TErr:       wire.Err{Envelope: env("err", "", ""), Code: wire.ErrBlobRefused, Detail: "too big", Ref: "b1"},
		wire.TGrant:     wire.GrantMsg{Envelope: env("grant", "", ""), Grant: grant.Raw},
		wire.TIntroduce: wire.Introduce{Envelope: env("introduce", "t1", ""), Peer: wire.IntroPeer{Key: otherKey, Name: "o", Address: "tailcat:tc0"}, Grant: grant.Raw},
		wire.TPresence:  wire.PresenceMsg{Envelope: env("presence", "", ""), Doc: presence, Hops: 1},
		wire.TMirror:    wire.Mirror{Envelope: env("mirror", "t1", ""), Of: otherKey, OfName: "o", Subject: "s", Dir: "out", Line: msgLine},
		wire.TPrivate:   wire.Private{Envelope: env("private", "t1", "")},
	}
	for _, m := range wire.Messages() {
		v, ok := messages[m.T]
		if !ok {
			t.Errorf("no example for %s", m.T)
		}
		_ = v
	}
	for typ, m := range messages {
		line, err := wire.Encode(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.Line(line); err != nil {
			t.Errorf("%s: %v\n%s", typ, err, line)
		}
	}
	// A withdrawal is a mirror without a line.
	line, _ := wire.Encode(wire.Mirror{Envelope: env("mirror", "t1", ""), Of: otherKey, Withdraw: true})
	if err := v.Line(line); err != nil {
		t.Errorf("mirror withdraw: %v", err)
	}
	if err := v.Object("Grant", grant.Raw); err != nil {
		t.Error(err)
	}
	if err := v.Object("Presence", presence); err != nil {
		t.Error(err)
	}
}

func TestRejects(t *testing.T) {
	v, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	ts := wire.Now()
	for name, line := range map[string]string{
		"not json":           `hello`,
		"not an object":      `[1,2]`,
		"missing id":         fmt.Sprintf(`{"t":"ping","ts":"%s"}`, ts),
		"empty id":           fmt.Sprintf(`{"t":"ping","id":"","ts":"%s"}`, ts),
		"bad ts":             `{"t":"ping","id":"x","ts":"yesterday"}`,
		"msg without th":     fmt.Sprintf(`{"t":"msg","id":"x","ts":"%s","parts":[]}`, ts),
		"ack without re":     fmt.Sprintf(`{"t":"ack","id":"x","ts":"%s","th":"t"}`, ts),
		"unknown part":       fmt.Sprintf(`{"t":"msg","id":"x","ts":"%s","th":"t","parts":[{"k":"hologram"}]}`, ts),
		"text part empty":    fmt.Sprintf(`{"t":"msg","id":"x","ts":"%s","th":"t","parts":[{"k":"text"}]}`, ts),
		"blob no size":       fmt.Sprintf(`{"t":"msg","id":"x","ts":"%s","th":"t","parts":[{"k":"blob","ref":"r"}]}`, ts),
		"hello bad key":      fmt.Sprintf(`{"t":"hello","id":"x","ts":"%s","v":0,"key":"rsa:abc","nonce":"abc"}`, ts),
		"hello v string":     fmt.Sprintf(`{"t":"hello","id":"x","ts":"%s","v":"0","key":"ed25519:%s","nonce":"abc"}`, ts, strings.Repeat("A", 43)),
		"err bad code":       fmt.Sprintf(`{"t":"err","id":"x","ts":"%s","code":"oops"}`, ts),
		"chunk bad data":     fmt.Sprintf(`{"t":"chunk","id":"x","ts":"%s","ref":"r","n":0,"last":true,"data":"not base64!"}`, ts),
		"chunk n negative":   fmt.Sprintf(`{"t":"chunk","id":"x","ts":"%s","ref":"r","n":-1,"last":true,"data":"aGk="}`, ts),
		"grant no sig":       fmt.Sprintf(`{"t":"grant","id":"x","ts":"%s","grant":{"iss":"ed25519:%s","sub":"ed25519:%s","caps":[],"exp":"%s","nonce":"n"}}`, ts, strings.Repeat("A", 43), strings.Repeat("B", 43), ts),
		"unknown type no id": fmt.Sprintf(`{"t":"future","ts":"%s"}`, ts),
	} {
		if err := v.Line([]byte(line)); err == nil {
			t.Errorf("%s: accepted %s", name, line)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}
	// Unknown types pass the envelope check and are reported as unknown.
	err = v.Line([]byte(fmt.Sprintf(`{"t":"future","id":"x","ts":"%s","whatever":1}`, ts)))
	if !errors.Is(err, ErrUnknownType) {
		t.Errorf("unknown type: %v", err)
	}
	// Unknown fields are fine.
	if err := v.Line([]byte(fmt.Sprintf(`{"t":"ping","id":"x","ts":"%s","priority":"high"}`, ts))); err != nil {
		t.Errorf("unknown field: %v", err)
	}
}
