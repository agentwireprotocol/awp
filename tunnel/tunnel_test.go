package tunnel

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
)

func exampleKey(name string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("awp draft 2 example key: " + name))
	return ed25519.NewKeyFromSeed(seed[:])
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// TestSpecVectors pins the worked example in SPEC.md section 4.2.
func TestSpecVectors(t *testing.T) {
	priv := exampleKey("B")
	if got := b64(priv.Seed()); got != "n91wt2HNOBdumTc7UI0QeqJw8wML6cYWcGzmd0k8Tbo" {
		t.Fatalf("seed %s", got)
	}
	xpriv := X25519Private(priv)
	if got := b64(xpriv[:]); got != "AFrT5h44MRTokzB0U6z4Jesw-ISu-CTuXvFnIMZbzUQ" {
		t.Errorf("X25519 private %s", got)
	}
	xpub, err := X25519Public(priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if got := xpub.String(); got != "O7KNWfmBVF214-z17qtor-Hl4UWIDOerTw_nad9Vn1Q" {
		t.Errorf("X25519 public %s", got)
	}
	// The converted private key's public key is the converted public key.
	check, _ := curve25519.X25519(xpriv[:], curve25519.Basepoint)
	if string(check) != string(xpub[:]) {
		t.Error("conversion mismatch")
	}
	for _, n := range []string{"A", "B", "C"} {
		p := exampleKey(n)
		k, _ := X25519Public(p.Public().(ed25519.PublicKey))
		t.Logf("%s ed25519:%s X25519 %s ip %s", n, b64(p.Public().(ed25519.PublicKey)), k, IP(k))
	}
}

func TestAddress(t *testing.T) {
	priv := exampleKey("B")
	psk := sha256.Sum256([]byte("awp draft 2 example psk"))
	a := Address{Key: priv.Public().(ed25519.PublicKey), PSK: psk[:], Endpoints: []Endpoint{
		{KindUDP, "[2a09:8280:1::4:1b2c]:41641"},
		{KindTailcat, "tcomFwWCCcjS5nKNqAod034nWoJZW0LZqDhhC8U_dKdnDRYQ8uNGFpGQEu"},
		{KindWS, "wss://quiet-otter-7f3a.trycloudflare.com/awp"},
	}}
	s := a.String()
	t.Logf("example address %s", s)
	if !strings.HasPrefix(s, "awp1") || s != a.String() {
		t.Fatal("not deterministic")
	}
	b, err := ParseAddress(" " + s + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != s || !b.Key.Equal(a.Key) || len(b.Endpoints) != 3 || b.Endpoints[2] != a.Endpoints[2] {
		t.Fatalf("round trip: %+v", b)
	}
	if p := b.Public(); p.PSK != nil || p.String() == s {
		t.Fatal("Public kept the psk")
	}
	m, err := Merge(a.Public(), Address{Key: a.Key, Endpoints: []Endpoint{{KindUnix, "/x"}, a.Endpoints[0]}}, a)
	if err != nil || len(m.Endpoints) != 4 || m.PSK == nil {
		t.Fatalf("merge: %+v %v", m, err)
	}
	if _, err := Merge(a, Address{Key: exampleKey("A").Public().(ed25519.PublicKey)}); err == nil {
		t.Fatal("merged different keys")
	}
	for _, bad := range []string{"", "awp1", "awp1!!", "tcfoo", "awp1" + b64([]byte{0xa1, 0x63, 'k', 'e', 'y', 0x41, 0})} {
		if _, err := ParseAddress(bad); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
}

type peer struct {
	priv ed25519.PrivateKey
	t    *Tunnel
}

func newPeer(t *testing.T, dir string, listen ...string) *peer {
	t.Helper()
	var priv ed25519.PrivateKey
	if dir != "" {
		priv = exampleKey(dir)
		dir = filepath.Join(tmp(t), dir)
	} else {
		_, priv, _ = ed25519.GenerateKey(nil)
	}
	return startPeer(t, priv, dir, listen...)
}

var tmpDir string

func tmp(t *testing.T) string {
	if tmpDir == "" {
		// Short, since unix socket paths are limited to ~100 bytes.
		d, err := os.MkdirTemp("", "awptun")
		if err != nil {
			t.Fatal(err)
		}
		tmpDir = d
	}
	return tmpDir
}

func TestMain(m *testing.M) {
	code := m.Run()
	if tmpDir != "" {
		os.RemoveAll(tmpDir)
	}
	os.Exit(code)
}

func startPeer(t *testing.T, priv ed25519.PrivateKey, dir string, listen ...string) *peer {
	t.Helper()
	tu, err := New(Config{Identity: priv, StateDir: dir, Listen: listen, Logf: t.Logf, Verbose: os.Getenv("WGV") != ""})
	if err != nil {
		t.Fatal(err)
	}
	if err := tu.Start(); err != nil {
		tu.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { tu.Close() })
	return &peer{priv: priv, t: tu}
}

// echo serves one stream at a time, replying to each line with its key and
// the line.
func echo(p *peer) {
	go func() {
		for {
			s, err := p.t.Accept()
			if err != nil {
				return
			}
			go func() {
				defer s.Close()
				sc := bufio.NewScanner(s)
				sc.Buffer(make([]byte, 1<<20), 1<<20)
				for sc.Scan() {
					fmt.Fprintf(s, "%s %s\n", s.Remote, sc.Text())
				}
			}()
		}
	}()
}

func roundTrip(t *testing.T, s *Stream, from *peer, msg string) {
	t.Helper()
	s.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := fmt.Fprintln(s, msg); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(s).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%s %s\n", from.t.Key(), msg)
	if line != want {
		t.Fatalf("got %q, want %q", line, want)
	}
}

func dial(t *testing.T, from *peer, addrs ...Address) *Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := from.t.Dial(ctx, addrs...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCarriers(t *testing.T) {
	for _, tc := range []struct{ name, spec string }{
		{"unix", "unix:" + filepath.Join(tmp(t), "c.sock")},
		{"udp", "udp:127.0.0.1:0"},
		{"ws", "ws:127.0.0.1:0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newPeer(t, "", tc.spec)
			echo(b)
			a := newPeer(t, "")
			addr := b.t.Address()
			if len(addr.Endpoints) != 1 || addr.PSK == nil {
				t.Fatalf("address %+v", addr)
			}
			s := dial(t, a, addr)
			defer s.Close()
			if !s.Is(b.priv.Public().(ed25519.PublicKey)) || s.Is(a.priv.Public().(ed25519.PublicKey)) {
				t.Fatal("Is")
			}
			roundTrip(t, s, a, "hello over "+tc.name)
			// A large write crosses many datagrams.
			big := strings.Repeat("x", 200000)
			roundTrip(t, s, a, big)
			t.Logf("via %s", s.Via())
		})
	}
}

func TestAdmission(t *testing.T) {
	sock := filepath.Join(tmp(t), "adm.sock")
	b := newPeer(t, "adm-b", "unix:"+sock)
	echo(b)
	addr := b.t.Address()

	// A wrong pre-shared key gets nowhere.
	c := newPeer(t, "")
	wrong := addr
	wrong.PSK = make([]byte, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	if s, err := c.t.Dial(ctx, wrong); err == nil {
		s.Close()
		t.Fatal("dialed with the wrong psk")
	}
	cancel()

	// The right one works, and the pair is remembered on both sides.
	a := newPeer(t, "adm-a")
	s := dial(t, a, addr)
	roundTrip(t, s, a, "first")
	s.Close()

	// B dials back with no endpoint: it knows where A is from the tunnel.
	echo(a)
	back := dial(t, b, Address{Key: a.priv.Public().(ed25519.PublicKey)})
	roundTrip(t, back, b, "back")
	back.Close()

	// Rotating B's psk locks out new peers holding the old address, but not A.
	if err := b.t.RotatePSK(); err != nil {
		t.Fatal(err)
	}
	d := newPeer(t, "")
	ctx, cancel = context.WithTimeout(context.Background(), 8*time.Second)
	if s, err := d.t.Dial(ctx, addr); err == nil {
		s.Close()
		t.Fatal("old address still admits new peers")
	}
	cancel()
	s = dial(t, a, addr)
	roundTrip(t, s, a, "after rotation")
	s.Close()

	// B restarts from its state directory; A reaches it at the same socket.
	b.t.Close()
	b2 := startPeer(t, b.priv, filepath.Join(tmp(t), "adm-b"), "unix:"+sock)
	echo(b2)
	s = dial(t, a, b2.t.Address())
	roundTrip(t, s, a, "after restart")
	s.Close()
}

func TestNotListening(t *testing.T) {
	// A peer that listens on nothing admits only peers it has met.
	a := newPeer(t, "")
	b := newPeer(t, "", "udp:127.0.0.1:0")
	echo(a)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if s, err := b.t.Dial(ctx, Address{Key: a.priv.Public().(ed25519.PublicKey), Endpoints: []Endpoint{{KindUDP, "127.0.0.1:9"}}}); err == nil {
		s.Close()
		t.Fatal("reached a non-listening stranger")
	}
}

func TestTailcat(t *testing.T) {
	if os.Getenv("AWP_TEST_TAILCAT") == "" {
		t.Skip("set AWP_TEST_TAILCAT=1 to test through tailcat's DERP relays")
	}
	carrierLive(t, "tailcat")
}

func TestCloudflare(t *testing.T) {
	if os.Getenv("AWP_TEST_CLOUDFLARE") == "" {
		t.Skip("set AWP_TEST_CLOUDFLARE=1 to test through a Cloudflare quick tunnel (needs cloudflared)")
	}
	carrierLive(t, "cloudflare")
}

func carrierLive(t *testing.T, spec string) {
	b := newPeer(t, "", spec)
	echo(b)
	deadline := time.Now().Add(90 * time.Second)
	for len(b.t.Address().Endpoints) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not come up: %v", spec, b.t.Pending())
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("address %s", b.t.Address())
	a := newPeer(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s, err := a.t.Dial(ctx, b.t.Address())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	roundTrip(t, s, a, "through "+spec)
	roundTrip(t, s, a, strings.Repeat("y", 100000))
	io.WriteString(s, "")
}
