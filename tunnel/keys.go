// Package tunnel is AWP's connection layer (SPEC.md sections 4 to 7): a
// WireGuard tunnel between two peers' identity keys, carried by any mix of
// carriers (udp, tailcat, ws, unix), with a TCP stream on port 1 inside it.
//
// A peer's Ed25519 identity key, converted to X25519, is its WireGuard
// static key, so the tunnel's authentication is the protocol's: a Stream's
// remote key is the key that completed the WireGuard handshake.
package tunnel

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"net/netip"

	"filippo.io/edwards25519"
)

// Port is the TCP port AWP listens on inside the tunnel.
const Port = 1

// MTU is the tunnel's inner MTU: the IPv6 minimum, so nothing inside ever
// needs to know about the path.
const MTU = 1280

// Key is a WireGuard (X25519) public key.
type Key [32]byte

func (k Key) String() string { return base64.RawURLEncoding.EncodeToString(k[:]) }

// X25519Private returns the WireGuard private key for an Ed25519 identity:
// the first half of SHA-512 of the seed, clamped (section 4.2). It is the
// scalar Ed25519 itself signs with.
func X25519Private(priv ed25519.PrivateKey) [32]byte {
	h := sha512.Sum512(priv.Seed())
	var k [32]byte
	copy(k[:], h[:32])
	k[0] &= 248
	k[31] &= 127
	k[31] |= 64
	return k
}

// X25519Public converts an Ed25519 public key to its WireGuard public key,
// the Montgomery form of the same point (RFC 7748 section 4.1).
//
// Two Ed25519 keys that differ only in the sign bit map to the same X25519
// key. Whoever holds the private key of one holds the other's (its
// negation), so this is not a way to claim someone else's identity; peers
// compare identities by the full Ed25519 key.
func X25519Public(pub ed25519.PublicKey) (Key, error) {
	if len(pub) != ed25519.PublicKeySize {
		return Key{}, errors.New("tunnel: bad Ed25519 public key length")
	}
	p, err := new(edwards25519.Point).SetBytes(pub)
	if err != nil {
		return Key{}, err
	}
	var k Key
	copy(k[:], p.BytesMontgomery())
	return k, nil
}

// IP is a tunnel key's IPv6 address inside the tunnel (section 4.3): 0xfd
// followed by the first 15 bytes of SHA-256("awp-ula-v1" || key).
func IP(k Key) netip.Addr {
	h := sha256.New()
	h.Write([]byte("awp-ula-v1"))
	h.Write(k[:])
	var a [16]byte
	a[0] = 0xfd
	copy(a[1:], h.Sum(nil)[:15])
	return netip.AddrFrom16(a)
}
