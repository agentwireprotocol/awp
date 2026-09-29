package conformance_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentwireprotocol/awp/internal/conformance"
	"github.com/agentwireprotocol/awp/internal/node"
)

func startNode(t *testing.T) *node.Node {
	t.Helper()
	home := t.TempDir()
	n, err := node.Open(node.Config{
		Home:             home,
		Name:             "go@test",
		Listen:           []string{"unix:" + filepath.Join(home, "s")},
		PingInterval:     2 * time.Second,
		HandshakeTimeout: 5 * time.Second,
		MaxBackoff:       500 * time.Millisecond,
		DialGrace:        300 * time.Millisecond,
		Logf:             func(format string, args ...any) { t.Logf("[node] "+format, args...) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

func report(t *testing.T, rep *conformance.Report) {
	t.Helper()
	for _, r := range rep.Results {
		t.Logf("%-5s %-15s %s", r.Status, r.Name, r.Reason)
		for _, c := range r.Checks {
			if !c.OK {
				t.Logf("        %s", c.What)
			}
		}
	}
}

// TestReferenceImplementation runs every scenario against the Go node:
// the reference implementation must pass its own conformance suite.
func TestReferenceImplementation(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rep, err := conformance.Run(ctx, conformance.Options{
		Addr:    n.Addresses()[0],
		Timeout: 10 * time.Second,
		Logf:    t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	report(t, rep)
	if rep.Failed != 0 || rep.Skipped != 0 {
		t.Fatalf("%d passed, %d failed, %d skipped", rep.Passed, rep.Failed, rep.Skipped)
	}
	if rep.Passed != len(conformance.Scenarios()) {
		t.Fatalf("%d passed of %d scenarios", rep.Passed, len(conformance.Scenarios()))
	}
	if rep.Peer == nil || rep.Peer.Key != n.Key() || rep.Peer.Name != "go@test" {
		t.Fatalf("peer %+v", rep.Peer)
	}
}

// TestListenMode has the node dial the runner; the scenarios that share a
// connection run, the solo ones are skipped.
func TestListenMode(t *testing.T) {
	n := startNode(t)
	sock := "unix:" + filepath.Join(t.TempDir(), "runner.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	go func() {
		// Give the runner a moment to listen, then connect.
		time.Sleep(300 * time.Millisecond)
		if _, err := n.Connect(ctx, sock); err != nil {
			t.Logf("connect: %v", err)
		}
	}()
	rep, err := conformance.Run(ctx, conformance.Options{Listen: sock, Timeout: 10 * time.Second, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	report(t, rep)
	if rep.Failed != 0 {
		t.Fatalf("%d failed", rep.Failed)
	}
	var solo int
	for _, s := range conformance.Scenarios() {
		if s.Solo {
			solo++
		}
	}
	if rep.Skipped != solo {
		t.Fatalf("%d skipped, want the %d solo scenarios", rep.Skipped, solo)
	}
	if !strings.HasPrefix(rep.Addr, "unix:") {
		t.Fatalf("report address %q", rep.Addr)
	}
}

// TestOnly runs a subset and rejects unknown names.
func TestOnly(t *testing.T) {
	n := startNode(t)
	ctx := context.Background()
	rep, err := conformance.Run(ctx, conformance.Options{Addr: n.Addresses()[0], Only: []string{"ping", "bye"}, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 2 || rep.Passed != 2 {
		report(t, rep)
		t.Fatalf("results %+v", rep.Results)
	}
	if _, err := conformance.Run(ctx, conformance.Options{Addr: n.Addresses()[0], Only: []string{"nope"}}); err == nil {
		t.Fatal("unknown scenario accepted")
	}
}
