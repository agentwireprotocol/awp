package tunnel

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// state is what the tunnel persists: the listener's pre-shared key, and the
// pre-shared key agreed with each peer (section 5.2). It lives in
// tunnel.json in the state directory, or only in memory.
type state struct {
	path string

	mu    sync.Mutex
	PSK   string            `json:"psk"`
	Pairs map[string]string `json:"pairs"` // tunnel key (base64url) -> pre-shared key
}

func loadState(dir string) (*state, error) {
	s := &state{Pairs: map[string]string{}}
	if dir != "" {
		s.path = filepath.Join(dir, "tunnel.json")
		b, err := os.ReadFile(s.path)
		switch {
		case err == nil:
			if err := json.Unmarshal(b, s); err != nil {
				return nil, fmt.Errorf("%s: %w", s.path, err)
			}
			if s.Pairs == nil {
				s.Pairs = map[string]string{}
			}
		case !errors.Is(err, os.ErrNotExist):
			return nil, err
		}
	}
	if _, err := decode32(s.PSK); err != nil {
		s.PSK = random32()
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func random32() string {
	var b [32]byte
	rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func decode32(s string) ([32]byte, error) {
	var out [32]byte
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != 32 {
		return out, errors.New("want 32 bytes, base64url")
	}
	copy(out[:], b)
	return out, nil
}

func (s *state) saveLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *state) listenerPSK() [32]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, _ := decode32(s.PSK)
	return k
}

func (s *state) rotate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PSK = random32()
	return s.saveLocked()
}

func (s *state) pair(k Key) ([32]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.Pairs[k.String()]
	if !ok {
		return [32]byte{}, false
	}
	p, err := decode32(v)
	return p, err == nil
}

func (s *state) setPair(k Key, psk [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := base64.RawURLEncoding.EncodeToString(psk[:])
	if s.Pairs[k.String()] == v {
		return
	}
	s.Pairs[k.String()] = v
	s.saveLocked()
}
