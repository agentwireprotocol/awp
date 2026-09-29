package tunnel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/tailscale/tailcat"
	"tailscale.com/types/logger"
)

// --- unix (section 7.4) ---

func newUnix(b *bind) *mux {
	return newMux(b, KindUnix, func(ctx context.Context, to string) (pconn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, "unix", to)
		if err != nil {
			return nil, err
		}
		return newLenConn(c), nil
	})
}

// listenUnix listens on a socket path, removing a stale socket first.
func listenUnix(m *mux, p string) ([]string, error) {
	if fi, err := os.Stat(p); err == nil && fi.Mode()&os.ModeSocket != 0 {
		if c, err := net.DialTimeout("unix", p, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s: already in use", p)
		}
		os.Remove(p)
	}
	ln, err := net.Listen("unix", p)
	if err != nil {
		return nil, err
	}
	m.serveListener(ln, func(c net.Conn) pconn { return newLenConn(c) })
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	return []string{abs}, nil
}

// --- tailcat (section 7.2) ---

// tailcatPort is the port inside the tailcat tunnel that carries WireGuard
// datagrams, length-prefixed like the unix carrier.
const tailcatPort = 1

type tailcatCarrier struct {
	*mux
	logf logger.Logf

	mu       sync.Mutex
	clients  map[string]*tailcat.Client
	failures map[string]int
	servers  []*tailcat.Server
}

func newTailcat(b *bind, logf logger.Logf) *tailcatCarrier {
	if logf == nil {
		logf = logger.Discard
	}
	t := &tailcatCarrier{logf: logf, clients: map[string]*tailcat.Client{}, failures: map[string]int{}}
	t.mux = newMux(b, KindTailcat, func(ctx context.Context, to string) (pconn, error) {
		c, err := t.client(to).DialTCPPort(ctx, tailcatPort)
		if err != nil {
			return nil, fmt.Errorf("tailcat dial: %w", err)
		}
		t.mu.Lock()
		delete(t.failures, to)
		t.mu.Unlock()
		return newLenConn(c), nil
	})
	t.mux.failed = func(to string, err error) {
		t.mu.Lock()
		t.failures[to]++
		reset := t.failures[to]%3 == 0
		t.mu.Unlock()
		if reset {
			t.reset(to) // the next dial starts from a fresh tailcat node
		}
	}
	return t
}

func (t *tailcatCarrier) client(addr string) *tailcat.Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.clients[addr]
	if c == nil {
		c = &tailcat.Client{Server: tailcat.Addr(addr), Logf: t.logf}
		t.clients[addr] = c
	}
	return c
}

func (t *tailcatCarrier) reset(addr string) {
	t.mu.Lock()
	c := t.clients[addr]
	delete(t.clients, addr)
	t.mu.Unlock()
	if c != nil {
		go c.Close()
	}
}

// listen starts a tailcat server whose key, pre-shared key and DERP region
// persist in keyFile (empty: a fresh one each time), so the endpoint
// survives restarts.
func (t *tailcatCarrier) listen(ctx context.Context, keyFile string) ([]string, error) {
	pk, err := loadOrCreateTailcatKey(keyFile)
	if err != nil {
		return nil, err
	}
	if pk.Public.RegionID <= 0 {
		// Pick the nearest DERP region once and remember it, like
		// `tailcat genkey --fixed-region`.
		dm, err := tailcat.FetchDERPMap(ctx, tailcat.ExpandForServer)
		if err != nil {
			return nil, fmt.Errorf("fetching DERP map: %w", err)
		}
		id, err := tailcat.PickBestRegion(ctx, dm)
		if err != nil {
			return nil, fmt.Errorf("picking DERP region: %w", err)
		}
		if id == 0 {
			return nil, errors.New("no reachable DERP region")
		}
		pk.Public.RegionID = id
		if err := saveTailcatKey(keyFile, pk); err != nil {
			return nil, err
		}
	}
	srv := &tailcat.Server{Key: pk.Private, PresharedKey: pk.Public.PresharedKey, RegionID: pk.Public.RegionID, Logf: t.logf}
	ln, err := srv.Listen(ctx, "tcp", fmt.Sprintf(":%d", tailcatPort))
	if err != nil {
		srv.Close()
		if strings.Contains(err.Error(), "no such region") {
			// The DERP map dropped our region: pick a new one next time.
			pk.Public.RegionID = 0
			saveTailcatKey(keyFile, pk)
		}
		return nil, err
	}
	t.mu.Lock()
	t.servers = append(t.servers, srv)
	t.mu.Unlock()
	t.serveListener(ln, func(c net.Conn) pconn { return newLenConn(c) })
	return []string{string(srv.TailcatAddr())}, nil
}

func (t *tailcatCarrier) close() error {
	t.mux.close()
	t.mu.Lock()
	cs, ss := t.clients, t.servers
	t.clients, t.servers = map[string]*tailcat.Client{}, nil
	t.mu.Unlock()
	for _, c := range cs {
		c.Close()
	}
	for _, s := range ss {
		s.Close()
	}
	return nil
}

func loadOrCreateTailcatKey(path string) (*tailcat.PrivateKey, error) {
	if path == "" {
		return tailcat.NewPrivateKey(), nil
	}
	b, err := os.ReadFile(path)
	if err == nil {
		var pk tailcat.PrivateKey
		if err := json.Unmarshal(b, &pk); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if pk.Private.IsZero() || pk.Public.PresharedKey.IsZero() {
			return nil, fmt.Errorf("%s: incomplete tailcat key", path)
		}
		return &pk, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	pk := tailcat.NewPrivateKey()
	return pk, saveTailcatKey(path, pk)
}

func saveTailcatKey(path string, pk *tailcat.PrivateKey) error {
	if path == "" {
		return nil
	}
	b, err := json.MarshalIndent(pk, "", "\t")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// --- ws (section 7.3) ---

// WSSubprotocol is the WebSocket subprotocol of the ws carrier.
const WSSubprotocol = "awp.wg.1"

type wsConn struct{ c *websocket.Conn }

func (w wsConn) readPacket() ([]byte, error) {
	for {
		typ, p, err := w.c.Read(context.Background())
		if err != nil {
			return nil, err
		}
		if typ == websocket.MessageBinary {
			return p, nil
		}
	}
}

func (w wsConn) writePacket(p []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return w.c.Write(ctx, websocket.MessageBinary, p)
}

func (w wsConn) Close() error { return w.c.Close(websocket.StatusNormalClosure, "") }

type wsCarrier struct {
	*mux
	mu    sync.Mutex
	procs []*exec.Cmd
}

func newWS(b *bind) *wsCarrier {
	return &wsCarrier{mux: newMux(b, KindWS, func(ctx context.Context, to string) (pconn, error) {
		c, _, err := websocket.Dial(ctx, to, &websocket.DialOptions{Subprotocols: []string{WSSubprotocol}})
		if err != nil {
			return nil, err
		}
		c.SetReadLimit(1 << 16)
		return wsConn{c}, nil
	})}
}

// listen serves WebSocket upgrades on hostport, at any path. The endpoint
// is public if given (the URL a tunnel provider forwards to hostport),
// otherwise ws://hostport/awp.
func (w *wsCarrier) listen(hostport, public string) ([]string, error) {
	ln, err := net.Listen("tcp", hostport)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(rw, r, &websocket.AcceptOptions{
			Subprotocols: []string{WSSubprotocol},
			// Origin checks protect cookies; the payload is WireGuard and
			// carries its own authentication, and browsers are welcome.
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		if c.Subprotocol() != WSSubprotocol {
			c.Close(websocket.StatusPolicyViolation, "want subprotocol "+WSSubprotocol)
			return
		}
		c.SetReadLimit(1 << 16)
		cn := &closeNotify{wsConn: wsConn{c}, done: make(chan struct{})}
		w.accept(cn)
		<-cn.done
	}), ReadHeaderTimeout: 10 * time.Second}
	w.addCloser(srv)
	go srv.Serve(ln)
	if public == "" {
		public = "ws://" + ln.Addr().String() + "/awp"
	}
	return []string{public}, nil
}

// closeNotify keeps an HTTP handler alive for as long as its WebSocket is.
type closeNotify struct {
	wsConn
	once sync.Once
	done chan struct{}
}

func (c *closeNotify) Close() error {
	err := c.wsConn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}

var quickTunnelURL = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// listenCloudflare serves WebSockets on a loopback port and exposes it
// through a Cloudflare quick tunnel: `cloudflared tunnel --url`, no account.
// The endpoint is the wss:// URL it prints.
func (w *wsCarrier) listenCloudflare(ctx context.Context, cloudflared string) ([]string, error) {
	if cloudflared == "" {
		cloudflared = "cloudflared"
	}
	bin, err := exec.LookPath(cloudflared)
	if err != nil {
		return nil, fmt.Errorf("cloudflare carrier needs cloudflared on PATH: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	local := ln.Addr().String()
	ln.Close()
	if _, err := w.listen(local, "unused"); err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, "tunnel", "--no-autoupdate", "--url", "http://"+local)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.procs = append(w.procs, cmd)
	w.mu.Unlock()
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if u := quickTunnelURL.FindString(sc.Text()); u != "" {
				select {
				case found <- u:
				default:
				}
			}
		}
	}()
	go cmd.Wait()
	select {
	case u := <-found:
		return []string{"wss://" + strings.TrimPrefix(u, "https://") + "/awp"}, nil
	case <-ctx.Done():
		cmd.Process.Kill()
		return nil, fmt.Errorf("cloudflared printed no quick tunnel URL: %w", ctx.Err())
	}
}

func (w *wsCarrier) close() error {
	w.mux.close()
	w.mu.Lock()
	ps := w.procs
	w.procs = nil
	w.mu.Unlock()
	for _, p := range ps {
		if p.Process != nil {
			p.Process.Kill()
		}
	}
	return nil
}
