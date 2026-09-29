package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/agentwireprotocol/awp/wire"
)

// identityFile is the on-disk form of the long-term Ed25519 key.
type identityFile struct {
	Key  string `json:"key"`
	Seed string `json:"seed"` // base64url, 32 bytes
}

// loadIdentity reads the key at path, creating a new one (mode 0600) if the
// file does not exist. A sandbox that starts with an empty home therefore
// gets a fresh key, as section 16 recommends.
func loadIdentity(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		priv := ed25519.NewKeyFromSeed(seed)
		f := identityFile{Key: wire.FormatKey(priv.Public().(ed25519.PublicKey)), Seed: wire.EncodeB64(seed)}
		out, _ := json.MarshalIndent(f, "", "  ")
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
			return nil, err
		}
		return priv, os.Rename(tmp, path)
	}
	if err != nil {
		return nil, err
	}
	var f identityFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seed, err := wire.DecodeB64(f.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s: bad seed", path)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	if f.Key != "" && f.Key != wire.FormatKey(priv.Public().(ed25519.PublicKey)) {
		return nil, fmt.Errorf("%s: key does not match seed", path)
	}
	return priv, nil
}
