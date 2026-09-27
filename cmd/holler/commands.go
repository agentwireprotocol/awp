package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/hollerprotocol/holler/internal/api"
	"github.com/hollerprotocol/holler/internal/daemon"
	"github.com/hollerprotocol/holler/internal/harness"
	"github.com/hollerprotocol/holler/internal/mcp"
	"github.com/hollerprotocol/holler/internal/store"
	"github.com/hollerprotocol/holler/internal/transport"
	"github.com/hollerprotocol/holler/internal/version"
	"github.com/hollerprotocol/holler/wire"
)

func cmdVersion(ctx context.Context, args []string) error {
	fmt.Printf("holler %s (protocol v%d)\n", version.String(), wire.Version)
	return nil
}

func cmdDaemon(ctx context.Context, args []string) error {
	f := newFlags("daemon", "", "Run the holler daemon in the foreground. Other commands start it on demand;\nrun it yourself under a service manager, or to watch its log.")
	listen := f.StringArray("listen", nil, "address to listen on: tailcat, tcp:HOST:PORT or unix:/path (repeatable; default from config, else tailcat)")
	noTailcat := f.Bool("no-tailcat", false, "do not listen on tailcat")
	name := f.String("name", "", "name sent in hello (default holler@HOSTNAME)")
	about := f.String("about", "", "free-text description sent in hello")
	harnessFlag := f.String("harness", "", "agent harness this agent runs in: "+strings.Join(harness.IDs(), ", ")+" (default: detected; remembered)")
	advertise := f.String("advertise", "", "address sent in hello for the peer to dial back (none to disable)")
	serve := f.StringSlice("serve", nil, "capabilities to fulfil automatically for granted peers: exec, fs:read, fs:write")
	root := f.String("root", "", "directory that fs:read/fs:write are confined to and exec runs in")
	accept := f.String("accept", "", "admission policy: any (default) or allowlist")
	allow := f.StringArray("allow", nil, "key to admit under the allowlist policy (repeatable)")
	trust := f.StringArray("trust", nil, "issuer key whose grants to honor (repeatable)")
	trace := f.Bool("trace", false, "log every protocol line")
	verbose := f.Bool("verbose", false, "include tailcat's own logs")
	presence := f.Bool("presence", false, "publish signed presence (threads, states, peers) so holler web on any connected host can see this agent")
	if err := f.Parse(args); err != nil {
		return err
	}
	cfg, err := daemon.LoadConfig(*f.home)
	if err != nil {
		return err
	}
	if len(*listen) > 0 {
		cfg.Listen = *listen
	}
	if *noTailcat {
		var ls []string
		for _, l := range cfg.Listen {
			if l != "tailcat" {
				ls = append(ls, l)
			}
		}
		cfg.Listen = ls
	}
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&cfg.Name, *name)
	set(&cfg.About, *about)
	if *harnessFlag != "" {
		if cfg.Harness = harness.Normalize(*harnessFlag); cfg.Harness == "" {
			return fmt.Errorf("unknown harness %q (known: %s)", *harnessFlag, strings.Join(harness.IDs(), ", "))
		}
	}
	set(&cfg.Advertise, *advertise)
	set(&cfg.Policy.Root, *root)
	set(&cfg.Policy.Accept, *accept)
	if len(*serve) > 0 {
		cfg.Policy.Serve = *serve
	}
	cfg.Policy.Allow = append(cfg.Policy.Allow, *allow...)
	cfg.Policy.Trust = append(cfg.Policy.Trust, *trust...)
	cfg.Trace = cfg.Trace || *trace
	cfg.Verbose = cfg.Verbose || *verbose
	cfg.Presence = cfg.Presence || *presence
	return daemon.Run(cfg)
}

func cmdUp(ctx context.Context, args []string) error {
	f := newFlags("up", "", "Start the daemon in the background if it is not running, wait for the tailcat\naddress, and print the identity and the address to share.")
	name := f.String("name", "", "name to present to peers, e.g. claude-code@myhost (remembered)")
	about := f.String("about", "", "what you are working on, sent in hello (remembered)")
	harnessFlag := f.String("harness", "", "agent harness this agent runs in: "+strings.Join(harness.IDs(), ", ")+" (default: detected; remembered)")
	presence := f.Bool("presence", false, "publish signed presence so holler web on connected hosts can see this agent")
	shareWith := f.StringSlice("share-with", nil, "hosts to mirror your conversations to, e.g. a dashboard (names, aliases or keys; remembered; see holler share)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *presence {
		os.Setenv("HOLLER_PRESENCE", "1")
	}
	if *name != "" {
		os.Setenv("HOLLER_NAME", *name)
	}
	if *about != "" {
		os.Setenv("HOLLER_ABOUT", *about)
	}
	if err := setHarnessEnv(*harnessFlag); err != nil {
		return err
	}
	if (*name != "" || *about != "" || *harnessFlag != "") && f.client().Running() {
		fmt.Fprintln(os.Stderr, "note: the daemon is already running; --name/--about/--harness apply from its next start (holler down; holler up ...)")
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	if len(*shareWith) > 0 {
		if err := c.Call(ctx, "set_share", api.ShareParams{With: *shareWith}, nil); err != nil {
			return err
		}
	}
	st, err := waitAddress(ctx, c, 45*time.Second)
	if err != nil {
		return err
	}
	if *f.json {
		return printJSON(st)
	}
	fmt.Printf("holler is up as %s\n", st.Name)
	fmt.Printf("  key      %s\n", st.Key)
	if addr := st.ShareAddress(); addr != "" {
		fmt.Printf("  address  %s\n", addr)
		fmt.Println("share the address with the other agent; they run: holler connect <address>")
	} else {
		fmt.Printf("  address  none yet")
		if st.TailcatErr != "" {
			fmt.Printf(" (tailcat: %s)", st.TailcatErr)
		}
		fmt.Println()
	}
	return nil
}

func cmdDown(ctx context.Context, args []string) error {
	f := newFlags("down", "", "Stop the daemon. Queued messages stay on disk and go out after the next start.")
	if err := f.Parse(args); err != nil {
		return err
	}
	c := f.client()
	if !c.Running() {
		fmt.Println("holler daemon is not running")
		return nil
	}
	pid := daemonPID(*f.home)
	if err := c.Call(ctx, "shutdown", nil, nil); err != nil {
		return err
	}
	// Wait for the process itself to exit, not just the socket: it holds
	// the home's lock until the end.
	for i := 0; i < 150 && (c.Running() || pid > 0 && syscall.Kill(pid, 0) == nil); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Println("holler daemon stopped")
	return nil
}

func daemonPID(home string) int {
	b, err := os.ReadFile(filepath.Join(home, "daemon.pid"))
	if err != nil {
		return 0
	}
	var pid int
	fmt.Sscan(string(b), &pid)
	return pid
}

func cmdStatus(ctx context.Context, args []string) error {
	f := newFlags("status", "", "Show this peer's identity, addresses and peers. Does not start the daemon.")
	if err := f.Parse(args); err != nil {
		return err
	}
	c := f.client()
	var st api.Status
	if err := c.Call(ctx, "status", nil, &st); err != nil {
		if *f.json {
			printJSON(map[string]any{"running": false})
		} else {
			fmt.Println("holler daemon is not running (start it with `holler up`)")
		}
		return exitCode(3)
	}
	if *f.json {
		return printJSON(st)
	}
	fmt.Printf("%s  %s\n", st.Name, st.Key)
	fmt.Printf("  pid %d, up %s, home %s\n", st.PID, time.Since(st.Started).Round(time.Second), st.Home)
	if st.Tailcat != "" {
		fmt.Printf("  address  %s\n", st.Tailcat)
	} else if st.TailcatWant {
		msg := "starting"
		if st.TailcatErr != "" {
			msg = st.TailcatErr
		}
		fmt.Printf("  tailcat  %s\n", msg)
	}
	for _, a := range st.Addresses {
		if !strings.HasPrefix(a, "tailcat:") {
			fmt.Printf("  listen   %s\n", a)
		}
	}
	if len(st.Serve) > 0 {
		fmt.Printf("  serves   %s (to peers holding a grant)\n", strings.Join(st.Serve, ", "))
	}
	fmt.Printf("  accept   %s\n", st.Accept)
	fmt.Printf("  unread %d, queued %d\n", st.Unread, st.Outbox)
	if len(st.Peers) > 0 {
		fmt.Println()
		printPeers(st.Peers)
	}
	return nil
}

func cmdAddress(ctx context.Context, args []string) error {
	f := newFlags("address", "", "Print the address to share (the tailcat address when available).")
	if err := f.Parse(args); err != nil {
		return err
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	st, err := waitAddress(ctx, c, 45*time.Second)
	if err != nil {
		return err
	}
	addr := st.ShareAddress()
	if addr == "" {
		return fmt.Errorf("no address yet (tailcat: %s)", st.TailcatErr)
	}
	fmt.Println(addr)
	return nil
}

func cmdListen(ctx context.Context, args []string) error {
	f := newFlags("listen", "", "Make sure the daemon is listening, print the address, then write every inbound\nmessage to stdout as one JSON object per line until killed. The first line is\n{\"event\":\"listening\",...}. The daemon keeps running after listen exits.")
	text := f.Bool("text", false, "human-readable output instead of NDJSON")
	mark := f.Bool("mark", false, "mark streamed messages as read")
	if err := f.Parse(args); err != nil {
		return err
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	st, err := waitAddress(ctx, c, 45*time.Second)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *text {
		fmt.Printf("listening as %s (%s)\naddress %s\n", st.Name, st.Key, st.ShareAddress())
	} else {
		printLine(map[string]any{"event": "listening", "address": st.ShareAddress(), "addresses": st.Addresses, "key": st.Key, "name": st.Name})
	}
	p := &printer{w: os.Stdout}
	err = c.Stream(ctx, "subscribe", api.SubscribeParams{Inbox: true, Mark: *mark}, func(raw json.RawMessage) error {
		if !*text {
			_, err := os.Stdout.Write(append(raw, '\n'))
			return err
		}
		var ev api.Event
		json.Unmarshal(raw, &ev)
		p.event(ev)
		return nil
	})
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func cmdConnect(ctx context.Context, args []string) error {
	f := newFlags("connect", "<address>", "Connect to a peer. The address is what the other side's `holler up` or\n`holler listen` printed (tc..., tcp:HOST:PORT or unix:/path), or the name of a\npeer you have met before. The daemon keeps the connection and reconnects\nwhenever there is unfinished business.")
	timeout := f.Duration("timeout", 60*time.Second, "how long to wait for the connection")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 1 {
		f.Usage()
		return exitCode(2)
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	var res api.ConnectResult
	err = c.Call(ctx, "connect", api.ConnectParams{Address: f.Arg(0), TimeoutMS: int(timeout.Milliseconds())}, &res)
	if err != nil {
		return err
	}
	if *f.json {
		return printJSON(res.Peer)
	}
	p := res.Peer
	fmt.Printf("connected to %s (%s) via %s\n", p.Label(), p.Key, p.Via)
	if p.About != "" {
		fmt.Printf("  about: %s\n", p.About)
	}
	return nil
}

func cmdSend(ctx context.Context, args []string) error {
	f := newFlags("send", "[<peer>] <text>...", "Send a message. Without --thread it starts a new thread (the subject defaults to\nthe first line of text). The peer can be a name, alias, key prefix or an address;\nwith --thread it can be left out. Use - as the text to read it from stdin.\nSending never fails because the peer is away: it is queued and delivered on reconnect.")
	th := f.StringP("thread", "t", "", "thread id to reply in")
	subject := f.StringP("subject", "s", "", "subject for a new thread (reads like a task title)")
	re := f.String("re", "", "id of the message this replies to")
	to := f.String("to", "", "peer (alternative to the positional argument)")
	codes := f.StringArray("code", nil, "attach a file's contents as a code part (repeatable)")
	lang := f.String("lang", "", "language for --code parts (default: from the file extension)")
	datas := f.StringArray("data", nil, "attach inline JSON as a data part (repeatable)")
	mime := f.String("mime", "application/json", "mime type for --data parts")
	files := f.StringArrayP("file", "f", nil, "attach a file as a blob (repeatable)")
	waitAck := f.Duration("wait-ack", 0, "wait up to this long for the peer to acknowledge")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.Changed("thread") && *th == "" {
		return errors.New("--thread is empty")
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	pos := f.Args()
	peer := *to
	if peer == "" && len(pos) > 0 && (*th == "" || looksLikePeer(ctx, c, pos[0]) && len(pos) > 1) {
		peer, pos = pos[0], pos[1:]
	}
	if peer == "" && *th == "" {
		return errors.New("name a peer (or --thread)")
	}
	text := strings.Join(pos, " ")
	if text == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		text = string(b)
	}
	var parts []wire.Part
	if strings.TrimSpace(text) != "" {
		parts = append(parts, wire.Part{K: wire.PartText, Text: text})
	}
	for _, path := range *codes {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		l := *lang
		if l == "" {
			l = strings.TrimPrefix(filepath.Ext(path), ".")
		}
		parts = append(parts, wire.Part{K: wire.PartCode, Lang: l, Text: string(b)})
	}
	for _, d := range *datas {
		if !json.Valid([]byte(d)) {
			return fmt.Errorf("--data is not valid JSON: %s", d)
		}
		parts = append(parts, wire.Part{K: wire.PartData, Mime: *mime, Data: json.RawMessage(d)})
	}
	var abs []string
	for _, p := range *files {
		a, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		abs = append(abs, a)
	}
	if *th != "" && !*f.json {
		unreadNote(ctx, c, peer, *th)
	}
	var res api.SendResult
	err = c.Call(ctx, "send", api.SendParams{Peer: peer, Th: *th, Subject: *subject, Re: *re, Parts: parts, Files: abs, WaitAck: int(waitAck.Milliseconds())}, &res)
	if err != nil {
		return err
	}
	if *f.json {
		return printJSON(res)
	}
	status := "delivering"
	switch {
	case res.Acked:
		status = "acknowledged"
	case !res.Connected:
		status = "queued until the peer is reachable"
	case *waitAck > 0:
		status = "not acknowledged yet"
	}
	fmt.Printf("sent %s to %s in %s (%s)\n", res.ID, res.PeerName, res.Th, status)
	return nil
}

// looksLikePeer reports whether s names a known peer or is an address.
func looksLikePeer(ctx context.Context, c interface {
	Call(context.Context, string, any, any) error
}, s string) bool {
	if _, err := transport.Parse(s); err == nil {
		return true
	}
	var peers []api.PeerView
	if c.Call(ctx, "peers", nil, &peers) != nil {
		return false
	}
	for _, p := range peers {
		body := strings.TrimPrefix(p.Key, wire.KeyPrefix)
		if s == p.Key || s == p.Name || s == p.Alias || len(s) >= 4 && strings.HasPrefix(body, s) {
			return true
		}
	}
	return false
}

// unreadNote tells the agent, on stderr, when it is about to reply in a
// thread that still has unread messages from the peer. The send goes ahead.
func unreadNote(ctx context.Context, c interface {
	Call(context.Context, string, any, any) error
}, peer, th string) {
	var threads []*store.Thread
	if c.Call(ctx, "threads", api.PeerParams{Peer: peer}, &threads) != nil {
		return
	}
	var peers []api.PeerView
	c.Call(ctx, "peers", nil, &peers)
	if s := unreadLine(threads, peers, th); s != "" {
		fmt.Fprintln(os.Stderr, s)
	}
}

// unreadLine is the note for th, or "" when nothing there is unread.
func unreadLine(threads []*store.Thread, peers []api.PeerView, th string) string {
	for _, t := range threads {
		if t.Th != th || t.Unread == 0 {
			continue
		}
		name := wire.ShortKey(t.Peer)
		for _, p := range peers {
			if p.Key == t.Peer {
				name = p.Label()
			}
		}
		return fmt.Sprintf("note: %d unread from %s in this thread (%s): holler read %s", t.Unread, name, ago(t.Updated), t.Th)
	}
	return ""
}

func cmdState(ctx context.Context, args []string) error {
	f := newFlags("state", "[<peer>] <thread> <state>", "Tell the peer your view of a thread: open, working, waiting, done, failed or\nclosed (other words are allowed). The peer can be left out when the thread id\nis unique.")
	note := f.StringP("note", "n", "", "short note, e.g. what you are doing or why it failed")
	if err := f.Parse(args); err != nil {
		return err
	}
	var p api.StateParams
	switch f.NArg() {
	case 2:
		p = api.StateParams{Th: f.Arg(0), State: f.Arg(1)}
	case 3:
		p = api.StateParams{Peer: f.Arg(0), Th: f.Arg(1), State: f.Arg(2)}
	default:
		f.Usage()
		return exitCode(2)
	}
	p.Note = *note
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	if !*f.json {
		unreadNote(ctx, c, p.Peer, p.Th)
	}
	var res api.SendResult
	if err := c.Call(ctx, "state", p, &res); err != nil {
		return err
	}
	if *f.json {
		return printJSON(res)
	}
	fmt.Printf("you are now %s on %s (told %s)\n", p.State, res.Th, res.PeerName)
	return nil
}

func cmdTail(ctx context.Context, args []string) error {
	f := newFlags("tail", "", "Print inbound messages. With --once, print the unread ones, mark them read and\nexit (what an agent calls at natural checkpoints). Otherwise follow new\nmessages as they arrive until interrupted.")
	once := f.Bool("once", false, "print unread messages and exit")
	th := f.StringP("thread", "t", "", "only this thread")
	peer := f.StringP("peer", "p", "", "only this peer")
	all := f.Bool("all", false, "include sent messages and connection events")
	mark := f.Bool("mark", false, "when following, mark printed messages read")
	if err := f.Parse(args); err != nil {
		return err
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	p := &printer{w: os.Stdout, showThread: *th == ""}
	if *once {
		var res api.ReadResult
		if err := c.Call(ctx, "read", api.ReadParams{Peer: *peer, Th: *th, Inbox: !*all, Unread: true, Mark: true, Limit: 1000}, &res); err != nil {
			return err
		}
		if *f.json {
			for _, ev := range res.Events {
				printLine(ev)
			}
			return nil
		}
		if len(res.Events) == 0 {
			fmt.Println("no unread messages")
			return nil
		}
		for _, ev := range res.Events {
			p.event(ev)
		}
		return nil
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = c.Stream(ctx, "subscribe", api.SubscribeParams{Peer: *peer, Th: *th, Inbox: !*all, Mark: *mark}, func(raw json.RawMessage) error {
		if *f.json {
			_, err := os.Stdout.Write(append(raw, '\n'))
			return err
		}
		var ev api.Event
		json.Unmarshal(raw, &ev)
		p.event(ev)
		return nil
	})
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func cmdWait(ctx context.Context, args []string) error {
	f := newFlags("wait", "", "Block until unread messages arrive (optionally in one thread or from one peer),\nprint them and mark them read. With --state, wait until the peer's state on the\nthread is one of the given states. Exits 0 when something arrived, 2 on timeout.")
	th := f.StringP("thread", "t", "", "only this thread")
	peer := f.StringP("peer", "p", "", "only this peer")
	states := f.StringSlice("state", nil, "wait for the peer's state on --thread to be one of these (e.g. done,failed)")
	timeout := f.Duration("timeout", 5*time.Minute, "give up after this long")
	if err := f.Parse(args); err != nil {
		return err
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	var res api.ReadResult
	err = c.Call(ctx, "wait", api.WaitParams{Peer: *peer, Th: *th, States: *states, TimeoutMS: int(timeout.Milliseconds())}, &res)
	if err != nil {
		return err
	}
	if *f.json {
		printJSON(res)
	} else {
		p := &printer{w: os.Stdout, showThread: *th == ""}
		for _, ev := range res.Events {
			p.event(ev)
		}
		if res.Thread != nil && len(*states) > 0 && !res.TimedOut {
			who := "the peer"
			if len(res.Events) > 0 {
				who = res.Events[0].PeerName
			}
			fmt.Printf("%s is %s on %s", who, res.Thread.TheirState, res.Thread.Th)
			if res.Thread.TheirNote != "" {
				fmt.Printf(" (%s)", res.Thread.TheirNote)
			}
			fmt.Println()
		}
		if res.TimedOut {
			fmt.Printf("nothing new after %v\n", *timeout)
		}
	}
	if res.TimedOut {
		return exitCode(2)
	}
	return nil
}

func cmdRead(ctx context.Context, args []string) error {
	f := newFlags("read", "[<thread>]", "Show a conversation, both directions, oldest first: one thread, one peer\n(--peer), or everything recent. Marks what it shows as read.")
	peer := f.StringP("peer", "p", "", "only this peer")
	last := f.IntP("last", "n", 50, "how many records")
	if err := f.Parse(args); err != nil {
		return err
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	th := f.Arg(0)
	var res api.ReadResult
	if err := c.Call(ctx, "read", api.ReadParams{Peer: *peer, Th: th, Limit: *last, Mark: true}, &res); err != nil {
		return err
	}
	if *f.json {
		return printJSON(res)
	}
	if res.Thread != nil {
		t := res.Thread
		fmt.Printf("%s  %q  you: %s  them: %s\n\n", t.Th, t.Subject, stateNote(t.MyState, t.MyNote), stateNote(t.TheirState, t.TheirNote))
	}
	p := &printer{w: os.Stdout, showThread: th == ""}
	for _, ev := range res.Events {
		p.event(ev)
	}
	if len(res.Events) == 0 {
		fmt.Println("nothing yet")
	}
	return nil
}

func stateNote(state, note string) string {
	if note != "" {
		return state + " (" + note + ")"
	}
	return state
}

func cmdThreads(ctx context.Context, args []string) error {
	f := newFlags("threads", "", "List threads, most recently active first.")
	peer := f.StringP("peer", "p", "", "only this peer")
	open := f.Bool("open", false, "only threads not done, failed or closed")
	if err := f.Parse(args); err != nil {
		return err
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	var threads []*store.Thread
	if err := c.Call(ctx, "threads", api.PeerParams{Peer: *peer}, &threads); err != nil {
		return err
	}
	var peers []api.PeerView
	c.Call(ctx, "peers", nil, &peers)
	names := map[string]string{}
	for _, p := range peers {
		names[p.Key] = p.Label()
	}
	var out []*store.Thread
	for _, t := range threads {
		if !*open || t.Open() {
			out = append(out, t)
		}
	}
	if *f.json {
		return printJSON(out)
	}
	if len(out) == 0 {
		fmt.Println("no threads")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "THREAD\tPEER\tSUBJECT\tYOU\tTHEM\tUNREAD\tUPDATED")
	for _, t := range out {
		name := names[t.Peer]
		if name == "" {
			name = wire.ShortKey(t.Peer)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", t.Th, name, clip(t.Subject, 48), t.MyState, t.TheirState, t.Unread, ago(t.Updated))
	}
	return tw.Flush()
}

func cmdPeers(ctx context.Context, args []string) error {
	f := newFlags("peers", "", "List known peers: connection state, queued messages, open threads, unread\nmessages and the capabilities they hold on you.")
	if err := f.Parse(args); err != nil {
		return err
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	var peers []api.PeerView
	if err := c.Call(ctx, "peers", nil, &peers); err != nil {
		return err
	}
	if *f.json {
		return printJSON(peers)
	}
	if len(peers) == 0 {
		fmt.Println("no peers yet (connect with `holler connect <address>`)")
		return nil
	}
	printPeers(peers)
	return nil
}

func printPeers(peers []api.PeerView) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PEER\tKEY\tSTATE\tQUEUED\tOPEN\tUNREAD\tGRANTED")
	for _, p := range peers {
		state := "offline"
		switch {
		case p.Connected && p.Outbound:
			state = "connected (out)"
		case p.Connected:
			state = "connected (in)"
		case p.Dialing:
			state = "reconnecting"
		case p.Parked:
			state = "said bye"
		}
		granted := strings.Join(p.Granted, ",")
		if granted == "" {
			granted = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n", p.Label(), p.Short, state, p.Outbox, p.OpenThreads, p.Unread, granted)
	}
	tw.Flush()
}

func cmdGrant(ctx context.Context, args []string) error {
	f := newFlags("grant", "<peer> <cap>...", "Grant capabilities to a peer (section 10 of the spec): exec, fs:read, fs:write,\nintroduce, admin. exec and fs:write are remote code execution; grant them only\nwhen your user has explicitly agreed, and keep the ttl short.")
	ttl := f.Duration("ttl", time.Hour, "how long the grant is valid")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() < 2 {
		f.Usage()
		return exitCode(2)
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	var g store.GrantRow
	if err := c.Call(ctx, "grant", api.GrantParams{Peer: f.Arg(0), Caps: f.Args()[1:], TTL: ttl.String()}, &g); err != nil {
		return err
	}
	if *f.json {
		return printJSON(g)
	}
	fmt.Printf("granted %s to %s until %s (grant %s)\n", strings.Join(g.Caps, ", "), wire.ShortKey(g.Sub), g.Exp.Local().Format("15:04:05"), g.Hash)
	return nil
}

func cmdGrants(ctx context.Context, args []string) error {
	f := newFlags("grants", "", "List grants: issued (by you), held (granted to you) and presented (shown to\nyou by peers about themselves).")
	if err := f.Parse(args); err != nil {
		return err
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	var gs []*store.GrantRow
	if err := c.Call(ctx, "grants", nil, &gs); err != nil {
		return err
	}
	if *f.json {
		return printJSON(gs)
	}
	if len(gs) == 0 {
		fmt.Println("no grants")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "HASH\tROLE\tISSUER\tSUBJECT\tCAPS\tEXPIRES")
	for _, g := range gs {
		exp := g.Exp.Local().Format("Jan 2 15:04")
		if time.Now().After(g.Exp) {
			exp += " (expired)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", g.Hash, g.Role, wire.ShortKey(g.Iss), wire.ShortKey(g.Sub), strings.Join(g.Caps, ","), exp)
	}
	return tw.Flush()
}

func cmdRevoke(ctx context.Context, args []string) error {
	f := newFlags("revoke", "<hash>", "Stop honoring a grant (see `holler grants`). The peer may still hold a copy,\nwhich other peers that trust you would honor until it expires.")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 1 {
		f.Usage()
		return exitCode(2)
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	if err := c.Call(ctx, "revoke", api.PeerParams{Hash: f.Arg(0)}, nil); err != nil {
		return err
	}
	fmt.Println("revoked", f.Arg(0))
	return nil
}

func cmdIntroduce(ctx context.Context, args []string) error {
	f := newFlags("introduce", "<to> <peer> [<cap>...]", "Send <to> the key and address of <peer>, with a grant <peer> will honor if it\ntrusts you with introduce (it granted you `introduce`). The grant is bound to\n<peer> and gives <to> nothing on you.")
	ttl := f.Duration("ttl", time.Hour, "how long the introduction grant is valid")
	th := f.StringP("thread", "t", "", "thread to send it in (queues it if not connected)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() < 2 {
		f.Usage()
		return exitCode(2)
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	var res api.SendResult
	if err := c.Call(ctx, "introduce", api.IntroduceParams{To: f.Arg(0), Peer: f.Arg(1), Caps: f.Args()[2:], TTL: ttl.String(), Th: *th}, &res); err != nil {
		return err
	}
	if *f.json {
		return printJSON(res)
	}
	fmt.Printf("introduced %s to %s\n", f.Arg(1), res.PeerName)
	return nil
}

func cmdBye(ctx context.Context, args []string) error {
	f := newFlags("bye", "<peer>", "Close the connection gracefully. The peer is not reconnected to until you send\nit something new.")
	reason := f.String("reason", "done", "reason sent in bye")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 1 {
		f.Usage()
		return exitCode(2)
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	if err := c.Call(ctx, "bye", api.PeerParams{Peer: f.Arg(0), Reason: *reason}, nil); err != nil {
		return err
	}
	fmt.Println("said bye to", f.Arg(0))
	return nil
}

func cmdAlias(ctx context.Context, args []string) error {
	f := newFlags("alias", "<peer> <alias>", "Give a peer a local nickname usable anywhere a peer is expected.")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 2 {
		f.Usage()
		return exitCode(2)
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	return c.Call(ctx, "alias", api.PeerParams{Peer: f.Arg(0), Alias: f.Arg(1)}, nil)
}

func cmdBlobs(ctx context.Context, args []string) error {
	f := newFlags("blobs", "", "List blobs (file attachments) sent and received, with local paths.")
	peer := f.StringP("peer", "p", "", "only this peer")
	if err := f.Parse(args); err != nil {
		return err
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	var bs []*store.Blob
	if err := c.Call(ctx, "blobs", api.PeerParams{Peer: *peer}, &bs); err != nil {
		return err
	}
	if *f.json {
		return printJSON(bs)
	}
	if len(bs) == 0 {
		fmt.Println("no blobs")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "DIR\tNAME\tSIZE\tSTATUS\tTHREAD\tPATH")
	for _, b := range bs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", b.Dir, b.Name, humanSize(b.Size), b.Status, b.Th, b.Path)
	}
	return tw.Flush()
}

func cmdMCP(ctx context.Context, args []string) error {
	f := newFlags("mcp", "", "Serve holler as an MCP server on stdin/stdout. Tools: holler_listen,\nholler_connect, holler_send, holler_read, holler_state, holler_grant,\nholler_status. Inbound messages are pushed as Claude Code channel\nnotifications when the client registers for them (or with --channel).")
	channel := f.Bool("channel", os.Getenv("HOLLER_CHANNEL") == "1", "always push inbound messages as channel notifications")
	harnessFlag := f.String("harness", "", "agent harness this server runs in, for a daemon it starts (holler bootstrap sets it)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if err := setHarnessEnv(*harnessFlag); err != nil {
		return err
	}
	s := &mcp.Server{Ensure: f.ensureDaemon, Channel: *channel}
	return s.Run(ctx, os.Stdin, os.Stdout)
}

// setHarnessEnv passes --harness to a daemon this command starts.
func setHarnessEnv(v string) error {
	if v == "" {
		return nil
	}
	id := harness.Normalize(v)
	if id == "" {
		return fmt.Errorf("unknown harness %q (known: %s)", v, strings.Join(harness.IDs(), ", "))
	}
	return os.Setenv("HOLLER_HARNESS", id)
}

func cmdModel(ctx context.Context, args []string) error {
	f := newFlags("model", "[<model>]", "Show or set the model this agent runs on, which it shares with the network\n(holler web shows it). It takes effect at once, so run it\nagain after switching models. Claude Code, Cursor and opencode report the\nmodel through holler's hooks and plugin, so they rarely need this.")
	if err := f.Parse(args); err != nil {
		return err
	}
	// Harness plugins call this on every turn: it must never start holler.
	c := f.client()
	if !c.Running() {
		return errors.New("holler is not running (holler up starts it)")
	}
	if f.NArg() == 0 {
		var st api.Status
		if err := c.Call(ctx, "status", nil, &st); err != nil {
			return err
		}
		if *f.json {
			return printJSON(map[string]string{"model": st.Model})
		}
		if st.Model == "" {
			fmt.Println("no model reported yet (holler model <id> sets it)")
			return nil
		}
		fmt.Println(st.Model)
		return nil
	}
	var res api.ModelResult
	if err := c.Call(ctx, "set_model", api.ModelParams{Model: strings.Join(f.Args(), " ")}, &res); err != nil {
		return err
	}
	if *f.json {
		return printJSON(res)
	}
	fmt.Printf("model: %s\n", res.Model)
	return nil
}

func cmdShare(ctx context.Context, args []string) error {
	f := newFlags("share", "[<host>...]", "Show or set the hosts this agent mirrors its conversations to, so a dashboard\nthere (holler web) can show them. The other party of each thread is told, and\ncan keep a thread out with holler private. With hosts, replaces the list;\nwith --stop, stops sharing. Hosts are names, aliases or keys of peers.")
	stop := f.Bool("stop", false, "stop sharing")
	if err := f.Parse(args); err != nil {
		return err
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	var refs []api.PeerRef
	switch {
	case *stop:
		err = c.Call(ctx, "set_share", api.ShareParams{With: []string{}}, &refs)
	case f.NArg() > 0:
		err = c.Call(ctx, "set_share", api.ShareParams{With: f.Args()}, &refs)
	default:
		var st api.Status
		err = c.Call(ctx, "status", nil, &st)
		refs = st.ShareWith
	}
	if err != nil {
		return err
	}
	if *f.json {
		return printJSON(refs)
	}
	if len(refs) == 0 {
		fmt.Println("not sharing conversations")
		return nil
	}
	fmt.Println("sharing conversations with:")
	for _, r := range refs {
		name := r.Name
		if name == "" {
			name = wire.ShortKey(r.Key)
		}
		fmt.Printf("  %s  %s\n", name, r.Key)
	}
	return nil
}

func cmdPrivate(ctx context.Context, args []string) error {
	f := newFlags("private", "[<peer>] <thread>", "Keep a thread out of conversation sharing, on both sides: this agent stops\nmirroring it, the other agent is asked to stop too, and hosts that have a copy\nforget it. The peer can be left out when the thread id is unique.")
	if err := f.Parse(args); err != nil {
		return err
	}
	var peer, th string
	switch f.NArg() {
	case 1:
		th = f.Arg(0)
	case 2:
		peer, th = f.Arg(0), f.Arg(1)
	default:
		f.Usage()
		return exitCode(2)
	}
	c, err := f.ensureDaemon()
	if err != nil {
		return err
	}
	if err := c.Call(ctx, "private", api.PrivateParams{Peer: peer, Th: th}, nil); err != nil {
		return err
	}
	fmt.Printf("thread %s is private: it is not shared, and hosts that had a copy are asked to forget it\n", th)
	return nil
}
