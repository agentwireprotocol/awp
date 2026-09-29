package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/agentwireprotocol/awp/internal/helper"
)

func cmdTunnel(ctx context.Context, args []string) error {
	f := newFlags("tunnel", "",
		"The WireGuard tunnel as a helper process, for SDKs that speak the protocol themselves\n"+
			"(SPEC.md section 18.2). It prints JSON lines on stdout (a ready event, then an address\n"+
			"event whenever the address changes), dials on request over --socket, forwards the\n"+
			"streams peers open to --forward, and exits when stdin closes. The identity comes from\n"+
			"--identity (a JSON file with \"seed\") or $AWP_IDENTITY_SEED.")
	identity := f.String("identity", "", "identity file: JSON with \"seed\", 32 bytes of base64url")
	state := f.String("state", "", "directory for pre-shared keys and the tailcat key (default: in memory)")
	listen := f.StringArray("listen", nil, "carrier to listen on: tailcat, udp:HOST:PORT, unix:/path, ws:HOST:PORT[=URL] or cloudflare (repeatable)")
	socket := f.String("socket", "", "control socket to create, for dial requests")
	forward := f.String("forward", "", "where streams peers open go: a unix socket path, or tcp:HOST:PORT")
	verbose := f.Bool("verbose", false, "log the tunnel's activity on stderr")
	if err := f.Parse(args); err != nil {
		return err
	}
	var cfg helper.Config
	var err error
	switch {
	case *identity != "":
		cfg.Identity, err = helper.LoadIdentity(*identity)
	case os.Getenv("AWP_IDENTITY_SEED") != "":
		cfg.Identity, err = helper.SeedKey(os.Getenv("AWP_IDENTITY_SEED"))
	default:
		err = errors.New("give --identity or $AWP_IDENTITY_SEED")
	}
	if err != nil {
		return err
	}
	if *socket == "" {
		return errors.New("--socket is required")
	}
	if len(*listen) > 0 && *forward == "" {
		return errors.New("--listen needs --forward, where accepted streams go")
	}
	cfg.StateDir, cfg.Listen, cfg.Socket, cfg.Forward = *state, *listen, *socket, *forward
	if *verbose {
		lg := log.New(os.Stderr, "awp tunnel: ", log.LstdFlags)
		cfg.Logf = func(format string, args ...any) { lg.Output(2, fmt.Sprintf(format, args...)) }
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return helper.Run(ctx, cfg, os.Stdin, os.Stdout)
}
