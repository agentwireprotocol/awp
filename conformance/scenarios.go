package conformance

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agentwireprotocol/awp/wire"
)

// Scenarios lists every scenario, in the order they run.
func Scenarios() []Scenario {
	return []Scenario{
		{Name: "handshake", Section: "10", Handshake: true,
			Doc: "hello without waiting, naming the tunnel's key, then resume",
			run: func(s *session) error { return s.handshake(true) }},
		{Name: "version", Section: "10.1", Solo: true,
			Doc: "a hello with an unknown v gets err version and the connection closes",
			run: func(s *session) error {
				if err := s.send(s.hello(99)); err != nil {
					return err
				}
				return expectErr(s, wire.ErrVersion, "after a hello with v 99")
			}},
		{Name: "hello-key", Section: "10.1", Solo: true,
			Doc: "a hello naming a key other than the tunnel's gets err auth and the connection closes",
			run: func(s *session) error {
				other, _, err := ed25519.GenerateKey(nil)
				if err != nil {
					return err
				}
				h := s.hello(wire.Version)
				h.Key = wire.FormatKey(other)
				if err := s.send(h); err != nil {
					return err
				}
				return expectErr(s, wire.ErrAuth, "after a hello whose key is not the tunnel's")
			}},
		{Name: "hello-own-key", Section: "10.1", Solo: true,
			Doc: "a hello naming the receiver's own key gets err auth and the connection closes",
			run: func(s *session) error {
				_, line, err := s.expect(wire.THello)
				if err != nil {
					return fmt.Errorf("waiting for hello: %w", err)
				}
				var theirs wire.Hello
				if err := wire.Decode(line, &theirs); err != nil {
					return err
				}
				h := s.hello(wire.Version)
				h.Key = theirs.Key
				if err := s.send(h); err != nil {
					return err
				}
				return expectErr(s, wire.ErrAuth, "after a hello carrying the peer's own key")
			}},
		{Name: "bad-frame", Section: "8", Solo: true,
			Doc: "a line that is not a JSON object gets err bad_frame and the connection closes",
			run: func(s *session) error {
				if err := s.handshake(false); err != nil {
					return err
				}
				if err := s.sendRaw([]byte("this is not json")); err != nil {
					return err
				}
				return expectErr(s, wire.ErrBadFrame, "after a line that is not JSON")
			}},
		{Name: "too-large", Section: "8", Solo: true,
			Doc: "a line over 1 MiB gets err too_large and the connection closes",
			run: func(s *session) error {
				if err := s.handshake(false); err != nil {
					return err
				}
				line := []byte(`{"t":"msg","id":"big","ts":"` + wire.Now() + `","th":"t","parts":[],"pad":"` + strings.Repeat("x", wire.MaxLine) + `"}`)
				if err := s.sendRaw(line); err != nil {
					// The peer may close before reading it all; that still counts.
					if !strings.Contains(err.Error(), "broken pipe") && !strings.Contains(err.Error(), "reset") {
						return err
					}
				}
				return expectErr(s, wire.ErrTooLarge, "after a line over 1 MiB")
			}},
		{Name: "ping", Section: "12.4",
			Doc: "a ping is answered by a pong whose re is the ping's id",
			run: func(s *session) error {
				ping := wire.Ping{Envelope: s.env(wire.TPing)}
				if err := s.send(ping); err != nil {
					return err
				}
				env, _, err := s.expect(wire.TPong)
				if err != nil {
					return err
				}
				s.check(env.Re == ping.ID, "pong.re is the ping's id (got %q)", env.Re)
				return nil
			}},
		{Name: "msg-ack", Section: "12.1, 12.2",
			Doc: "a msg with text, code and data parts is acked, with the msg's id and thread",
			run: func(s *session) error {
				m := wire.Msg{Envelope: s.env(wire.TMsg), Subject: "conformance", Parts: []wire.Part{
					{K: wire.PartText, Text: "Hello from the conformance runner."},
					{K: wire.PartCode, Lang: "go", Text: "package main"},
					{K: wire.PartData, Mime: "application/json", Data: mustJSON(map[string]any{"runner": "awp conform"})},
				}}
				m.Th = "conform-" + m.ID
				return expectAck(s, m.Envelope, m)
			}},
		{Name: "state-ack", Section: "11.1, 12.2",
			Doc: "a state is acked like a msg",
			run: func(s *session) error {
				m := wire.Msg{Envelope: s.env(wire.TMsg), Subject: "conformance state", Parts: text("state follows")}
				m.Th = "conform-" + m.ID
				if err := expectAck(s, m.Envelope, m); err != nil {
					return err
				}
				st := wire.State{Envelope: s.env(wire.TState), State: wire.StateWorking, Note: "checking"}
				st.Th = m.Th
				return expectAck(s, st.Envelope, st)
			}},
		{Name: "dedup", Section: "12.5",
			Doc: "a msg delivered twice with the same id is acked both times",
			run: func(s *session) error {
				m := wire.Msg{Envelope: s.env(wire.TMsg), Subject: "conformance dedup", Parts: text("once")}
				m.Th = "conform-" + m.ID
				if err := expectAck(s, m.Envelope, m); err != nil {
					return err
				}
				if err := s.send(m); err != nil {
					return err
				}
				env, _, err := s.expect(wire.TAck)
				if err != nil {
					s.check(false, "the replayed msg is acked again (%v)", err)
					return nil
				}
				s.check(env.Re == m.ID, "the replayed msg is acked again (ack.re %q)", env.Re)
				return nil
			}},
		{Name: "unknown-type", Section: "8",
			Doc: "a message of an unknown type is ignored, or answered with a non-closing err unsupported",
			run: func(s *session) error {
				line := []byte(`{"t":"conformance-future","id":"` + s.r.ids.New() + `","ts":"` + wire.Now() + `","payload":[1,2,3]}`)
				if err := s.sendRaw(line); err != nil {
					return err
				}
				ping := wire.Ping{Envelope: s.env(wire.TPing)}
				if err := s.send(ping); err != nil {
					return err
				}
				env, line, err := s.expect(wire.TPong, wire.TErr)
				if err != nil {
					s.check(false, "connection survives an unknown message type (%v)", err)
					return nil
				}
				if env.T == wire.TErr {
					var e wire.Err
					wire.Decode(line, &e)
					if !s.check(e.Code == wire.ErrUnsupported, "an err for an unknown type has code unsupported (got %s)", e.Code) {
						return nil
					}
					if env, _, err = s.expect(wire.TPong); err != nil {
						s.check(false, "connection survives err unsupported (%v)", err)
						return nil
					}
				}
				s.check(env.Re == ping.ID, "connection survives an unknown message type: pong answers the ping")
				return nil
			}},
		{Name: "unknown-field", Section: "8",
			Doc: "unknown fields in a msg and its parts are ignored; the msg is acked",
			run: func(s *session) error {
				id, th := s.r.ids.New(), "conform-fields"
				line := []byte(`{"t":"msg","id":"` + id + `","ts":"` + wire.Now() + `","th":"` + th + `","priority":"high","subject":"conformance fields",` +
					`"parts":[{"k":"text","text":"with extras","mood":"sunny"}],"x-runner":{"nested":true}}`)
				if err := s.sendRaw(line); err != nil {
					return err
				}
				return expectAck(s, wire.Envelope{T: wire.TMsg, ID: id, Th: th})
			}},
		{Name: "err-nonclosing", Section: "12.7",
			Doc: "an err with a non-closing code (unsupported) leaves the connection open",
			run: func(s *session) error {
				if err := s.send(wire.Err{Envelope: s.env(wire.TErr), Code: wire.ErrUnsupported, Detail: "conformance: a non-closing err"}); err != nil {
					return err
				}
				return s.alive("err unsupported")
			}},
		{Name: "chunk", Section: "12.3", Needs: "blob",
			Doc: "a msg with a blob part, followed by the blob in two chunks, is acked and refused only with blob_refused",
			run: func(s *session) error {
				data := []byte("hello, conformance runner\n")
				m := wire.Msg{Envelope: s.env(wire.TMsg), Subject: "conformance blob", Parts: []wire.Part{
					{K: wire.PartText, Text: "a small file follows"},
					{K: wire.PartBlob, Ref: "conform-blob", Name: "hello.txt", Mime: "text/plain", Size: int64(len(data))},
				}}
				m.Th = "conform-" + m.ID
				if err := expectAck(s, m.Envelope, m); err != nil {
					return err
				}
				half := len(data) / 2
				for i, piece := range [][]byte{data[:half], data[half:]} {
					c := wire.Chunk{Envelope: s.env(wire.TChunk), Ref: "conform-blob", N: i, Last: i == 1, Data: base64.StdEncoding.EncodeToString(piece)}
					c.Th = m.Th
					if err := s.send(c); err != nil {
						return err
					}
				}
				err := s.quiet(time.Second, "the chunks")
				var pe peerErr
				if errors.As(err, &pe) && pe.e.Code == wire.ErrBlobRefused {
					s.note("peer refused the blob with blob_refused, which it may")
					err = nil
				}
				if err != nil {
					s.check(false, "chunks are accepted (%v)", err)
					return nil
				}
				return s.alive("the chunks")
			}},
		{Name: "grant", Section: "13.3", Needs: "grant",
			Doc: "a grant message carrying a grant to the peer is accepted without an err",
			run: func(s *session) error {
				g, err := wire.MintGrant(s.r.priv, s.r.peer.Key, []string{"fs:read"}, time.Now().Add(time.Hour), "")
				if err != nil {
					return err
				}
				if err := s.send(wire.GrantMsg{Envelope: s.env(wire.TGrant), Grant: g.Raw}); err != nil {
					return err
				}
				if err := s.quiet(time.Second, "the grant"); err != nil {
					s.check(false, "the grant is accepted (%v)", err)
					return nil
				}
				return s.alive("the grant")
			}},
		{Name: "bye", Section: "12.6",
			Doc: "a bye is answered with a bye and the connection closes",
			run: func(s *session) error {
				if err := s.send(wire.Bye{Envelope: s.env(wire.TBye), Reason: "conformance done"}); err != nil {
					return err
				}
				_, _, err := s.expect(wire.TBye)
				if !s.check(err == nil, "peer answered bye with bye (%v)", err) {
					if errors.Is(err, errClosed) {
						s.note("peer closed without a bye of its own")
					}
					return nil
				}
				s.established = false
				return s.expectClosed("after bye")
			}},
	}
}

// expectErr waits for an err with the code and, since every such code
// closes the connection, for the close.
func expectErr(s *session, code, after string) error {
	env, line, err := s.expect(wire.TErr)
	if err != nil {
		if errors.Is(err, errClosed) {
			s.check(false, "peer sent err %s %s (it closed without one)", code, after)
			return nil
		}
		return err
	}
	var e wire.Err
	wire.Decode(line, &e)
	_ = env
	if !s.check(e.Code == code, "peer sent err %s %s (got %s: %s)", code, after, e.Code, e.Detail) {
		return nil
	}
	s.established = false
	return s.expectClosed("after err " + code)
}

// expectAck sends the message (if given) and waits for its ack, checking
// re and th.
func expectAck(s *session, sent wire.Envelope, send ...any) error {
	for _, m := range send {
		if err := s.send(m); err != nil {
			return err
		}
	}
	env, _, err := s.expect(wire.TAck)
	if err != nil {
		s.check(false, "%s %s is acked (%v)", sent.T, sent.ID, err)
		return nil
	}
	s.check(env.Re == sent.ID, "ack.re is the %s's id (got %q)", sent.T, env.Re)
	s.check(env.Th == sent.Th, "ack.th is the %s's thread (got %q)", sent.T, env.Th)
	return nil
}
