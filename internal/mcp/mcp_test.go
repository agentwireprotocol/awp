package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentwireprotocol/awp/internal/api"
	"github.com/agentwireprotocol/awp/internal/control"
	"github.com/agentwireprotocol/awp/internal/daemon"
	"github.com/agentwireprotocol/awp/wire"
)

// startDaemon runs a daemon with its own home, listening on a Unix socket.
func startDaemon(t *testing.T, name string) (*control.Client, string) {
	t.Helper()
	home, err := os.MkdirTemp("", "awpm") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	sock := "unix:" + filepath.Join(home, "p.sock")
	cfg := daemon.Config{Home: home, Name: name, Listen: []string{sock}}
	errc := make(chan error, 1)
	go func() { errc <- daemon.Run(cfg) }()
	c := &control.Client{Home: home}
	deadline := time.Now().Add(10 * time.Second)
	for !c.Running() {
		if time.Now().After(deadline) {
			t.Fatalf("daemon %s did not start: %v", name, <-errc)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() {
		c.Call(context.Background(), "shutdown", nil, nil)
		<-errc
	})
	var st api.Status
	if err := c.Call(context.Background(), "status", nil, &st); err != nil || st.Address == "" {
		t.Fatalf("status: %v %+v", err, st)
	}
	return c, st.Address
}

// session is an MCP client talking to a Server over pipes.
type session struct {
	t     *testing.T
	w     io.Writer
	lines chan map[string]any
	id    int
}

func newSession(t *testing.T, c *control.Client, channel bool) *session {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &Server{Ensure: func() (*control.Client, error) { return c, nil }, Channel: channel, Logf: t.Logf}
	go s.Run(context.Background(), inR, outW)
	t.Cleanup(func() { inW.Close() })
	sess := &session{t: t, w: inW, lines: make(chan map[string]any, 100)}
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(nil, 16<<20)
		for sc.Scan() {
			var m map[string]any
			json.Unmarshal(sc.Bytes(), &m)
			sess.lines <- m
		}
	}()
	return sess
}

func (s *session) request(method string, params any) map[string]any {
	s.t.Helper()
	s.id++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": s.id, "method": method, "params": params})
	s.w.Write(append(b, '\n'))
	deadline := time.After(20 * time.Second)
	for {
		select {
		case m := <-s.lines:
			if id, ok := m["id"].(float64); ok && int(id) == s.id {
				if m["error"] != nil {
					s.t.Fatalf("%s: error %v", method, m["error"])
				}
				return m["result"].(map[string]any)
			}
			s.lines <- m // a notification; keep it for later
			time.Sleep(10 * time.Millisecond)
		case <-deadline:
			s.t.Fatalf("%s: no reply", method)
		}
	}
}

func (s *session) tool(name string, args any) string {
	s.t.Helper()
	res := s.request("tools/call", map[string]any{"name": name, "arguments": args})
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if res["isError"] == true {
		s.t.Fatalf("%s failed: %s", name, text)
	}
	return text
}

func TestToolsEndToEnd(t *testing.T) {
	a, _ := startDaemon(t, "alice")
	b, bAddr := startDaemon(t, "bob")
	s := newSession(t, a, false)

	init := s.request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test"}})
	if init["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocol version %v", init["protocolVersion"])
	}
	caps := init["capabilities"].(map[string]any)
	if _, ok := caps["experimental"].(map[string]any)["claude/channel"]; !ok {
		t.Fatalf("capabilities %v", caps)
	}
	tools := s.request("tools/list", nil)["tools"].([]any)
	var names []string
	for _, tl := range tools {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	want := "awp_listen awp_connect awp_send awp_read awp_state awp_grant awp_status"
	if strings.Join(names, " ") != want {
		t.Fatalf("tools %v", names)
	}

	if out := s.tool("awp_listen", map[string]any{}); !strings.Contains(out, "You are alice") {
		t.Fatalf("listen: %s", out)
	}
	if out := s.tool("awp_connect", map[string]any{"address": bAddr}); !strings.Contains(out, "Connected to bob") {
		t.Fatalf("connect: %s", out)
	}
	out := s.tool("awp_send", map[string]any{"peer": "bob", "subject": "Port the auth middleware", "text": "Can you run the suite?", "data": map[string]any{"branch": "kyle/refactor"}, "wait_ack_seconds": 5})
	if !strings.Contains(out, "acknowledged") {
		t.Fatalf("send: %s", out)
	}
	th := strings.Fields(strings.SplitN(out, "thread ", 2)[1])[0]

	// Bob reads it and answers with a state change and a reply.
	var got api.ReadResult
	if err := b.Call(context.Background(), "wait", api.WaitParams{TimeoutMS: 5000}, &got); err != nil || len(got.Events) != 1 {
		t.Fatalf("bob wait: %v %+v", err, got)
	}
	b.Call(context.Background(), "state", api.StateParams{Th: th, State: "working", Note: "on it"}, nil)
	b.Call(context.Background(), "send", api.SendParams{Th: th, Parts: []wire.Part{{K: wire.PartText, Text: "3 of 42 failing"}}}, nil)

	out = s.tool("awp_read", map[string]any{"thread": th, "wait_seconds": 5})
	if !strings.Contains(out, "untrusted") || !(strings.Contains(out, "3 of 42 failing") || strings.Contains(out, "state working")) {
		t.Fatalf("read: %s", out)
	}
	s.tool("awp_state", map[string]any{"thread": th, "state": "waiting", "note": "need logs"})
	status := s.tool("awp_status", map[string]any{})
	if !strings.Contains(status, "bob") || !strings.Contains(status, th) {
		t.Fatalf("status: %s", status)
	}
	if out := s.tool("awp_grant", map[string]any{"peer": "bob", "caps": []string{"fs:read"}, "ttl": "10m"}); !strings.Contains(out, "Granted [fs:read]") {
		t.Fatalf("grant: %s", out)
	}

	// Errors come back as tool errors, not protocol errors.
	res := s.request("tools/call", map[string]any{"name": "awp_send", "arguments": map[string]any{"peer": "nobody", "text": "x"}})
	if res["isError"] != true {
		t.Fatalf("expected a tool error: %v", res)
	}
}

func TestChannelPush(t *testing.T) {
	a, _ := startDaemon(t, "alice")
	b, bAddr := startDaemon(t, "bob")
	s := newSession(t, a, false)
	// A client that registers for channel events gets pushes.
	s.request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"experimental": map[string]any{"claude/channel": map[string]any{}}}})
	s.tool("awp_connect", map[string]any{"address": bAddr})
	var peers []api.PeerView
	b.Call(context.Background(), "peers", nil, &peers)
	if len(peers) != 1 {
		t.Fatalf("bob peers %+v", peers)
	}
	b.Call(context.Background(), "send", api.SendParams{Peer: peers[0].Key, Subject: "Heads up", Parts: []wire.Part{{K: wire.PartText, Text: "deploy is frozen"}}}, nil)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m := <-s.lines:
			if m["method"] != "notifications/claude/channel" {
				continue
			}
			p := m["params"].(map[string]any)
			meta := p["meta"].(map[string]any)
			if !strings.Contains(p["content"].(string), "deploy is frozen") || meta["peer"] != "bob" || meta["type"] != "msg" || !strings.HasPrefix(meta["thread"].(string), "thr_") {
				t.Fatalf("push %v", p)
			}
			// Pushed messages count as read.
			time.Sleep(200 * time.Millisecond)
			var res api.ReadResult
			a.Call(context.Background(), "read", api.ReadParams{Inbox: true, Unread: true}, &res)
			for _, ev := range res.Events {
				if ev.Type == "msg" {
					t.Fatalf("pushed message still unread")
				}
			}
			return
		case <-deadline:
			t.Fatal("no channel notification")
		}
	}
}
