package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agentwireprotocol/awp/internal/web"
)

func cmdWeb(ctx context.Context, args []string) error {
	f := newFlags("web", "", "Serve the web dashboard: every agent on the network, the connections between\nthem, every thread's state and the activity as it happens, in a browser.\nIt reads this host's daemon (it does not start one). Agents on other hosts\nappear when they share presence (awp up --presence). Conversations can be\nopened only for threads this host is part of.")
	listen := f.String("listen", "127.0.0.1:7788", "address to serve on")
	allow := f.StringSlice("allow-host", nil, "also accept requests for this host name (behind a proxy)")
	if err := f.Parse(args); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("--listen %q: %w", *listen, err)
	}
	loopback := host == "localhost"
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		loopback = true
	}
	if !loopback {
		fmt.Fprintf(os.Stderr, "awp web: warning: serving on %s with no authentication; anyone who can reach it sees your agents and your conversations\n", *listen)
	}
	srv := &web.Server{C: f.client(), Assets: web.Assets()}
	if srv.Assets == nil {
		fmt.Fprintln(os.Stderr, "awp web: this binary was built without the page (make web); serving the API only")
	}
	if !srv.C.Running() {
		fmt.Fprintln(os.Stderr, "awp web: the daemon is not running yet (awp up); the page will show it when it is")
	}
	go srv.Run(ctx)

	h := srv.Handler()
	if loopback {
		h = hostCheck(h, *allow)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	fmt.Printf("awp web: http://%s/\n", ln.Addr())
	hs := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// hostCheck refuses requests addressed to other host names, so a web page
// elsewhere cannot reach a loopback-only dashboard by rebinding its DNS
// name to 127.0.0.1.
func hostCheck(h http.Handler, allow []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if hh, _, err := net.SplitHostPort(host); err == nil {
			host = hh
		}
		host = strings.Trim(strings.ToLower(host), "[]")
		ok := host == "localhost" || strings.HasSuffix(host, ".localhost")
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			ok = true
		}
		for _, a := range allow {
			ok = ok || strings.EqualFold(a, host)
		}
		if !ok {
			http.Error(w, "awp web: unexpected host "+r.Host+" (use --allow-host to accept it)", http.StatusMisdirectedRequest)
			return
		}
		h.ServeHTTP(w, r)
	})
}
