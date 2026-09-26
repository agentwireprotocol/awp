// Command holler is the reference peer for the holler agent messaging
// protocol: a daemon that holds the identity, the tailcat listener and every
// connection, plus a small CLI, harness hooks and an MCP server on top.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"github.com/hollerprotocol/holler/internal/api"
	"github.com/hollerprotocol/holler/internal/control"
	"github.com/hollerprotocol/holler/internal/daemon"
)

type command struct {
	name    string
	args    string
	summary string
	run     func(ctx context.Context, args []string) error
}

var commands []command

func init() {
	commands = []command{
		{"up", "", "start the daemon in the background (if needed) and print the address to share", cmdUp},
		{"listen", "", "start listening, print the address, then stream inbound messages as NDJSON until killed", cmdListen},
		{"connect", "<address>", "connect to a peer's address (the daemon keeps it alive and reconnects)", cmdConnect},
		{"send", "[<peer>] <text>...", "send a message; starts a new thread unless --thread is given", cmdSend},
		{"state", "[<peer>] <thread> <state>", "set your state on a thread: working, waiting, done, failed, closed", cmdState},
		{"tail", "", "print unread messages (--once), or follow new ones", cmdTail},
		{"wait", "", "block until a new message arrives (or a thread reaches --state)", cmdWait},
		{"read", "[<thread>]", "show a thread's (or a peer's) conversation, both directions", cmdRead},
		{"threads", "", "list threads and their states", cmdThreads},
		{"peers", "", "list known peers and connections", cmdPeers},
		{"status", "", "show identity, addresses and peers", cmdStatus},
		{"address", "", "print just the address to share", cmdAddress},
		{"grant", "<peer> <cap>...", "grant capabilities (exec, fs:read, fs:write, introduce, admin) to a peer", cmdGrant},
		{"grants", "", "list grants issued, held and presented", cmdGrants},
		{"revoke", "<hash>", "stop honoring a grant you issued", cmdRevoke},
		{"introduce", "<to> <peer> [<cap>...]", "hand <to> the address of <peer> plus a grant <peer> will honor", cmdIntroduce},
		{"bye", "<peer>", "close the connection to a peer gracefully", cmdBye},
		{"alias", "<peer> <alias>", "give a peer a local nickname", cmdAlias},
		{"blobs", "", "list blobs sent and received", cmdBlobs},
		{"down", "", "stop the daemon", cmdDown},
		{"bootstrap", "", "install holler into this machine's agent harnesses (Claude Code, Codex, ...)", cmdBootstrap},
		{"daemon", "", "run the daemon in the foreground", cmdDaemon},
		{"mcp", "", "run the MCP server (stdio) for harnesses that prefer tools", cmdMCP},
		{"hook", "<event>", "harness hook helper (session-start, inbox, stop)", cmdHook},
		{"web", "", "the dashboard in a browser: the whole network, live", cmdWeb},
		{"model", "[<model>]", "show or set the model this agent runs on (shared with the network)", cmdModel},
		{"share", "[<host>...]", "show or set the hosts this agent shares its conversations with", cmdShare},
		{"private", "[<peer>] <thread>", "keep a thread out of conversation sharing", cmdPrivate},
		{"version", "", "print the version", cmdVersion},
	}
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		if len(args) > 1 {
			if c := findCommand(args[1]); c != nil {
				c.run(context.Background(), []string{"--help"})
				return
			}
		}
		usage(os.Stdout)
		return
	}
	c := findCommand(args[0])
	if c == nil {
		fmt.Fprintf(os.Stderr, "holler: unknown command %q\n\n", args[0])
		usage(os.Stderr)
		os.Exit(2)
	}
	err := c.run(context.Background(), args[1:])
	var ec exitCode
	switch {
	case err == nil:
	case errors.Is(err, pflag.ErrHelp):
	case errors.As(err, &ec):
		os.Exit(int(ec))
	default:
		fmt.Fprintf(os.Stderr, "holler %s: %v\n", c.name, err)
		os.Exit(1)
	}
}

func findCommand(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "holler: peer-to-peer messaging between agents, over tailcat.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "usage: holler <command> [flags] [args]")
	fmt.Fprintln(w)
	for _, c := range commands {
		fmt.Fprintf(w, "  %-10s %-26s %s\n", c.name, c.args, c.summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Every command takes --home (default $HOLLER_HOME or ~/.holler) and most take --json.")
	fmt.Fprintln(w, "Commands start the daemon on demand. Run `holler <command> --help` for details.")
}

// exitCode ends the program quietly with a status.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

// flagSet makes a subcommand flag set with the common flags.
type flags struct {
	*pflag.FlagSet
	home *string
	json *bool
}

func newFlags(name, args, summary string) *flags {
	fs := pflag.NewFlagSet(name, pflag.ContinueOnError)
	fs.SetInterspersed(true)
	f := &flags{FlagSet: fs}
	f.home = fs.String("home", daemon.DefaultHome(), "holler state directory")
	f.json = fs.Bool("json", false, "machine-readable JSON output")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: holler %s [flags] %s\n\n%s\n\nflags:\n", name, args, summary)
		fs.PrintDefaults()
	}
	return f
}

func (f *flags) client() *control.Client {
	return &control.Client{Home: *f.home}
}

// ensureDaemon returns a client for a running daemon, starting one in the
// background if needed.
func (f *flags) ensureDaemon() (*control.Client, error) {
	c := f.client()
	if c.Running() {
		return c, nil
	}
	if err := os.MkdirAll(*f.home, 0o700); err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(*f.home, "daemon.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "daemon", "--home", *f.home)
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting daemon: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.After(20 * time.Second)
	for {
		if c.Running() {
			cmd.Process.Release()
			return c, nil
		}
		select {
		case err := <-exited:
			if c.Running() {
				return c, nil // lost a race with another starter
			}
			if strings.Contains(tail(logPath, 1), "another holler daemon is running") {
				// One is starting or still shutting down: give it a moment.
				for i := 0; i < 50 && !c.Running(); i++ {
					time.Sleep(100 * time.Millisecond)
				}
				if c.Running() {
					return c, nil
				}
			}
			return nil, fmt.Errorf("daemon exited (%v); see %s:\n%s", err, logPath, tail(logPath, 10))
		case <-deadline:
			return nil, fmt.Errorf("daemon did not start within 20s; see %s", logPath)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func tail(path string, lines int) string {
	b, _ := os.ReadFile(path)
	ls := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(ls) > lines {
		ls = ls[len(ls)-lines:]
	}
	return strings.Join(ls, "\n")
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printLine(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// waitAddress polls status until tailcat is up (if enabled), for commands
// that need an address to hand out.
func waitAddress(ctx context.Context, c *control.Client, timeout time.Duration) (*api.Status, error) {
	deadline := time.Now().Add(timeout)
	for {
		var st api.Status
		if err := c.Call(ctx, "status", nil, &st); err != nil {
			return nil, err
		}
		if st.Tailcat != "" || !st.TailcatWant || time.Now().After(deadline) {
			return &st, nil
		}
		select {
		case <-ctx.Done():
			return &st, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
