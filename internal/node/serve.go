package node

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agentwireprotocol/awp/wire"
)

// Result mime types for served requests (PROFILE.md).
const (
	MimeExecResult    = "application/vnd.awp.exec-result+json"
	MimeFSReadResult  = "application/vnd.awp.fs-read-result+json"
	MimeFSWriteResult = "application/vnd.awp.fs-write-result+json"
)

const inlineLimit = 64 << 10

// ExecRequest is the data of an application/vnd.awp.exec+json part.
type ExecRequest struct {
	Cmd     json.RawMessage   `json:"cmd"` // ["argv", ...] or "shell string"
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Timeout float64           `json:"timeout,omitempty"` // seconds, default 600
	Stdin   string            `json:"stdin,omitempty"`
}

// FSReadRequest is the data of an application/vnd.awp.fs-read+json part.
type FSReadRequest struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset,omitempty"`
	Length int64  `json:"length,omitempty"`
}

// FSWriteRequest is the data of an application/vnd.awp.fs-write+json part.
type FSWriteRequest struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding,omitempty"` // "utf-8" (default) or "base64"
	Append   bool   `json:"append,omitempty"`
	Mkdir    bool   `json:"mkdir,omitempty"`
}

// serveRequests fulfils allowed requests for capabilities listed in
// Policy.Serve, each in its own goroutine, and replies in the same thread.
func (n *Node) serveRequests(peer string, m *wire.Msg, reqs []Request) {
	for _, r := range reqs {
		if !r.Allowed || !slices.Contains(n.cfg.Policy.Serve, r.Cap) {
			continue
		}
		part := m.Parts[r.Part]
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			reply := n.serveOne(r.Cap, part.Data)
			reply.Peer, reply.Th, reply.Re = peer, m.Th, m.ID
			if _, err := n.Send(reply); err != nil {
				n.logf("reply to %s request %s: %v", r.Cap, m.ID, err)
			}
		}()
	}
}

func (n *Node) serveOne(cap string, data json.RawMessage) SendRequest {
	n.logf("serving %s request: %s", cap, abbrevJSON(data))
	switch cap {
	case CapExec:
		return n.serveExec(data)
	case CapFSRead:
		return n.serveFSRead(data)
	case CapFSWrite:
		return n.serveFSWrite(data)
	}
	return errorReply(cap, "", errors.New("not served here"))
}

func errorReply(cap, mime string, err error) SendRequest {
	if mime == "" {
		mime = "application/json"
	}
	d, _ := json.Marshal(map[string]string{"error": err.Error()})
	return SendRequest{Parts: []wire.Part{
		{K: wire.PartText, Text: fmt.Sprintf("%s failed: %v", cap, err)},
		{K: wire.PartData, Mime: mime, Data: d},
	}}
}

func (n *Node) rootDir() string {
	if n.cfg.Policy.Root != "" {
		return n.cfg.Policy.Root
	}
	wd, _ := os.Getwd()
	return wd
}

func (n *Node) serveExec(data json.RawMessage) SendRequest {
	var req ExecRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return errorReply(CapExec, MimeExecResult, err)
	}
	var argv []string
	var shell string
	if err := json.Unmarshal(req.Cmd, &argv); err != nil || len(argv) == 0 {
		if err := json.Unmarshal(req.Cmd, &shell); err != nil || shell == "" {
			return errorReply(CapExec, MimeExecResult, errors.New(`cmd must be ["argv", ...] or a shell string`))
		}
		argv = []string{"/bin/sh", "-c", shell}
	}
	timeout := time.Duration(req.Timeout * float64(time.Second))
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	root := n.rootDir()
	dir := root
	if req.Cwd != "" {
		dir = filepath.Join(root, req.Cwd)
		if filepath.IsAbs(req.Cwd) {
			dir = filepath.Clean(req.Cwd)
		}
	}
	ctx, cancel := context.WithTimeout(n.ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	runErr := cmd.Run()
	dur := time.Since(start)

	res := map[string]any{"exit": -1, "duration_ms": dur.Milliseconds()}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		res["exit"] = 0
	case errors.As(runErr, &exitErr):
		res["exit"] = exitErr.ExitCode()
	default:
		res["error"] = runErr.Error()
	}
	if ctx.Err() == context.DeadlineExceeded {
		res["error"] = fmt.Sprintf("timed out after %v", timeout)
	}
	reply := SendRequest{}
	var files []string
	for _, stream := range []struct {
		name string
		buf  *bytes.Buffer
	}{{"stdout", &stdout}, {"stderr", &stderr}} {
		b := stream.buf.Bytes()
		if len(b) <= inlineLimit && utf8.Valid(b) {
			res[stream.name] = string(b)
			continue
		}
		res[stream.name] = strings.ToValidUTF8(string(b[:min(len(b), inlineLimit)]), "�")
		res[stream.name+"_truncated"] = true
		if f, err := n.spool(stream.name+".txt", b); err == nil {
			files = append(files, f)
		}
	}
	d, _ := json.Marshal(res)
	summary := fmt.Sprintf("exit %v in %.1fs: %s", res["exit"], dur.Seconds(), strings.Join(argv, " "))
	if e, ok := res["error"]; ok {
		summary += fmt.Sprintf(" (%v)", e)
	}
	reply.Parts = []wire.Part{{K: wire.PartText, Text: summary}, {K: wire.PartData, Mime: MimeExecResult, Data: d}}
	reply.Files = files
	reply.cleanup = files
	return reply
}

// spool writes bytes to a temporary file under Home so they can be sent as
// a blob.
func (n *Node) spool(name string, b []byte) (string, error) {
	dir := filepath.Join(n.cfg.Home, "spool", n.ids.New())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	return path, os.WriteFile(path, b, 0o600)
}

// openRoot confines file access to the configured root.
func (n *Node) openRoot() (*os.Root, error) {
	return os.OpenRoot(n.rootDir())
}

// relPath makes a request path relative to the root; absolute paths must
// already be inside it.
func (n *Node) relPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(p) {
		rel, err := filepath.Rel(n.rootDir(), p)
		if err != nil || strings.HasPrefix(rel, "..") {
			return "", fmt.Errorf("%s is outside the served root", p)
		}
		return rel, nil
	}
	return p, nil
}

func (n *Node) serveFSRead(data json.RawMessage) SendRequest {
	var req FSReadRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return errorReply(CapFSRead, MimeFSReadResult, err)
	}
	rel, err := n.relPath(req.Path)
	if err != nil {
		return errorReply(CapFSRead, MimeFSReadResult, err)
	}
	root, err := n.openRoot()
	if err != nil {
		return errorReply(CapFSRead, MimeFSReadResult, err)
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		return errorReply(CapFSRead, MimeFSReadResult, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return errorReply(CapFSRead, MimeFSReadResult, err)
	}
	if fi.IsDir() {
		ents, err := f.ReadDir(-1)
		if err != nil {
			return errorReply(CapFSRead, MimeFSReadResult, err)
		}
		var names []string
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			names = append(names, name)
		}
		d, _ := json.Marshal(map[string]any{"path": req.Path, "dir": true, "entries": names})
		return SendRequest{Parts: []wire.Part{{K: wire.PartText, Text: fmt.Sprintf("%s: %d entries", req.Path, len(names))}, {K: wire.PartData, Mime: MimeFSReadResult, Data: d}}}
	}
	length := req.Length
	if length <= 0 || length > n.cfg.BlobLimit {
		length = n.cfg.BlobLimit
	}
	b, err := io.ReadAll(io.LimitReader(io.NewSectionReader(f, req.Offset, fi.Size()-min(req.Offset, fi.Size())), length))
	if err != nil {
		return errorReply(CapFSRead, MimeFSReadResult, err)
	}
	res := map[string]any{"path": req.Path, "size": fi.Size(), "offset": req.Offset, "length": len(b), "mode": fi.Mode().String()}
	reply := SendRequest{}
	if len(b) <= inlineLimit && utf8.Valid(b) {
		res["encoding"], res["content"] = "utf-8", string(b)
	} else if len(b) <= inlineLimit {
		res["encoding"], res["content"] = "base64", base64.StdEncoding.EncodeToString(b)
	} else if path, err := n.spool(filepath.Base(rel), b); err == nil {
		res["blob"] = filepath.Base(rel)
		reply.Files, reply.cleanup = []string{path}, []string{path}
	} else {
		return errorReply(CapFSRead, MimeFSReadResult, err)
	}
	d, _ := json.Marshal(res)
	reply.Parts = []wire.Part{{K: wire.PartText, Text: fmt.Sprintf("%s (%d bytes)", req.Path, len(b))}, {K: wire.PartData, Mime: MimeFSReadResult, Data: d}}
	return reply
}

func (n *Node) serveFSWrite(data json.RawMessage) SendRequest {
	var req FSWriteRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return errorReply(CapFSWrite, MimeFSWriteResult, err)
	}
	rel, err := n.relPath(req.Path)
	if err != nil {
		return errorReply(CapFSWrite, MimeFSWriteResult, err)
	}
	content := []byte(req.Content)
	if req.Encoding == "base64" {
		if content, err = wire.DecodeB64(req.Content); err != nil {
			return errorReply(CapFSWrite, MimeFSWriteResult, err)
		}
	}
	root, err := n.openRoot()
	if err != nil {
		return errorReply(CapFSWrite, MimeFSWriteResult, err)
	}
	defer root.Close()
	if req.Mkdir {
		if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
			return errorReply(CapFSWrite, MimeFSWriteResult, err)
		}
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if req.Append {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	f, err := root.OpenFile(rel, flags, 0o644)
	if err != nil {
		return errorReply(CapFSWrite, MimeFSWriteResult, err)
	}
	k, err := f.Write(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return errorReply(CapFSWrite, MimeFSWriteResult, err)
	}
	d, _ := json.Marshal(map[string]any{"path": req.Path, "written": k, "append": req.Append})
	return SendRequest{Parts: []wire.Part{{K: wire.PartText, Text: fmt.Sprintf("wrote %d bytes to %s", k, req.Path)}, {K: wire.PartData, Mime: MimeFSWriteResult, Data: d}}}
}

func abbrevJSON(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
