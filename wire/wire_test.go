package wire

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestIDsIncrease(t *testing.T) {
	g := NewIDGen()
	fixed := time.UnixMilli(1_790_000_000_000)
	g.now = func() time.Time { return fixed }
	prev := ""
	for i := 0; i < 10000; i++ {
		id := g.New()
		if len(id) != 26 {
			t.Fatalf("len(%q) = %d", id, len(id))
		}
		if id <= prev {
			t.Fatalf("id %d not increasing: %q <= %q", i, id, prev)
		}
		prev = id
	}
	// Clock steps backwards: ids must still increase.
	g.now = func() time.Time { return fixed.Add(-time.Hour) }
	if id := g.New(); id <= prev {
		t.Fatalf("after clock step back: %q <= %q", id, prev)
	}
}

func TestIDObserveAndRoundTrip(t *testing.T) {
	g := NewIDGen()
	future := NewIDGen()
	future.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	far := future.New()
	g.Observe(far)
	if id := g.New(); id <= far {
		t.Fatalf("New after Observe: %q <= %q", id, far)
	}
	u, ok := decodeULID(far)
	if !ok || encodeULID(u) != far {
		t.Fatalf("round trip failed for %q", far)
	}
	ts, ok := IDTime(far)
	if !ok || time.Until(ts) < 23*time.Hour {
		t.Fatalf("IDTime(%q) = %v, %v", far, ts, ok)
	}
	if _, ok := decodeULID("not-a-ulid"); ok {
		t.Fatal("decoded garbage")
	}
}

func TestKeys(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	s := FormatKey(pub)
	if !strings.HasPrefix(s, "ed25519:") || strings.Contains(s, "=") {
		t.Fatalf("FormatKey = %q", s)
	}
	got, err := ParseKey(s)
	if err != nil || !got.Equal(pub) {
		t.Fatalf("ParseKey: %v", err)
	}
	// Padded and standard-alphabet forms are accepted too.
	padded := s + "="
	if _, err := ParseKey(padded); err != nil {
		t.Fatalf("padded: %v", err)
	}
	std := strings.NewReplacer("-", "+", "_", "/").Replace(s)
	if got, err := ParseKey(std); err != nil || !got.Equal(pub) {
		t.Fatalf("std alphabet: %v", err)
	}
	for _, bad := range []string{"", "ed25519:", "rsa:AAAA", "ed25519:AAAA"} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) succeeded", bad)
		}
	}
}

func TestAuthSignature(t *testing.T) {
	pubA, privA, _ := ed25519.GenerateKey(nil)
	helloA := []byte(`{"t":"hello","id":"a1","ts":"x","v":0,"key":"A","nonce":"nA"}`)
	helloB := []byte(`{"t":"hello","id":"b1","ts":"x","v":0,"key":"B","nonce":"nB"}`)
	sig := SignAuth(privA, helloA, helloB)
	// B verifies with A's key, peer (A) hello first.
	if err := VerifyAuth(pubA, sig, helloA, helloB); err != nil {
		t.Fatal(err)
	}
	// Swapped order, a different hello, or a different key must fail.
	if VerifyAuth(pubA, sig, helloB, helloA) == nil {
		t.Fatal("verified with swapped hellos")
	}
	if VerifyAuth(pubA, sig, helloA, append(helloB, ' ')) == nil {
		t.Fatal("verified with modified hello")
	}
	pubC, _, _ := ed25519.GenerateKey(nil)
	if VerifyAuth(pubC, sig, helloA, helloB) == nil {
		t.Fatal("verified with wrong key")
	}
	payload := AuthPayload(helloA, helloB)
	want := "awp-auth-v0\x00" + string(helloA) + "\x00" + string(helloB)
	if string(payload) != want {
		t.Fatalf("payload = %q", payload)
	}
}

const canonInput = `{"z":1,"a":{"y":[3,"x",null,true],"b":"q\"uo\\te"},"m":"<&> é ✓   \u0001 \t\n","caps":["exec","fs:read"],"n":-1.50e3}`

func TestCanonical(t *testing.T) {
	got, err := Canonical([]byte(canonInput))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":{"b":"q\"uo\\te","y":[3,"x",null,true]},"caps":["exec","fs:read"],"m":"<&> é ✓ ` + " " + ` \u0001 \t\n","n":-1.50e3,"z":1}`
	if string(got) != want {
		t.Fatalf("Canonical:\n got %s\nwant %s", got, want)
	}
}

// TestCanonicalMatchesPython checks the property that matters for
// interoperability: an independent peer computing canonical JSON with the
// obvious Python one-liner gets the same bytes (numbers aside, which grants
// do not use).
func TestCanonicalMatchesPython(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	input := `{"iss":"ed25519:AAA","sub":"ed25519:BBB","caps":["exec","fs:read","ünï<>&"],"exp":"2026-09-25T20:00:00Z","nonce":"n \u0007\"\\/","x":{"b":[true,false,null],"a":"\t"}}`
	cmd := exec.Command(py, "-c", `import json,sys; sys.stdout.write(json.dumps(json.loads(sys.stdin.read()), sort_keys=True, separators=(",", ":"), ensure_ascii=False))`)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Canonical([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, out) {
		t.Fatalf("mismatch:\n  go %s\n  py %s", got, out)
	}
}

func TestGrant(t *testing.T) {
	pubA, privA, _ := ed25519.GenerateKey(nil)
	pubB, _, _ := ed25519.GenerateKey(nil)
	exp := time.Now().Add(time.Hour)
	g, err := MintGrant(privA, FormatKey(pubB), []string{"exec", "fs:read"}, exp, "")
	if err != nil {
		t.Fatal(err)
	}
	if g.Iss != FormatKey(pubA) || g.Sub != FormatKey(pubB) || !g.Has("exec") || g.Has("admin") {
		t.Fatalf("grant fields: %+v", g)
	}
	if _, err := ParseGrant(g.Raw, time.Now()); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Re-serialized with different key order and whitespace: still valid.
	var obj map[string]any
	json.Unmarshal(g.Raw, &obj)
	pretty, _ := json.MarshalIndent(obj, "", "  ")
	if _, err := ParseGrant(pretty, time.Now()); err != nil {
		t.Fatalf("pretty-printed grant: %v", err)
	}
	// Tampering with caps breaks the signature.
	obj["caps"] = []any{"exec", "fs:read", "admin"}
	tampered, _ := json.Marshal(obj)
	if _, err := ParseGrant(tampered, time.Now()); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("tampered grant: err = %v", err)
	}
	// Expiry.
	if _, err := ParseGrant(g.Raw, exp.Add(time.Second)); !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("expired grant: err = %v", err)
	}
	// Unknown fields are covered by the signature and preserved.
	body := map[string]any{"iss": g.Iss, "sub": g.Sub, "caps": []any{"exec"}, "exp": exp.UTC().Format(time.RFC3339), "nonce": "n", "ext": map[string]any{"why": "ci"}}
	canon, _ := CanonicalValue(body)
	body["sig"] = EncodeB64(ed25519.Sign(privA, canon))
	raw, _ := json.Marshal(body)
	if _, err := ParseGrant(raw, time.Now()); err != nil {
		t.Fatalf("grant with extension field: %v", err)
	}
	// The aud extension is signed like everything else.
	pubC, _, _ := ed25519.GenerateKey(nil)
	ga, err := MintGrant(privA, FormatKey(pubB), nil, exp, FormatKey(pubC))
	if err != nil || ga.Aud != FormatKey(pubC) || len(ga.Caps) != 0 {
		t.Fatalf("aud grant: %+v %v", ga, err)
	}
	if _, err := ParseGrant(ga.Raw, time.Now()); err != nil {
		t.Fatalf("aud grant verify: %v", err)
	}
	json.Unmarshal(ga.Raw, &obj)
	obj["aud"] = FormatKey(pubB)
	moved, _ := json.Marshal(obj)
	if _, err := ParseGrant(moved, time.Now()); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("re-targeted aud accepted: %v", err)
	}
}

func TestReader(t *testing.T) {
	long := strings.Repeat("x", MaxLine)
	in := "one\n\n" + long + "\n" + long + "y\nlast"
	r := NewReader(strings.NewReader(in))
	for _, want := range []string{"one", "", long} {
		got, err := r.ReadLine()
		if err != nil || string(got) != want {
			t.Fatalf("ReadLine = %d bytes, %v; want %d bytes", len(got), err, len(want))
		}
	}
	if _, err := r.ReadLine(); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("oversized line: err = %v", err)
	}
	r = NewReader(strings.NewReader("partial"))
	if _, err := r.ReadLine(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("partial line: err = %v", err)
	}
	r = NewReader(strings.NewReader(""))
	if _, err := r.ReadLine(); !errors.Is(err, io.EOF) {
		t.Fatalf("empty input: err = %v", err)
	}
}

func TestEncode(t *testing.T) {
	m := Msg{
		Envelope: Envelope{T: TMsg, ID: "01J9", TS: "2026-09-25T17:03:11.000Z", Th: "thr_1"},
		Subject:  "a <b> & c",
		Parts: []Part{
			{K: PartText, Text: "line1\nline2"},
			{K: PartCode, Lang: "diff", Text: "--- a\n+++ b"},
			{K: PartData, Mime: "application/json", Data: json.RawMessage(`{"branch":"x"}`)},
			{K: PartBlob, Ref: "b1", Name: "log.txt", Mime: "text/plain", Size: 0},
		},
	}
	line, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	s := string(line)
	if strings.Contains(s, "\n") || strings.Contains(s, "\\"+"u003c") {
		t.Fatalf("encoded line has raw newline or HTML escaping: %s", s)
	}
	for _, want := range []string{`"t":"msg"`, `"subject":"a <b> & c"`, `{"k":"blob","ref":"b1","name":"log.txt","mime":"text/plain","size":0}`, `{"k":"data","mime":"application/json","data":{"branch":"x"}}`, `{"k":"code","lang":"diff","text":"--- a\n+++ b"}`} {
		if !strings.Contains(s, want) {
			t.Errorf("encoded line missing %s\n%s", want, s)
		}
	}
	var back Msg
	if err := Decode(line, &back); err != nil || len(back.Parts) != 4 || back.Parts[3].Ref != "b1" {
		t.Fatalf("decode: %v %+v", err, back)
	}
	// Hello always carries v, even when it is zero; chunks always carry n and last.
	hl, _ := Encode(Hello{Envelope: Envelope{T: THello}, Key: "k", Nonce: "n"})
	if !strings.Contains(string(hl), `"v":0`) {
		t.Fatalf("hello without v: %s", hl)
	}
	cl, _ := Encode(Chunk{Envelope: Envelope{T: TChunk}, Ref: "r"})
	if !strings.Contains(string(cl), `"n":0`) || !strings.Contains(string(cl), `"last":false`) {
		t.Fatalf("chunk without n/last: %s", cl)
	}
}

func TestParseEnvelope(t *testing.T) {
	for _, bad := range []string{"", "null", "[1,2]", `"str"`, "{nope", "42"} {
		if _, err := ParseEnvelope([]byte(bad)); !errors.Is(err, ErrNotObject) {
			t.Errorf("ParseEnvelope(%q) err = %v", bad, err)
		}
	}
	env, err := ParseEnvelope([]byte(`{"t":"msg","id":"x","ts":"y","th":"t1","future":{"a":1}}`))
	if err != nil || env.T != "msg" || env.Th != "t1" {
		t.Fatalf("ParseEnvelope: %+v %v", env, err)
	}
}

func TestErrCloses(t *testing.T) {
	closing := []string{ErrBadFrame, ErrVersion, ErrAuth, ErrTooLarge}
	open := []string{ErrForbidden, ErrUnsupported, ErrBlobRefused, ErrInternal, "some_future_code"}
	sort.Strings(closing)
	for _, c := range closing {
		if !ErrCloses(c) {
			t.Errorf("%s should close", c)
		}
	}
	for _, c := range open {
		if ErrCloses(c) {
			t.Errorf("%s should not close", c)
		}
	}
}
