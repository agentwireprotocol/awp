package tunnel

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/wireguard-go/device"
	"tailscale.com/types/logger"
)

// Config configures a Tunnel.
type Config struct {
	// Identity is the peer's Ed25519 key; its X25519 form is the WireGuard key.
	Identity ed25519.PrivateKey

	// StateDir holds tunnel.json (pre-shared keys) and tailcat.json (the
	// tailcat carrier's key). Empty keeps everything in memory.
	StateDir string

	// Listen lists carriers to accept on:
	//
	//	tailcat                 tailcat, the default for anything behind NAT
	//	udp:HOST:PORT           plain UDP; an empty or unspecified host advertises every local address
	//	unix:/path              a local socket
	//	ws:HOST:PORT            WebSockets, advertised as ws://HOST:PORT/awp
	//	ws:HOST:PORT=URL        WebSockets behind a tunnel or proxy, advertised as URL
	//	cloudflare              WebSockets through a Cloudflare quick tunnel (needs cloudflared)
	Listen []string

	// Admit decides whether an unknown key may start a handshake. Nil admits
	// every key that holds the listener's pre-shared key (section 5.2).
	Admit func(Key) bool

	// Changed is called when the address changes (a carrier came up).
	Changed func()

	Logf        func(format string, args ...any) // the tunnel's own log
	TailcatLogf logger.Logf                      // tailcat's diagnostics
	Cloudflared string                           // the cloudflared binary; default "cloudflared" on PATH
	Verbose     bool                             // WireGuard's debug log
}

// Tunnel is one peer's WireGuard device, its carriers, and the TCP stack
// inside.
type Tunnel struct {
	cfg  Config
	pub  Key
	ip   netip.Addr
	st   *state
	dev  *device.Device
	tun  *netTun
	bind *bind
	ln   net.Listener

	udp     *udpCarrier
	unix    *mux
	tailcat *tailcatCarrier
	ws      *wsCarrier

	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	endpoints map[int][]Endpoint // by index in cfg.Listen
	errs      map[string]string  // listen spec -> last error
	keys      map[netip.Addr]Key
	admitted  map[Key][32]byte // psk each lookup-admitted peer was given
	closed    bool
}

// New creates the device and the stack. Start begins listening.
func New(cfg Config) (*Tunnel, error) {
	if len(cfg.Identity) != ed25519.PrivateKeySize {
		return nil, errors.New("tunnel: no identity")
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	st, err := loadState(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	pub, err := X25519Public(cfg.Identity.Public().(ed25519.PublicKey))
	if err != nil {
		return nil, err
	}
	t := &Tunnel{cfg: cfg, pub: pub, ip: IP(pub), st: st, endpoints: map[int][]Endpoint{}, errs: map[string]string{}, keys: map[netip.Addr]Key{}, admitted: map[Key][32]byte{}}
	t.ctx, t.cancel = context.WithCancel(context.Background())
	t.bind = newBind(t)
	t.udp = newUDP(t.bind)
	t.unix = newUnix(t.bind)
	t.tailcat = newTailcat(t.bind, cfg.TailcatLogf)
	t.ws = newWS(t.bind)

	t.tun, err = newNetTun(t.ip, MTU)
	if err != nil {
		return nil, err
	}
	lg := device.NewLogger(device.LogLevelSilent, "")
	if cfg.Verbose {
		lg = &device.Logger{Verbosef: func(f string, a ...any) { cfg.Logf("wg: "+f, a...) }, Errorf: func(f string, a ...any) { cfg.Logf("wg: "+f, a...) }}
	}
	t.dev = device.NewDevice(t.tun, t.bind, lg)
	if err := t.dev.SetPrivateKey(device.NoisePrivateKey(X25519Private(cfg.Identity))); err != nil {
		t.dev.Close()
		return nil, err
	}
	t.dev.SetPeerLookupFunc(t.lookup)
	if err := t.dev.Up(); err != nil {
		t.dev.Close()
		return nil, err
	}
	t.ln, err = t.tun.listenTCP(netip.AddrPortFrom(t.ip, Port))
	if err != nil {
		t.dev.Close()
		return nil, err
	}
	return t, nil
}

// Key is this tunnel's WireGuard public key.
func (t *Tunnel) Key() Key { return t.pub }

func (t *Tunnel) carrier(kind string) carrier {
	switch kind {
	case KindUDP:
		return t.udp
	case KindUnix:
		return t.unix
	case KindTailcat:
		return t.tailcat
	case KindWS:
		return t.ws
	}
	return nil
}

// lookup admits a handshake from a key WireGuard does not know yet.
func (t *Tunnel) lookup(pk device.NoisePublicKey) (*device.NewPeerConfig, bool) {
	k := Key(pk)
	psk, known := t.st.pair(k)
	if !known {
		if !t.listening() {
			return nil, false // only peers we have met may reach a dialer
		}
		if t.cfg.Admit != nil && !t.cfg.Admit(k) {
			return nil, false
		}
		psk = t.st.listenerPSK()
	}
	t.noteKey(k)
	t.mu.Lock()
	t.admitted[k] = psk
	t.mu.Unlock()
	return &device.NewPeerConfig{
		AllowedIPs:   []netip.Prefix{netip.PrefixFrom(IP(k), 128)},
		PresharedKey: device.NoisePresharedKey(psk),
		Endpoint:     t.bind.peer(k),
	}, true
}

func (t *Tunnel) noteKey(k Key) {
	t.mu.Lock()
	t.keys[IP(k)] = k
	t.mu.Unlock()
}

func (t *Tunnel) listening() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, es := range t.endpoints {
		if len(es) > 0 {
			return true
		}
	}
	return false
}

// Start opens every carrier in cfg.Listen. tailcat and cloudflare come up in
// the background, since they take seconds; Changed is called when they do.
// Start fails only if a local carrier (udp, unix, ws) cannot listen.
func (t *Tunnel) Start() error {
	for i, spec := range t.cfg.Listen {
		kind, arg, _ := strings.Cut(spec, ":")
		switch kind {
		case KindTailcat, "cloudflare":
			go t.listenRetry(i, spec)
			continue
		}
		es, err := t.listenOne(t.ctx, spec)
		if err != nil {
			return fmt.Errorf("listen %s: %w", spec, err)
		}
		_ = arg
		t.setEndpoints(i, es)
	}
	return nil
}

func (t *Tunnel) listenOne(ctx context.Context, spec string) ([]Endpoint, error) {
	kind, arg, _ := strings.Cut(spec, ":")
	var vals []string
	var err error
	switch kind {
	case KindUDP:
		vals, err = t.udp.listen(arg)
	case KindUnix:
		if arg == "" {
			return nil, errors.New("unix: empty path")
		}
		vals, err = listenUnix(t.unix, arg)
	case KindWS:
		hostport, public, _ := strings.Cut(arg, "=")
		vals, err = t.ws.listen(hostport, public)
		kind = KindWS
	case KindTailcat:
		keyFile := ""
		if t.cfg.StateDir != "" {
			keyFile = filepath.Join(t.cfg.StateDir, "tailcat.json")
		}
		vals, err = t.tailcat.listen(ctx, keyFile)
	case "cloudflare":
		vals, err = t.ws.listenCloudflare(ctx, t.cfg.Cloudflared)
		kind = KindWS
	default:
		return nil, fmt.Errorf("unknown carrier %q (want tailcat, udp:HOST:PORT, unix:PATH, ws:HOST:PORT or cloudflare)", kind)
	}
	if err != nil {
		return nil, err
	}
	var out []Endpoint
	for _, v := range vals {
		out = append(out, Endpoint{Kind: kind, Value: v})
	}
	return out, nil
}

func (t *Tunnel) listenRetry(i int, spec string) {
	backoff := 5 * time.Second
	for {
		ctx, cancel := context.WithTimeout(t.ctx, 60*time.Second)
		es, err := t.listenOne(ctx, spec)
		cancel()
		if err == nil {
			t.setEndpoints(i, es)
			return
		}
		t.mu.Lock()
		t.errs[spec] = err.Error()
		t.mu.Unlock()
		t.cfg.Logf("%s listener: %v (retrying in %v)", spec, err, backoff)
		select {
		case <-t.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

func (t *Tunnel) setEndpoints(i int, es []Endpoint) {
	t.mu.Lock()
	t.endpoints[i] = es
	delete(t.errs, t.cfg.Listen[i])
	t.mu.Unlock()
	for _, e := range es {
		t.cfg.Logf("listening on %s", e)
	}
	if t.cfg.Changed != nil {
		t.cfg.Changed()
	}
}

// Address is this peer's address: key, the listener's pre-shared key, and
// every endpoint it listens on, in the order of Config.Listen.
func (t *Tunnel) Address() Address {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := Address{Key: t.cfg.Identity.Public().(ed25519.PublicKey)}
	for i := range t.cfg.Listen {
		a.Endpoints = append(a.Endpoints, t.endpoints[i]...)
	}
	if len(a.Endpoints) > 0 {
		psk := t.st.listenerPSK()
		a.PSK = psk[:]
	}
	return a
}

// Pending lists carriers still coming up, with their last error if any.
func (t *Tunnel) Pending() map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]string{}
	for i, spec := range t.cfg.Listen {
		if len(t.endpoints[i]) == 0 {
			out[spec] = t.errs[spec]
		}
	}
	return out
}

// RotatePSK replaces the listener's pre-shared key. Every address shared
// before stops working for peers not met yet; peers already met keep the
// key agreed with them.
func (t *Tunnel) RotatePSK() error {
	if err := t.st.rotate(); err != nil {
		return err
	}
	// Drop peers admitted under the old key that never completed a stream.
	t.dev.RemoveMatchingPeers(func(pk device.NoisePublicKey) bool {
		_, known := t.st.pair(Key(pk))
		return !known
	})
	return nil
}

// Stream is a connection to port 1 inside the tunnel.
type Stream struct {
	net.Conn
	// Remote is the WireGuard key that completed the handshake.
	Remote Key
	t      *Tunnel
}

// Is reports whether the Ed25519 key s ("ed25519:...") is the one on the
// other end of the stream (section 10.1).
func (s *Stream) Is(pub ed25519.PublicKey) bool {
	k, err := X25519Public(pub)
	return err == nil && k == s.Remote
}

// Via describes the carrier path the peer was last heard on.
func (s *Stream) Via() string {
	if v := s.t.bind.peer(s.Remote).current(); v != "" {
		return v
	}
	return "tunnel"
}

// Accept waits for the next stream a peer opens.
func (t *Tunnel) Accept() (*Stream, error) {
	for {
		c, err := t.ln.Accept()
		if err != nil {
			return nil, err
		}
		ra, ok := c.RemoteAddr().(*net.TCPAddr)
		if !ok {
			c.Close()
			continue
		}
		ip, _ := netip.AddrFromSlice(ra.IP)
		t.mu.Lock()
		k, known := t.keys[ip.Unmap()]
		psk, admitted := t.admitted[k]
		delete(t.admitted, k)
		t.mu.Unlock()
		if !known {
			c.Close()
			continue
		}
		if admitted {
			// A stream proves the handshake held: the pair's key is agreed.
			t.st.setPair(k, psk)
		}
		return &Stream{Conn: c, Remote: k, t: t}, nil
	}
}

// Dial opens a stream to the peer the addresses describe (all for one key).
// The pre-shared key tried first is the one agreed with that peer before,
// then the address's; the one that works is remembered (section 5.2).
func (t *Tunnel) Dial(ctx context.Context, addrs ...Address) (*Stream, error) {
	a, err := Merge(addrs...)
	if err != nil {
		return nil, err
	}
	k, err := X25519Public(a.Key)
	if err != nil {
		return nil, err
	}
	if k == t.pub {
		return nil, errors.New("that address is this peer's own")
	}
	var cands []path
	for _, e := range a.Endpoints {
		if t.carrier(e.Kind) != nil {
			cands = append(cands, path{e.Kind, e.Value})
		}
	}
	ep := t.bind.peer(k)
	if len(cands) == 0 && ep.current() == "" {
		return nil, fmt.Errorf("no endpoint this peer can use in %s", a.KeyString())
	}
	ep.addCands(cands...)
	t.noteKey(k)

	var psks [][32]byte
	if p, ok := t.st.pair(k); ok {
		psks = append(psks, p)
	}
	if len(a.PSK) == 32 {
		var p [32]byte
		copy(p[:], a.PSK)
		if len(psks) == 0 || psks[0] != p {
			psks = append(psks, p)
		}
	}
	if len(psks) == 0 {
		psks = append(psks, [32]byte{})
	}
	// A dial means the last stream is gone. Whatever session WireGuard
	// still holds may be one the peer lost in a restart; start afresh
	// rather than wait for WireGuard to notice.
	if p, ok := t.dev.LookupActivePeer(device.NoisePublicKey(k)); ok {
		p.ExpireCurrentKeypairs()
	}
	var lastErr error
	for i, psk := range psks {
		if err := t.configure(k, psk); err != nil {
			return nil, err
		}
		actx, cancel := ctx, context.CancelFunc(func() {})
		if i < len(psks)-1 {
			actx, cancel = context.WithTimeout(ctx, 12*time.Second)
		}
		c, err := t.tun.dialTCP(actx, netip.AddrPortFrom(IP(k), Port))
		cancel()
		if err == nil {
			t.st.setPair(k, psk)
			return &Stream{Conn: c, Remote: k, t: t}, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("no tunnel to %s: %w", a.KeyString(), lastErr)
}

// configure adds or updates the WireGuard peer for k.
func (t *Tunnel) configure(k Key, psk [32]byte) error {
	var b strings.Builder
	fmt.Fprintf(&b, "public_key=%s\n", hex.EncodeToString(k[:]))
	fmt.Fprintf(&b, "preshared_key=%s\n", hex.EncodeToString(psk[:]))
	fmt.Fprintf(&b, "endpoint=peer:%s\n", hex.EncodeToString(k[:]))
	b.WriteString("replace_allowed_ips=true\n")
	fmt.Fprintf(&b, "allowed_ip=%s/128\n", IP(k))
	return t.dev.IpcSet(b.String())
}

// Close stops the device and every carrier.
func (t *Tunnel) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	t.cancel()
	t.ln.Close()
	t.udp.close()
	t.unix.close()
	t.tailcat.close()
	t.ws.close()
	t.dev.Close()
	return nil
}

// CloseWrite half-closes the stream: the peer reads EOF, and can still
// write.
func (s *Stream) CloseWrite() error {
	if cw, ok := s.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return s.Conn.Close()
}
