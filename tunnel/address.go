package tunnel

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/fxamacker/cbor/v2"
)

// AddressPrefix starts every AWP address.
const AddressPrefix = "awp1"

// Carrier kinds (section 7).
const (
	KindUDP     = "udp"
	KindTailcat = "tailcat"
	KindWS      = "ws"
	KindUnix    = "unix"
)

// Endpoint is where a peer can be reached on one carrier.
type Endpoint struct {
	Kind  string `cbor:"k" json:"k"`
	Value string `cbor:"v" json:"v"`
}

func (e Endpoint) String() string { return e.Kind + ":" + e.Value }

// Address is a peer's key, optional pre-shared key and endpoints: everything
// a dialer needs (section 6).
type Address struct {
	Key       ed25519.PublicKey
	PSK       []byte // 32 bytes, or nil
	Endpoints []Endpoint
}

type wireAddress struct {
	Key []byte     `cbor:"key"`
	PSK []byte     `cbor:"psk,omitempty"`
	EP  []Endpoint `cbor:"ep,omitempty"`
}

var encMode = func() cbor.EncMode {
	m, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	return m
}()

// String encodes the address: "awp1" and the unpadded base64url of its
// deterministic CBOR encoding.
func (a Address) String() string {
	b, err := encMode.Marshal(wireAddress{Key: a.Key, PSK: a.PSK, EP: a.Endpoints})
	if err != nil {
		return ""
	}
	return AddressPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// KeyString is the address's key in the protocol's ed25519: form.
func (a Address) KeyString() string {
	return "ed25519:" + base64.RawURLEncoding.EncodeToString(a.Key)
}

// Public is the address without its pre-shared key: what a peer may say
// about itself in hello, where the admission secret has no place.
func (a Address) Public() Address {
	a.PSK = nil
	return a
}

// IsAddress reports whether s looks like an AWP address.
func IsAddress(s string) bool { return strings.HasPrefix(strings.TrimSpace(s), AddressPrefix) }

// ParseAddress decodes an address. Unknown endpoint kinds are kept (a dialer
// skips kinds it has no carrier for); unknown CBOR keys are ignored.
func ParseAddress(s string) (Address, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, AddressPrefix) {
		return Address{}, fmt.Errorf("address %q: want %s...", clip(s), AddressPrefix)
	}
	b, err := base64.RawURLEncoding.DecodeString(s[len(AddressPrefix):])
	if err != nil {
		return Address{}, fmt.Errorf("address %q: %v", clip(s), err)
	}
	var w wireAddress
	if err := cbor.Unmarshal(b, &w); err != nil {
		return Address{}, fmt.Errorf("address %q: %v", clip(s), err)
	}
	if len(w.Key) != ed25519.PublicKeySize {
		return Address{}, fmt.Errorf("address %q: key is %d bytes, want 32", clip(s), len(w.Key))
	}
	if w.PSK != nil && len(w.PSK) != 32 {
		return Address{}, fmt.Errorf("address %q: pre-shared key is %d bytes, want 32", clip(s), len(w.PSK))
	}
	if _, err := X25519Public(w.Key); err != nil {
		return Address{}, fmt.Errorf("address %q: key is not a valid Ed25519 point", clip(s))
	}
	return Address{Key: ed25519.PublicKey(w.Key), PSK: w.PSK, Endpoints: w.EP}, nil
}

// Merge combines addresses of one key: endpoints in first-seen order without
// duplicates, and the first pre-shared key given.
func Merge(as ...Address) (Address, error) {
	if len(as) == 0 {
		return Address{}, errors.New("no address")
	}
	out := Address{Key: as[0].Key}
	seen := map[Endpoint]bool{}
	for _, a := range as {
		if !a.Key.Equal(out.Key) {
			return Address{}, fmt.Errorf("addresses for different keys: %s and %s", out.KeyString(), a.KeyString())
		}
		if out.PSK == nil && a.PSK != nil {
			out.PSK = a.PSK
		}
		for _, e := range a.Endpoints {
			if !seen[e] {
				seen[e] = true
				out.Endpoints = append(out.Endpoints, e)
			}
		}
	}
	return out, nil
}

func clip(s string) string {
	if len(s) > 24 {
		return s[:24] + "…"
	}
	return s
}
