// Package control is the local API between the awp daemon and its
// clients (the CLI, the MCP server, harness hooks): newline-delimited JSON
// over a Unix socket that only the owning user can open.
//
// A request is one line, {"method": "...", "params": {...}}. A plain call
// gets one reply line, {"result": ...} or {"error": "..."}. A streaming call
// gets {"event": ...} lines until either side closes.
package control

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SocketName is the control socket's file name inside the awp home.
const SocketName = "awp.sock"

// socketPointer is the file in the awp home that names the socket when
// it lives elsewhere, so a client finds it whatever its environment.
const socketPointer = "awp.sock.path"

// maxSocketPath keeps socket paths under the kernel's sun_path limit (108
// bytes on Linux, 104 on macOS).
const maxSocketPath = 100

// SocketPath is where the daemon for home listens. Unix socket paths are
// limited in length, so a home that is too deep (common in sandboxes) gets
// its socket in the user's private runtime directory instead, named by a
// hash of the home path. Daemon and clients compute the same path.
func SocketPath(home string) (string, error) {
	p := filepath.Join(home, SocketName)
	if len(p) <= maxSocketPath {
		return p, nil
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(abs))
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" || len(dir) > maxSocketPath-40 {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("awp-%d", os.Getuid()))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		// Refuse a directory another user created or opened up: its owner
		// could replace our socket.
		if err := checkPrivateDir(dir); err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, "awp-"+hex.EncodeToString(sum[:8])+".sock"), nil
}

// Request is one call.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Reply is a call's answer, or one event of a stream.
type Reply struct {
	Result json.RawMessage `json:"result,omitempty"`
	Event  json.RawMessage `json:"event,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Handler serves calls. Stream methods write events with emit until ctx is
// done or they return.
type Handler interface {
	Call(ctx context.Context, method string, params json.RawMessage) (any, error)
	Stream(ctx context.Context, method string, params json.RawMessage, emit func(any) error) (bool, error)
}

// Serve accepts control connections on the socket in home until ctx ends.
func Serve(ctx context.Context, home string, h Handler) error {
	path, err := SocketPath(home)
	if err != nil {
		return err
	}
	if c, err := (&Client{Home: home}).dial(); err == nil {
		c.Close()
		return fmt.Errorf("%s: a daemon is already running", home)
	}
	pointer := filepath.Join(home, socketPointer)
	if filepath.Dir(path) == filepath.Clean(home) {
		os.Remove(pointer)
	} else if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	} else if err := os.WriteFile(pointer, []byte(path+"\n"), 0o600); err != nil {
		return err
	}
	os.Remove(path)
	old := umask(0o077)
	ln, err := net.Listen("unix", path)
	umask(old)
	if err != nil {
		return err
	}
	os.Chmod(path, 0o600)
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go serveOne(ctx, c, h)
	}
}

func serveOne(ctx context.Context, c net.Conn, h Handler) {
	defer c.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	if !sc.Scan() {
		return
	}
	var req Request
	enc := json.NewEncoder(c)
	enc.SetEscapeHTML(false)
	if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
		enc.Encode(Reply{Error: "bad request: " + err.Error()})
		return
	}
	// A stream ends when the client hangs up.
	go func() {
		buf := make([]byte, 1)
		for {
			if _, err := c.Read(buf); err != nil {
				cancel()
				return
			}
		}
	}()
	emit := func(ev any) error {
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		return enc.Encode(Reply{Event: b})
	}
	if ok, err := h.Stream(ctx, req.Method, req.Params, emit); ok {
		if err != nil && ctx.Err() == nil {
			enc.Encode(Reply{Error: err.Error()})
		}
		return
	}
	res, err := h.Call(ctx, req.Method, req.Params)
	if err != nil {
		enc.Encode(Reply{Error: err.Error()})
		return
	}
	b, err := json.Marshal(res)
	if err != nil {
		enc.Encode(Reply{Error: err.Error()})
		return
	}
	enc.Encode(Reply{Result: b})
}

// ErrNoDaemon means nothing is listening on the control socket.
var ErrNoDaemon = errors.New("awp daemon is not running")

// Client calls a daemon.
type Client struct {
	Home string
}

func (c *Client) dial() (net.Conn, error) {
	path, err := SocketPath(c.Home)
	// A daemon started with another environment (XDG_RUNTIME_DIR, TMPDIR)
	// may have put its socket elsewhere; it says where.
	if b, perr := os.ReadFile(filepath.Join(c.Home, socketPointer)); perr == nil {
		if p := strings.TrimSpace(string(b)); p != "" {
			path, err = p, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrNoDaemon, err)
	}
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrNoDaemon, err)
	}
	return conn, nil
}

// Running reports whether a daemon answers on the socket.
func (c *Client) Running() bool {
	conn, err := c.dial()
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (c *Client) start(ctx context.Context, method string, params any) (net.Conn, *bufio.Scanner, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, nil, err
	}
	p, err := json.Marshal(params)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	req, _ := json.Marshal(Request{Method: method, Params: p})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		conn.Close()
		return nil, nil, err
	}
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	return conn, sc, nil
}

// Call makes one call and decodes the result into out (if non-nil).
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn, sc, err := c.start(ctx, method, params)
	if err != nil {
		return err
	}
	defer conn.Close()
	if !sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := sc.Err(); err != nil {
			return err
		}
		return errors.New("daemon closed the connection")
	}
	var r Reply
	if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
		return err
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	if out != nil && len(r.Result) > 0 {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// Stream makes a streaming call, passing each event to fn until fn returns
// an error, ctx ends or the daemon ends the stream.
func (c *Client) Stream(ctx context.Context, method string, params any, fn func(json.RawMessage) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn, sc, err := c.start(ctx, method, params)
	if err != nil {
		return err
	}
	defer conn.Close()
	for sc.Scan() {
		var r Reply
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return err
		}
		if r.Error != "" {
			return errors.New(r.Error)
		}
		if err := fn(r.Event); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("daemon ended the stream")
}
