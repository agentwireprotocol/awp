package render

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentwireprotocol/awp/internal/api"
	"github.com/agentwireprotocol/awp/wire"
)

// A key prefix can start with -, which a shell would read as flags: the
// hint names the peer, or puts -- ahead of the prefix.
func TestIntroduceHint(t *testing.T) {
	key := "ed25519:-EdZIWymh9AbCdEfGhIjKlMnOpQrStUvWxYz0123456"
	for _, c := range []struct{ name, want string }{
		{"qa-lead@sprite", "connect with: awp connect qa-lead@sprite\n"},
		{"", "connect with: awp connect -- -EdZIWymh9\n"},
	} {
		msg, _ := json.Marshal(wire.Introduce{Peer: wire.IntroPeer{Key: key, Name: c.name}})
		got := Event(api.Event{Type: wire.TIntroduce, Dir: "in", PeerName: "boss", Msg: msg}, Options{})
		if !strings.Contains(got, c.want) {
			t.Errorf("name %q: got %q, want it to contain %q", c.name, got, c.want)
		}
	}
}
