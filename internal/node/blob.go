package node

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentwireprotocol/awp/internal/store"
	"github.com/agentwireprotocol/awp/wire"
)

// blobPath is where a received blob is written.
func (n *Node) blobPath(peer, ref string) string {
	return filepath.Join(n.cfg.Home, "blobs", wire.ShortKey(peer), safeName(ref))
}

// safeName turns a sender-chosen ref into a file name that cannot escape
// the blob directory.
func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	name := b.String()
	if len(name) > 120 {
		name = name[:120]
	}
	if name == "" || strings.HasPrefix(name, ".") {
		name = "b_" + name
	}
	return name
}

// queueBlob chunks a file into the outbox (inside the caller's transaction)
// and returns the blob part that refers to it. Chunks go ahead of the msg
// that references them, so the msg's ack also retires them.
func (n *Node) queueBlob(q store.Q, peer, th, path string, now time.Time) (wire.Part, error) {
	f, err := os.Open(path)
	if err != nil {
		return wire.Part{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return wire.Part{}, err
	}
	if fi.IsDir() {
		return wire.Part{}, fmt.Errorf("%s is a directory", path)
	}
	if fi.Size() > n.cfg.BlobLimit {
		return wire.Part{}, fmt.Errorf("%s is %d bytes; the blob limit is %d", path, fi.Size(), n.cfg.BlobLimit)
	}
	ref := "b_" + n.ids.New()
	name := filepath.Base(path)
	mt := detectMime(f, name)

	ts := wire.FormatTime(now)
	cur := make([]byte, wire.ChunkSize)
	next := make([]byte, wire.ChunkSize)
	k, err := io.ReadFull(f, cur)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return wire.Part{}, err
	}
	var total int64
	for i := 0; ; i++ {
		var k2 int
		last := k < len(cur)
		if !last {
			k2, err = io.ReadFull(f, next)
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				return wire.Part{}, err
			}
			last = k2 == 0
		}
		ch := wire.Chunk{
			Envelope: wire.Envelope{T: wire.TChunk, ID: n.ids.New(), TS: ts, Th: th},
			Ref:      ref,
			N:        i,
			Last:     last,
			Data:     base64.StdEncoding.EncodeToString(cur[:k]),
		}
		line, err := wire.Encode(ch)
		if err != nil {
			return wire.Part{}, err
		}
		if err := store.Enqueue(q, &store.OutboxRow{Peer: peer, ID: ch.ID, T: wire.TChunk, Th: th, Ref: ref, Line: line, Created: now}); err != nil {
			return wire.Part{}, err
		}
		total += int64(k)
		if total > n.cfg.BlobLimit {
			return wire.Part{}, fmt.Errorf("%s grew past the blob limit while reading", path)
		}
		if last {
			break
		}
		cur, next = next, cur
		k = k2
	}
	abs, _ := filepath.Abs(path)
	if err := store.PutBlob(q, &store.Blob{Peer: peer, Dir: "out", Ref: ref, Th: th, Name: name, Mime: mt, Size: total, Received: total, Path: abs, Status: "queued", Updated: now}); err != nil {
		return wire.Part{}, err
	}
	return wire.Part{K: wire.PartBlob, Ref: ref, Name: name, Mime: mt, Size: total}, nil
}

func detectMime(f *os.File, name string) string {
	if mt := mime.TypeByExtension(filepath.Ext(name)); mt != "" {
		return mt
	}
	buf := make([]byte, 512)
	k, _ := f.ReadAt(buf, 0)
	return http.DetectContentType(buf[:k])
}

// noteInboundBlob records what a msg says about a blob it references. It
// reports whether the blob is refused for being over the limit, and returns
// the blob if its data had already arrived (chunks usually come first).
func (n *Node) noteInboundBlob(q store.Q, peer, th string, p wire.Part, now time.Time) (bool, *store.Blob, error) {
	b, err := store.GetBlob(q, peer, "in", p.Ref)
	if err != nil {
		return false, nil, err
	}
	if b == nil {
		b = &store.Blob{Peer: peer, Dir: "in", Ref: p.Ref, Size: -1, Status: "pending", Path: n.blobPath(peer, p.Ref)}
	}
	if b.Th == "" {
		b.Th = th
	}
	b.Name, b.Mime, b.Size, b.Updated = p.Name, p.Mime, p.Size, now
	refused := false
	if b.Status == "pending" && p.Size > n.cfg.BlobLimit {
		b.Status = "refused"
		os.Remove(b.Path)
		refused = true
	}
	var done *store.Blob
	if b.Status == "received" {
		// Its data came first; now it has a name, it takes its final path
		// and is complete, in this one transaction.
		b.Status = "complete"
		n.placeBlob(b)
		cp := *b
		done = &cp
	}
	return refused, done, store.PutBlob(q, b)
}

// placeBlob moves a complete blob whose name is known to
// blobs/<peer>/<ref>.d/<name>, so the path the agent sees ends in the name
// the sender gave it. It runs inside the transaction that makes the blob
// both complete and named, so no reader ever sees a path that is about to
// change. If that transaction fails, unplaceBlob puts the file back.
func (n *Node) placeBlob(b *store.Blob) {
	src := n.blobPath(b.Peer, b.Ref)
	if b.Status != "complete" || b.Name == "" || b.Path != src {
		return
	}
	dst := filepath.Join(filepath.Dir(src), safeName(b.Ref)+".d", safeName(b.Name))
	if os.MkdirAll(filepath.Dir(dst), 0o700) == nil && os.Rename(src, dst) == nil {
		b.Path = dst
	}
}

func (n *Node) unplaceBlob(b *store.Blob) {
	if src := n.blobPath(b.Peer, b.Ref); b.Path != src {
		os.Rename(b.Path, src)
	}
}

// blobEvent announces a received blob once both its data and the msg that
// names it have arrived, whichever came last.
func (n *Node) blobEvent(b *store.Blob) {
	n.sysEvent(b.Peer, "blob", map[string]any{"ref": b.Ref, "th": b.Th, "name": b.Name, "mime": b.Mime, "size": b.Received, "path": b.Path})
}

// onChunk appends a chunk to its blob. Writes go to the offset recorded in
// the database, after truncating to it, so a crash between the file write
// and the commit cannot duplicate data when the chunk is replayed.
func (n *Node) onChunk(c *Conn, line []byte) error {
	var ch wire.Chunk
	if err := wire.Decode(line, &ch); err != nil {
		return c.fatal(wire.ErrBadFrame, "", "malformed chunk: "+err.Error())
	}
	if ch.ID == "" || ch.Ref == "" {
		return c.fatal(wire.ErrBadFrame, ch.ID, "chunk needs id and ref")
	}
	now := time.Now()
	var completed *store.Blob
	var refuse string
	err := n.st.Tx(func(q store.Q) error {
		b, err := store.GetBlob(q, c.peerKey, "in", ch.Ref)
		if err != nil {
			return err
		}
		if b == nil {
			b = &store.Blob{Peer: c.peerKey, Dir: "in", Ref: ch.Ref, Th: ch.Th, Size: -1, Status: "pending", Path: n.blobPath(c.peerKey, ch.Ref)}
		}
		if b.Th == "" {
			b.Th = ch.Th
		}
		bumpSeen := func() error { return store.BumpSeen(q, c.peerKey, ch.Th, ch.ID, now) }
		if b.Status != "pending" || ch.N < b.NextN {
			return bumpSeen() // refused, finished, or a replayed chunk
		}
		if ch.N > b.NextN {
			refuse = fmt.Sprintf("chunk %d arrived but %d was expected", ch.N, b.NextN)
			return nil
		}
		data, err := wire.DecodeB64(ch.Data)
		if err != nil {
			refuse = "chunk data is not base64"
			return nil
		}
		if b.Received+int64(len(data)) > n.cfg.BlobLimit {
			refuse = fmt.Sprintf("blob exceeds this peer's %d byte limit", n.cfg.BlobLimit)
			return nil
		}
		if b.Size >= 0 && b.Received+int64(len(data)) > b.Size {
			refuse = fmt.Sprintf("blob is larger than its declared size %d", b.Size)
			return nil
		}
		if err := writeAt(b.Path, b.Received, data); err != nil {
			return err
		}
		b.Received += int64(len(data))
		b.NextN++
		b.Updated = now
		if ch.Last {
			// Complete only once named and in its final place; until the
			// msg that names it arrives, the data is just "received", so no
			// reader sees a complete blob at a path that is about to change.
			b.Status = "received"
			if b.Name != "" {
				b.Status = "complete"
				n.placeBlob(b)
			}
			done := *b
			completed = &done
		}
		if err := store.PutBlob(q, b); err != nil {
			return err
		}
		return bumpSeen()
	})
	if err != nil {
		if completed != nil {
			n.unplaceBlob(completed)
		}
		return n.storageFailed(c, ch.ID, err)
	}
	if refuse != "" {
		n.refuseBlob(c, ch.Ref, refuse)
		return nil
	}
	if completed != nil && completed.Status == "complete" {
		n.blobEvent(completed) // the msg came first; otherwise onMsg announces it
	}
	return nil
}

func (n *Node) refuseBlob(c *Conn, ref, reason string) {
	n.logf("refusing blob %s from %s: %s", ref, wire.ShortKey(c.peerKey), reason)
	n.st.Tx(func(q store.Q) error {
		b, err := store.GetBlob(q, c.peerKey, "in", ref)
		if err != nil || b == nil {
			return err
		}
		b.Status, b.Updated = "refused", time.Now()
		os.Remove(b.Path)
		return store.PutBlob(q, b)
	})
	c.send(wire.Err{Envelope: wire.Envelope{T: wire.TErr, ID: n.ids.New(), TS: wire.Now()}, Code: wire.ErrBlobRefused, Ref: ref, Detail: reason})
}

func writeAt(path string, off int64, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(off); err != nil {
		return err
	}
	if _, err := f.WriteAt(data, off); err != nil {
		return err
	}
	return f.Sync()
}
