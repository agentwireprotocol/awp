package wire

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// KeyPrefix prefixes an encoded Ed25519 public key.
const KeyPrefix = "ed25519:"

// FormatKey encodes a public key as "ed25519:" + unpadded base64url.
func FormatKey(pub ed25519.PublicKey) string {
	return KeyPrefix + base64.RawURLEncoding.EncodeToString(pub)
}

// ParseKey decodes an "ed25519:..." public key.
func ParseKey(s string) (ed25519.PublicKey, error) {
	rest, ok := strings.CutPrefix(s, KeyPrefix)
	if !ok {
		return nil, fmt.Errorf("key %q: want %q prefix", abbrev(s), KeyPrefix)
	}
	b, err := DecodeB64(rest)
	if err != nil {
		return nil, fmt.Errorf("key %q: %v", abbrev(s), err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("key %q: %d bytes, want %d", abbrev(s), len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// ShortKey is a short, human-friendly form of an encoded key: the first
// ten characters of its base64url body.
func ShortKey(key string) string {
	body := strings.TrimPrefix(key, KeyPrefix)
	if len(body) > 10 {
		body = body[:10]
	}
	return body
}

// Fingerprint is the SHA-256 fingerprint of a public key, the way section 13
// suggests agents print it in their about text.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// EncodeB64 is the encoding used for key, nonce and sig fields: unpadded
// base64url.
func EncodeB64(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeB64 accepts base64url or standard base64, padded or not. The spec
// says base64url for keys, nonces and signatures and plain base64 for chunk
// data, but is silent on padding, so a receiver takes any of them.
func DecodeB64(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	if strings.ContainsAny(s, "+/") {
		return base64.RawStdEncoding.DecodeString(s)
	}
	return base64.RawURLEncoding.DecodeString(s)
}

// Nonce returns n random bytes, base64url encoded.
func Nonce(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return EncodeB64(b)
}

// AuthPayload is the byte string signed in auth:
// "awp-auth-v0" || 0x00 || my_hello_line || 0x00 || peer_hello_line.
func AuthPayload(myHello, peerHello []byte) []byte {
	b := make([]byte, 0, len(AuthContext)+2+len(myHello)+len(peerHello))
	b = append(b, AuthContext...)
	b = append(b, 0)
	b = append(b, myHello...)
	b = append(b, 0)
	b = append(b, peerHello...)
	return b
}

// SignAuth signs the auth transcript from the signer's point of view.
func SignAuth(priv ed25519.PrivateKey, myHello, peerHello []byte) string {
	return EncodeB64(ed25519.Sign(priv, AuthPayload(myHello, peerHello)))
}

// ErrBadSignature means a signature did not verify.
var ErrBadSignature = errors.New("signature does not verify")

// VerifyAuth checks the peer's auth signature. From the verifier's side the
// peer's hello comes first, since the peer signed its own hello first.
func VerifyAuth(peer ed25519.PublicKey, sig string, peerHello, myHello []byte) error {
	raw, err := DecodeB64(sig)
	if err != nil {
		return fmt.Errorf("sig: %v", err)
	}
	if !ed25519.Verify(peer, AuthPayload(peerHello, myHello), raw) {
		return ErrBadSignature
	}
	return nil
}

func abbrev(s string) string {
	if len(s) > 24 {
		return s[:24] + "…"
	}
	return s
}
