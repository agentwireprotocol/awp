package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/agentwireprotocol/awp/conformance"
	"github.com/agentwireprotocol/awp/schema"
	"github.com/agentwireprotocol/awp/wire"
)

func cmdConform(ctx context.Context, args []string) error {
	f := newFlags("conform", "[<address>]",
		"Check a peer against the protocol (SPEC.md). The runner is a peer of its own: it connects\n"+
			"to the address once per scenario, drives each exchange, checks every line against the\n"+
			"JSON Schema, and reports. With --listen the peer connects to the runner instead, and the\n"+
			"scenarios that fit one connection run in turn. Exit status 1 if any scenario fails.")
	listen := f.String("listen", "", "listen on this carrier (udp:127.0.0.1:0, unix:/path, tailcat, ...) and let the peer connect")
	run := f.String("run", "", "with --listen: a shell command that starts the peer, {addr} replaced by the runner's address")
	only := f.StringArray("scenario", nil, "run only this scenario (repeatable; see --list)")
	timeout := f.Duration("timeout", 15*time.Second, "how long to wait for the peer at each step")
	list := f.Bool("list", false, "list the scenarios and exit")
	trace := f.Bool("trace", false, "print every line sent and received on stderr")
	name := f.String("name", "awp-conform", "the name the runner sends in hello")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *list {
		for _, s := range conformance.Scenarios() {
			extra := ""
			if s.Solo {
				extra = " (needs its own connection)"
			}
			if s.Needs != "" {
				extra = " (needs hello cap " + s.Needs + ")"
			}
			fmt.Printf("  %-15s §%-8s %s%s\n", s.Name, s.Section, s.Doc, extra)
		}
		return nil
	}
	o := conformance.Options{Listen: *listen, Run: *run, Only: *only, Timeout: *timeout, Name: *name}
	if f.NArg() > 0 {
		o.Addr = f.Arg(0)
	}
	if o.Addr == "" && o.Listen == "" {
		return errors.New("give the peer's address, or --listen for the peer to connect here")
	}
	if o.Addr != "" && o.Listen != "" {
		return errors.New("give either the peer's address or --listen, not both")
	}
	if *trace {
		o.Trace = func(dir string, line []byte) {
			arrow := "<"
			if dir == "out" {
				arrow = ">"
			}
			if len(line) > 400 {
				line = append(line[:400:400], "…"...)
			}
			fmt.Fprintf(os.Stderr, "%s %s\n", arrow, line)
		}
	}
	if !*f.json {
		o.Logf = func(format string, args ...any) {
			if *trace {
				fmt.Fprintf(os.Stderr, "# "+format+"\n", args...)
			}
		}
	}
	rep, err := conformance.Run(ctx, o)
	if err != nil {
		return err
	}
	if *f.json {
		if err := printJSON(rep); err != nil {
			return err
		}
	} else {
		printReport(rep)
	}
	if rep.Failed > 0 {
		return exitCode(1)
	}
	return nil
}

func printReport(rep *conformance.Report) {
	fmt.Printf("awp conform: %s %s\n", rep.Mode, rep.Addr)
	if p := rep.Peer; p != nil {
		name := p.Name
		if name == "" {
			name = "(no name)"
		}
		fmt.Printf("peer %s %s (v%d; caps %s)\n", name, p.Key, p.V, strings.Join(p.Caps, " "))
	}
	fmt.Println()
	status := map[string]string{"pass": "ok  ", "fail": "FAIL", "skip": "skip"}
	for _, r := range rep.Results {
		line := r.Doc
		if r.Status != "pass" && r.Reason != "" {
			line = r.Reason
		}
		fmt.Printf("  %s  %-15s §%-8s %s\n", status[r.Status], r.Name, r.Section, line)
		if r.Status == "fail" {
			for _, c := range r.Checks {
				if !c.OK && c.What != r.Reason {
					fmt.Printf("        - %s\n", c.What)
				}
			}
		}
	}
	fmt.Println()
	fmt.Printf("%d passed, %d failed, %d skipped in %s\n", rep.Passed, rep.Failed, rep.Skipped, rep.Duration.Round(100*time.Millisecond))
}

func cmdSchema(ctx context.Context, args []string) error {
	f := newFlags("schema", "", fmt.Sprintf("Print the JSON Schema (draft 2020-12) of protocol v%d: every message and its fields.\nThe same document is published at %s.", wire.Version, schema.ID))
	if err := f.Parse(args); err != nil {
		return err
	}
	_, err := os.Stdout.Write(schema.V0)
	return err
}
