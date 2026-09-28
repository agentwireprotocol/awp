// Package transport provides the byte-stream bindings AWP runs over
// (SPEC.md section 4): tailcat, embedded as a library, plus plain TCP and
// Unix sockets for Fly 6PN, local use and tests.
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/logger"
)

// TailcatPort is the port awp listens on inside the tailcat tunnel. It
// is the port tailcat's own pipe mode uses, so `tailcat <addr>` connects a
// terminal straight to an awp peer: you can type NDJSON at it.
const TailcatPort = 1

// Kinds of address.
const (
	KindTailcat = "tailcat"
	KindTCP     = "tcp"
	KindUnix    = "unix"
)

// Addr is a parsed awp address.
type Addr struct {
	Kind   string
	Target string // tailcat address, host:port or socket path
}

// String returns the canonical form: "tailcat:tc...", "tcp:host:port" or
// "unix:/path".
func (a Addr) String() string { return a.Kind + ":" + a.Target }

// Parse accepts:
//
//	tc...                 a bare tailcat address, as `tailcat` prints it
//	tailcat:tc...
//	tcp:host:port         or tcp://host:port
//	unix:/path            or unix:///path
func Parse(s string) (Addr, error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "tailcat:"):
		t := strings.TrimPrefix(strings.TrimPrefix(s, "tailcat:"), "//")
		if !looksTailcat(t) {
			return Addr{}, fmt.Errorf("address %q: not a tailcat address", s)
		}
		return Addr{KindTailcat, t}, nil
	case looksTailcat(s):
		return Addr{KindTailcat, s}, nil
	case strings.HasPrefix(s, "tcp:"):
		t := strings.TrimPrefix(strings.TrimPrefix(s, "tcp:"), "//")
		if _, _, err := net.SplitHostPort(t); err != nil {
			return Addr{}, fmt.Errorf("address %q: %v", s, err)
		}
		return Addr{KindTCP, t}, nil
	case strings.HasPrefix(s, "unix:"):
		t := strings.TrimPrefix(s, "unix:")
		if strings.HasPrefix(t, "//") {
			t = strings.TrimPrefix(t, "//")
		}
		if t == "" {
			return Addr{}, fmt.Errorf("address %q: empty socket path", s)
		}
		return Addr{KindUnix, t}, nil
	}
	return Addr{}, fmt.Errorf("address %q: want tc..., tailcat:tc..., tcp:host:port or unix:/path", s)
}

func looksTailcat(s string) bool {
	if len(s) < 40 || !strings.HasPrefix(s, "tc") {
		return false
	}
	for _, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

// Transport dials awp addresses. It keeps one tailcat client per
// tailcat address, since each client is a whole userspace WireGuard node.
type Transport struct {
	// Logf receives tailcat's diagnostics. Nil discards them.
	Logf logger.Logf

	// AllowPublicTCP permits plain TCP to public addresses. By default
	// awp refuses, as section 4.2 asks: plain TCP has no encryption, so
	// it is only for loopback and private networks such as Fly's 6PN.
	AllowPublicTCP bool

	mu      sync.Mutex
	clients map[string]*tailcat.Client
}

// Dial opens a byte stream to a.
func (t *Transport) Dial(ctx context.Context, a Addr) (net.Conn, error) {
	switch a.Kind {
	case KindTCP:
		if !t.AllowPublicTCP {
			if err := checkPrivate(ctx, a.Target); err != nil {
				return nil, err
			}
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", a.Target)
	case KindUnix:
		var d net.Dialer
		return d.DialContext(ctx, "unix", a.Target)
	case KindTailcat:
		c := t.client(a.Target)
		conn, err := c.DialTCPPort(ctx, TailcatPort)
		if err != nil {
			return nil, fmt.Errorf("tailcat dial: %w", err)
		}
		return conn, nil
	}
	return nil, fmt.Errorf("unsupported address kind %q", a.Kind)
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10") // Tailscale and carrier-grade NAT

// checkPrivate refuses plain TCP to any address outside loopback, private
// (RFC 1918 and IPv6 ULA, which includes Fly's fdaa::/16 6PN), link-local
// and CGNAT ranges.
func checkPrivate(ctx context.Context, target string) error {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return err
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		ip = ip.Unmap()
		if !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)) {
			return fmt.Errorf("refusing plain TCP to public address %s: awp over TCP is unencrypted (use a tailcat address, or set AWP_ALLOW_PLAINTEXT=1)", ip)
		}
	}
	return nil
}

func (t *Transport) client(addr string) *tailcat.Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c, ok := t.clients[addr]; ok {
		return c
	}
	if t.clients == nil {
		t.clients = map[string]*tailcat.Client{}
	}
	c := &tailcat.Client{Server: tailcat.Addr(addr), Logf: t.logf()}
	t.clients[addr] = c
	return c
}

func (t *Transport) logf() logger.Logf {
	if t.Logf == nil {
		return logger.Discard
	}
	return t.Logf
}

// Reset drops the cached tailcat client for a, so the next dial starts from
// a fresh WireGuard node. Used after repeated failures, and when the peer at
// that address is no longer wanted.
func (t *Transport) Reset(a Addr) {
	if a.Kind != KindTailcat {
		return
	}
	t.mu.Lock()
	c := t.clients[a.Target]
	delete(t.clients, a.Target)
	t.mu.Unlock()
	if c != nil {
		go c.Close()
	}
}

// Close shuts down all tailcat clients.
func (t *Transport) Close() {
	t.mu.Lock()
	clients := t.clients
	t.clients = nil
	t.mu.Unlock()
	for _, c := range clients {
		c.Close()
	}
}

// Listen opens a listener for a TCP or Unix address. A stale Unix socket
// file is removed first.
func Listen(a Addr) (net.Listener, error) {
	switch a.Kind {
	case KindTCP:
		return net.Listen("tcp", a.Target)
	case KindUnix:
		if fi, err := os.Stat(a.Target); err == nil && fi.Mode()&os.ModeSocket != 0 {
			if c, err := net.DialTimeout("unix", a.Target, time.Second); err == nil {
				c.Close()
				return nil, fmt.Errorf("%s: already in use", a.Target)
			}
			os.Remove(a.Target)
		}
		return net.Listen("unix", a.Target)
	}
	return nil, fmt.Errorf("cannot listen on %s addresses this way", a.Kind)
}

// TailcatListener is an awp listener inside a tailcat tunnel.
type TailcatListener struct {
	net.Listener
	srv  *tailcat.Server
	addr string
}

// Address returns the listener's shareable tailcat address.
func (l *TailcatListener) Address() string { return l.addr }

// Close stops the listener and the tailcat server.
func (l *TailcatListener) Close() error {
	l.Listener.Close()
	return l.srv.Close()
}

// ListenTailcat starts a tailcat server whose key, pre-shared key and DERP
// region persist in keyFile, so the address stays the same across restarts
// (a sandbox waking from sleep comes back at the address its peers know).
// The first run picks the nearest region and writes the file.
func ListenTailcat(ctx context.Context, keyFile string, logf logger.Logf) (*TailcatListener, error) {
	if logf == nil {
		logf = logger.Discard
	}
	pk, err := loadOrCreateKey(keyFile)
	if err != nil {
		return nil, err
	}
	if pk.Public.RegionID <= 0 {
		// Pick the nearest DERP region once and remember it, like
		// `tailcat genkey --fixed-region`: the same key, pre-shared key and
		// region give the same address on every start. (Reading the region
		// back out of the address does not work: embedded regions are
		// renumbered.)
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
		if err := saveKey(keyFile, pk); err != nil {
			return nil, err
		}
	}
	srv := &tailcat.Server{
		Key:          pk.Private,
		PresharedKey: pk.Public.PresharedKey,
		RegionID:     pk.Public.RegionID,
		Logf:         logf,
	}
	ln, err := srv.Listen(ctx, "tcp", fmt.Sprintf(":%d", TailcatPort))
	if err != nil {
		srv.Close()
		if strings.Contains(err.Error(), "no such region") {
			// The DERP map dropped our region: forget it so the next
			// attempt picks a new one (the address changes; peers learn
			// it from hello).
			pk.Public.RegionID = 0
			saveKey(keyFile, pk)
		}
		return nil, err
	}
	return &TailcatListener{Listener: ln, srv: srv, addr: string(srv.TailcatAddr())}, nil
}

func loadOrCreateKey(path string) (*tailcat.PrivateKey, error) {
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
	if err := saveKey(path, pk); err != nil {
		return nil, err
	}
	return pk, nil
}

func saveKey(path string, pk *tailcat.PrivateKey) error {
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
