// Package conformance checks a peer against SPEC.md. The runner is a
// scripted peer with a key of its own: it connects to the peer under test
// (or listens for it), drives each scenario over the wire, checks every
// line it receives against the JSON Schema, and reports what the peer did.
//
// It is what `awp conform` runs. The reference implementation passes it in
// its own tests; scripts/conformance.sh runs it against the Python peer.
package conformance

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/agentwireprotocol/awp/transport"
	"github.com/agentwireprotocol/awp/schema"
	"github.com/agentwireprotocol/awp/wire"
)

// Options say which peer to check and how.
type Options struct {
	// Addr is the peer's address. The runner dials it once per scenario.
	Addr string
	// Listen is an address (tcp:host:port or unix:/path) to listen on
	// instead: the peer dials the runner, and the scenarios that fit one
	// connection run in turn on it. Solo scenarios are skipped.
	Listen string
	// Run is a shell command to start once the runner listens, with
	// {addr} replaced by the listening address; it is killed at the end.
	Run string
	// Only names the scenarios to run; nil runs all of them.
	Only []string
	// Timeout bounds each wait for the peer; 15 seconds by default.
	Timeout time.Duration
	// Name is the runner's hello name; "awp-conform" by default.
	Name string
	// Trace receives every line sent (dir "out") and received ("in").
	Trace func(dir string, line []byte)
	// Logf receives progress; nil discards it.
	Logf func(format string, args ...any)
}

// Check is one thing a scenario looked at.
type Check struct {
	OK   bool   `json:"ok"`
	What string `json:"what"`
}

// Result is the outcome of one scenario.
type Result struct {
	Name    string  `json:"name"`
	Section string  `json:"section"`
	Doc     string  `json:"doc"`
	Status  string  `json:"status"` // pass, fail or skip
	Reason  string  `json:"reason,omitempty"`
	Checks  []Check `json:"checks,omitempty"`
	Lines   int     `json:"lines"` // lines received from the peer
}

// Peer is what the peer said about itself in hello.
type Peer struct {
	Key   string   `json:"key"`
	Name  string   `json:"name,omitempty"`
	About string   `json:"about,omitempty"`
	V     int      `json:"v"`
	Caps  []string `json:"caps,omitempty"`
}

// Report is the outcome of a run.
type Report struct {
	Mode     string        `json:"mode"` // connect or listen
	Addr     string        `json:"addr"`
	Peer     *Peer         `json:"peer,omitempty"`
	Results  []Result      `json:"results"`
	Passed   int           `json:"passed"`
	Failed   int           `json:"failed"`
	Skipped  int           `json:"skipped"`
	Duration time.Duration `json:"duration"`
}

// Scenario is one scripted exchange with the peer.
type Scenario struct {
	Name    string
	Section string // of SPEC.md
	Doc     string
	// Solo scenarios break the handshake or the connection, so they need
	// a connection of their own; in listen mode they are skipped. The
	// others run after a completed handshake.
	Solo bool
	// Handshake marks the scenario that drives the handshake itself: it
	// runs first on a connection.
	Handshake bool
	// Needs is a hello cap the peer must list, or the scenario is skipped.
	Needs string
	run   func(s *session) error
}

// Run checks the peer and returns the report. The error is about the run
// itself (a bad address, no connection); scenario failures are in the
// report.
func Run(ctx context.Context, o Options) (*Report, error) {
	if o.Timeout <= 0 {
		o.Timeout = 15 * time.Second
	}
	if o.Name == "" {
		o.Name = "awp-conform"
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	v, err := schema.Default()
	if err != nil {
		return nil, err
	}
	scenarios := Scenarios()
	if len(o.Only) > 0 {
		var chosen []Scenario
		for _, name := range o.Only {
			i := slices.IndexFunc(scenarios, func(s Scenario) bool { return s.Name == name })
			if i < 0 {
				return nil, fmt.Errorf("no scenario %q (awp conform --list)", name)
			}
			chosen = append(chosen, scenarios[i])
		}
		scenarios = chosen
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	r := &runner{o: o, v: v, priv: priv, key: wire.FormatKey(pub), ids: wire.NewIDGen()}
	started := time.Now()
	rep := &Report{Addr: o.Addr, Mode: "connect"}
	if o.Listen != "" {
		rep.Mode, rep.Addr = "listen", o.Listen
		err = r.listen(ctx, scenarios, rep)
	} else {
		if o.Addr == "" {
			return nil, errors.New("no address to connect to")
		}
		err = r.connect(ctx, scenarios, rep)
	}
	rep.Peer = r.peer
	for _, res := range rep.Results {
		switch res.Status {
		case "pass":
			rep.Passed++
		case "fail":
			rep.Failed++
		default:
			rep.Skipped++
		}
	}
	rep.Duration = time.Since(started)
	return rep, err
}

type runner struct {
	o    Options
	v    *schema.Validator
	priv ed25519.PrivateKey
	key  string
	ids  *wire.IDGen
	peer *Peer
}

// connect runs every scenario on a connection of its own.
func (r *runner) connect(ctx context.Context, scenarios []Scenario, rep *Report) error {
	addr, err := transport.Parse(r.o.Addr)
	if err != nil {
		return err
	}
	t := &transport.Transport{AllowPublicTCP: true}
	defer t.Close()
	for _, sc := range scenarios {
		r.o.Logf("%s: connecting", sc.Name)
		dctx, cancel := context.WithTimeout(ctx, r.o.Timeout)
		conn, err := t.Dial(dctx, addr)
		cancel()
		if err != nil {
			if len(rep.Results) == 0 {
				return fmt.Errorf("connect to %s: %w", r.o.Addr, err)
			}
			rep.Results = append(rep.Results, Result{Name: sc.Name, Section: sc.Section, Doc: sc.Doc, Status: "fail", Reason: "connect: " + err.Error()})
			continue
		}
		s := r.session(conn)
		rep.Results = append(rep.Results, r.runOne(s, sc))
		conn.Close()
	}
	return nil
}

// listen accepts one connection from the peer and runs the scenarios that
// share a connection on it, in order.
func (r *runner) listen(ctx context.Context, scenarios []Scenario, rep *Report) error {
	addr, err := transport.Parse(r.o.Listen)
	if err != nil {
		return err
	}
	ln, err := transport.Listen(addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	rep.Addr = addr.Kind + ":" + ln.Addr().String()
	r.o.Logf("listening on %s", rep.Addr)
	if r.o.Run != "" {
		cmd := exec.CommandContext(ctx, "sh", "-c", strings.ReplaceAll(r.o.Run, "{addr}", rep.Addr))
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		// Its own process group, so that the peer goes with the shell
		// that started it.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("run %q: %w", r.o.Run, err)
		}
		defer func() {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			cmd.Wait()
		}()
	}
	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, err := ln.Accept()
		ch <- accepted{c, err}
	}()
	var conn net.Conn
	select {
	case a := <-ch:
		if a.err != nil {
			return a.err
		}
		conn = a.conn
	case <-time.After(4 * r.o.Timeout):
		return fmt.Errorf("no peer connected to %s within %s", rep.Addr, 4*r.o.Timeout)
	case <-ctx.Done():
		return ctx.Err()
	}
	defer conn.Close()
	s := r.session(conn)
	for _, sc := range scenarios {
		switch {
		case sc.Solo:
			rep.Results = append(rep.Results, Result{Name: sc.Name, Section: sc.Section, Doc: sc.Doc, Status: "skip",
				Reason: "needs a connection of its own: run awp conform <address> against the listening peer"})
			continue
		case sc.Handshake && s.established:
			rep.Results = append(rep.Results, Result{Name: sc.Name, Section: sc.Section, Doc: sc.Doc, Status: "skip",
				Reason: "runs first on a connection: list it first, or leave --scenario out"})
			continue
		}
		rep.Results = append(rep.Results, r.runOne(s, sc))
	}
	return nil
}

func (r *runner) session(conn net.Conn) *session {
	return &session{r: r, conn: conn, rd: wire.NewReader(conn), wr: wire.NewWriter(conn), timeout: r.o.Timeout, trace: r.o.Trace}
}

// runOne runs a scenario on a session. Unless the scenario drives the
// handshake itself or breaks it, a handshake is done first as a prelude,
// not a check, when the session has none yet.
func (r *runner) runOne(s *session, sc Scenario) Result {
	s.checks, s.lines = nil, 0
	res := Result{Name: sc.Name, Section: sc.Section, Doc: sc.Doc}
	if !sc.Solo && !sc.Handshake && !s.established {
		if err := s.handshake(false); err != nil {
			res.Status, res.Reason = "fail", "handshake before the scenario: "+err.Error()
			res.Checks, res.Lines = s.checks, s.lines
			return res
		}
		s.checks = nil
	}
	if sc.Needs != "" && r.peer != nil && !slices.Contains(r.peer.Caps, sc.Needs) {
		res.Status, res.Reason = "skip", "peer does not list "+sc.Needs+" in hello.caps"
		return res
	}
	r.o.Logf("%s: running", sc.Name)
	err := sc.run(s)
	res.Checks, res.Lines = s.checks, s.lines
	switch {
	case err != nil:
		res.Status, res.Reason = "fail", err.Error()
	default:
		res.Status = "pass"
		for _, c := range s.checks {
			if !c.OK {
				res.Status, res.Reason = "fail", c.What
				break
			}
		}
	}
	return res
}

// session is one connection to the peer, as the scenarios see it.
type session struct {
	r       *runner
	conn    net.Conn
	rd      *wire.Reader
	wr      *wire.Writer
	timeout time.Duration
	trace   func(dir string, line []byte)

	established        bool
	myHello, peerHello []byte
	checks             []Check
	lines              int
}

func (s *session) env(t string) wire.Envelope {
	return wire.Envelope{T: t, ID: s.r.ids.New(), TS: wire.Now()}
}

// check records an observation; it returns ok.
func (s *session) check(ok bool, format string, args ...any) bool {
	s.checks = append(s.checks, Check{OK: ok, What: fmt.Sprintf(format, args...)})
	return ok
}

// note records something that is neither a pass nor a failure.
func (s *session) note(format string, args ...any) {
	s.checks = append(s.checks, Check{OK: true, What: "note: " + fmt.Sprintf(format, args...)})
}

func (s *session) send(v any) error {
	line, err := wire.Encode(v)
	if err != nil {
		return err
	}
	return s.sendRaw(line)
}

// sendRaw writes a line as is, however long.
func (s *session) sendRaw(line []byte) error {
	if s.trace != nil {
		s.trace("out", line)
	}
	s.conn.SetWriteDeadline(time.Now().Add(s.timeout))
	_, err := s.conn.Write(append(slices.Clone(line), '\n'))
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// errClosed means the peer closed the connection.
var errClosed = errors.New("peer closed the connection")

// read returns the next line, checked against the schema. It returns
// errClosed at EOF.
func (s *session) read(d time.Duration) (wire.Envelope, []byte, error) {
	s.conn.SetReadDeadline(time.Now().Add(d))
	line, err := s.rd.ReadLine()
	if err != nil {
		switch {
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed):
			return wire.Envelope{}, nil, errClosed
		case errors.Is(err, os.ErrDeadlineExceeded):
			return wire.Envelope{}, nil, fmt.Errorf("nothing from the peer for %s", d)
		case errors.Is(err, wire.ErrLineTooLong):
			s.check(false, "peer sent a line over 1 MiB")
			return wire.Envelope{}, nil, err
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return wire.Envelope{}, nil, fmt.Errorf("nothing from the peer for %s", d)
		}
		// A reset after the peer closed counts as closed.
		if errors.Is(err, io.ErrClosedPipe) || strings.Contains(err.Error(), "reset by peer") {
			return wire.Envelope{}, nil, errClosed
		}
		return wire.Envelope{}, nil, fmt.Errorf("read: %w", err)
	}
	s.lines++
	if s.trace != nil {
		s.trace("in", line)
	}
	env, perr := wire.ParseEnvelope(line)
	if verr := s.r.v.Line(line); verr != nil {
		if errors.Is(verr, schema.ErrUnknownType) {
			s.note("peer sent a line of unknown type %q, ignored", env.T)
		} else {
			s.check(false, "schema: %v", verr)
		}
	}
	if perr != nil {
		return env, line, fmt.Errorf("peer sent a line that is not a JSON object: %.80q", line)
	}
	return env, line, nil
}

// peerErr is an err line the peer sent when a scenario expected something
// else.
type peerErr struct{ e wire.Err }

func (e peerErr) Error() string {
	if e.e.Detail != "" {
		return fmt.Sprintf("peer sent err %s: %s", e.e.Code, e.e.Detail)
	}
	return "peer sent err " + e.e.Code
}

// expect reads until a line of one of the types arrives, within the
// scenario's timeout. Pings are answered, other lines skipped. An err line
// is returned as a peerErr unless err is among the types.
func (s *session) expect(types ...string) (wire.Envelope, []byte, error) {
	deadline := time.Now().Add(s.timeout)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return wire.Envelope{}, nil, fmt.Errorf("no %s from the peer within %s", strings.Join(types, " or "), s.timeout)
		}
		env, line, err := s.read(left)
		if err != nil {
			return env, line, err
		}
		if slices.Contains(types, env.T) {
			return env, line, nil
		}
		switch env.T {
		case wire.TPing:
			s.send(wire.Pong{Envelope: wire.Envelope{T: wire.TPong, ID: s.r.ids.New(), TS: wire.Now(), Re: env.ID}})
		case wire.TErr:
			var e wire.Err
			wire.Decode(line, &e)
			return env, line, peerErr{e}
		}
	}
}

// expectClosed waits for the peer to close the connection. Lines that
// arrive first are recorded as failed checks, except bye.
func (s *session) expectClosed(what string) error {
	deadline := time.Now().Add(s.timeout)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("connection still open %s after %s", what, s.timeout)
		}
		env, _, err := s.read(left)
		if errors.Is(err, errClosed) {
			s.check(true, "peer closed the connection %s", what)
			return nil
		}
		if err != nil {
			return err
		}
		if env.T != wire.TBye {
			s.check(false, "peer sent %s %s instead of closing", env.T, what)
		}
	}
}

// helloCaps are what the runner lists.
var helloCaps = []string{"chat", "blob", "grant", "introduce"}

func (s *session) hello(v int) wire.Hello {
	return wire.Hello{Envelope: s.env(wire.THello), V: v, Key: s.r.key, Name: s.r.o.Name, Nonce: wire.Nonce(32),
		Caps: helloCaps, About: "conformance runner"}
}

// handshake does section 7. With waitFirst the runner holds its own hello
// until the peer's arrives and checks that the peer did not wait.
func (s *session) handshake(waitFirst bool) error {
	if !waitFirst {
		my, err := wire.Encode(s.hello(wire.Version))
		if err != nil {
			return err
		}
		s.myHello = my
		if err := s.sendRaw(my); err != nil {
			return err
		}
	}
	env, line, err := s.read(s.timeout)
	if err != nil {
		return fmt.Errorf("waiting for hello: %w", err)
	}
	if !s.check(env.T == wire.THello, "the peer's first line is hello (got %q)", env.T) {
		return errors.New("first line was not hello")
	}
	if waitFirst {
		s.check(true, "peer sent hello without waiting for ours")
		my, err := wire.Encode(s.hello(wire.Version))
		if err != nil {
			return err
		}
		s.myHello = my
		if err := s.sendRaw(my); err != nil {
			return err
		}
	}
	s.peerHello = line
	var h wire.Hello
	if err := wire.Decode(line, &h); err != nil {
		return fmt.Errorf("malformed hello: %v", err)
	}
	s.check(h.V == wire.Version, "hello.v is %d (got %d)", wire.Version, h.V)
	pub, err := wire.ParseKey(h.Key)
	if !s.check(err == nil, "hello.key is an ed25519 key (%v)", err) {
		return errors.New("bad hello key")
	}
	s.check(h.Nonce != "", "hello carries a nonce")
	if s.r.peer == nil {
		s.r.peer = &Peer{Key: h.Key, Name: h.Name, About: h.About, V: h.V, Caps: h.Caps}
	}

	if err := s.send(wire.Auth{Envelope: s.env(wire.TAuth), Sig: wire.SignAuth(s.r.priv, s.myHello, s.peerHello)}); err != nil {
		return err
	}
	_, line, err = s.expect(wire.TAuth)
	if err != nil {
		return fmt.Errorf("waiting for auth: %w", err)
	}
	var a wire.Auth
	if err := wire.Decode(line, &a); err != nil {
		return fmt.Errorf("malformed auth: %v", err)
	}
	err = wire.VerifyAuth(pub, a.Sig, s.peerHello, s.myHello)
	if !s.check(err == nil, "auth.sig verifies with hello.key over both hello lines (%v)", err) {
		return errors.New("auth signature does not verify")
	}
	for i, g := range a.Grants {
		if _, err := wire.ParseGrant(g, time.Time{}); err != nil {
			s.check(false, "auth.grants[%d] verifies (%v)", i, err)
		}
	}

	env, line, err = s.expect(wire.TResume)
	if err != nil {
		return fmt.Errorf("waiting for resume: %w", err)
	}
	var res wire.Resume
	if err := wire.Decode(line, &res); err != nil {
		return fmt.Errorf("malformed resume: %v", err)
	}
	s.check(true, "peer sent resume after auth (%d threads seen)", len(res.Seen))
	if err := s.send(wire.Resume{Envelope: s.env(wire.TResume), Seen: map[string]string{}}); err != nil {
		return err
	}
	s.established = true
	return nil
}

// alive checks the connection still works: a ping gets a pong.
func (s *session) alive(after string) error {
	ping := wire.Ping{Envelope: s.env(wire.TPing)}
	if err := s.send(ping); err != nil {
		return err
	}
	env, _, err := s.expect(wire.TPong)
	if err != nil {
		s.check(false, "connection still works after %s (%v)", after, err)
		return err
	}
	s.check(env.Re == ping.ID, "connection still works after %s: pong answers the ping", after)
	return nil
}

// quiet checks that the peer sends no err for a while (pings answered).
func (s *session) quiet(d time.Duration, after string) error {
	deadline := time.Now().Add(d)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			s.check(true, "no err from the peer after %s", after)
			return nil
		}
		env, line, err := s.read(left)
		if err != nil {
			if strings.HasPrefix(err.Error(), "nothing from the peer") {
				s.check(true, "no err from the peer after %s", after)
				return nil
			}
			return err
		}
		switch env.T {
		case wire.TPing:
			s.send(wire.Pong{Envelope: wire.Envelope{T: wire.TPong, ID: s.r.ids.New(), TS: wire.Now(), Re: env.ID}})
		case wire.TErr:
			var e wire.Err
			wire.Decode(line, &e)
			return peerErr{e}
		}
	}
}

func text(t string) []wire.Part { return []wire.Part{{K: wire.PartText, Text: t}} }

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
