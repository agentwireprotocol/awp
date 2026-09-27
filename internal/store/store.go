// Package store persists a holler peer's state in SQLite: the outbox of
// unacked messages, the per-thread "last seen" ids that resume needs, a log
// of every message sent and received, thread state, blobs and grants.
//
// Section 4.3 requires this to be on disk, because the process can be
// killed together with its sandbox at any moment. Every function that
// changes state is safe to interrupt: either the transaction commits or it
// never happened.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Q is implemented by *sql.DB and *sql.Tx.
type Q interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Store is the database handle.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS kv (k TEXT PRIMARY KEY, v TEXT NOT NULL);

CREATE TABLE IF NOT EXISTS peers (
	key        TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	about      TEXT NOT NULL DEFAULT '',
	caps       TEXT NOT NULL DEFAULT '[]',
	alias      TEXT NOT NULL DEFAULT '',
	first_seen INTEGER NOT NULL,
	last_seen  INTEGER NOT NULL DEFAULT 0,
	parked     INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS addrs (
	peer     TEXT NOT NULL,
	addr     TEXT NOT NULL,
	source   TEXT NOT NULL,
	added    INTEGER NOT NULL,
	last_ok  INTEGER NOT NULL DEFAULT 0,
	last_err TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (peer, addr)
);

CREATE TABLE IF NOT EXISTS outbox (
	seq     INTEGER PRIMARY KEY AUTOINCREMENT,
	peer    TEXT NOT NULL,
	id      TEXT NOT NULL,
	t       TEXT NOT NULL,
	th      TEXT NOT NULL DEFAULT '',
	ref     TEXT NOT NULL DEFAULT '',
	line    BLOB NOT NULL,
	created INTEGER NOT NULL,
	UNIQUE (peer, id)
);
CREATE INDEX IF NOT EXISTS outbox_peer ON outbox (peer, seq);

CREATE TABLE IF NOT EXISTS log (
	seq   INTEGER PRIMARY KEY AUTOINCREMENT,
	peer  TEXT NOT NULL,
	dir   TEXT NOT NULL,
	id    TEXT NOT NULL,
	t     TEXT NOT NULL,
	th    TEXT NOT NULL DEFAULT '',
	line  BLOB NOT NULL,
	at    INTEGER NOT NULL,
	acked INTEGER NOT NULL DEFAULT 0,
	read  INTEGER NOT NULL DEFAULT 0,
	meta  TEXT NOT NULL DEFAULT '{}',
	UNIQUE (peer, dir, id)
);
CREATE INDEX IF NOT EXISTS log_th ON log (th, seq);

-- Threads kept out of conversation sharing, because their other party
-- asked (a private message) or this agent did (wire/mirror.go).
CREATE TABLE IF NOT EXISTS private_threads (
	peer TEXT NOT NULL,
	th   TEXT NOT NULL,
	at   INTEGER NOT NULL,
	PRIMARY KEY (peer, th)
);

CREATE TABLE IF NOT EXISTS seen (
	peer    TEXT NOT NULL,
	th      TEXT NOT NULL,
	last_id TEXT NOT NULL,
	updated INTEGER NOT NULL,
	PRIMARY KEY (peer, th)
);

CREATE TABLE IF NOT EXISTS threads (
	peer        TEXT NOT NULL,
	th          TEXT NOT NULL,
	subject     TEXT NOT NULL DEFAULT '',
	origin      TEXT NOT NULL DEFAULT '',
	created     INTEGER NOT NULL,
	updated     INTEGER NOT NULL,
	my_state    TEXT NOT NULL DEFAULT 'open',
	my_note     TEXT NOT NULL DEFAULT '',
	my_since    INTEGER NOT NULL DEFAULT 0,
	their_state TEXT NOT NULL DEFAULT 'open',
	their_note  TEXT NOT NULL DEFAULT '',
	their_since INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (peer, th)
);

CREATE TABLE IF NOT EXISTS blobs (
	peer     TEXT NOT NULL,
	dir      TEXT NOT NULL,
	ref      TEXT NOT NULL,
	th       TEXT NOT NULL DEFAULT '',
	name     TEXT NOT NULL DEFAULT '',
	mime     TEXT NOT NULL DEFAULT '',
	size     INTEGER NOT NULL DEFAULT -1,
	received INTEGER NOT NULL DEFAULT 0,
	next_n   INTEGER NOT NULL DEFAULT 0,
	path     TEXT NOT NULL DEFAULT '',
	status   TEXT NOT NULL DEFAULT 'pending',
	updated  INTEGER NOT NULL,
	PRIMARY KEY (peer, dir, ref)
);

CREATE TABLE IF NOT EXISTS grants (
	hash     TEXT NOT NULL,
	role     TEXT NOT NULL,
	iss      TEXT NOT NULL,
	sub      TEXT NOT NULL,
	caps     TEXT NOT NULL,
	exp      INTEGER NOT NULL,
	raw      BLOB NOT NULL,
	audience TEXT NOT NULL DEFAULT '',
	created  INTEGER NOT NULL,
	PRIMARY KEY (hash, role)
);
`

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(10000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection serializes all access. The workload is small and this
	// rules out SQLITE_BUSY between our own goroutines.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + presenceSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store migration: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate adds columns that a database from an older version lacks.
// CREATE TABLE IF NOT EXISTS leaves an existing table as it was, so each
// addition checks the table's columns first and is safe to run at every
// open.
func migrate(db *sql.DB) error {
	cols, err := columns(db, "threads")
	if err != nil {
		return err
	}
	// Per-side state timestamps. Existing rows start from the thread's
	// last update, the closest thing known.
	for _, col := range []string{"my_since", "their_since"} {
		if cols[col] {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`ALTER TABLE threads ADD COLUMN ` + col + ` INTEGER NOT NULL DEFAULT 0`); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`UPDATE threads SET ` + col + ` = updated`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// columns names a table's columns.
func columns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB returns the handle for reads outside a transaction.
func (s *Store) DB() Q { return s.db }

// Tx runs fn in a transaction. fn must use only the Q it is given: the
// store has a single connection, so touching s.DB() inside fn deadlocks.
func (s *Store) Tx(fn func(q Q) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

// --- kv ---

// GetKV returns a stored value.
func (s *Store) GetKV(k string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM kv WHERE k = ?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// SetKV stores a value.
func (s *Store) SetKV(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO kv (k, v) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}

// MaxOwnID returns the largest id this peer ever sent, so the id generator
// can keep increasing across restarts.
func (s *Store) MaxOwnID() (string, error) {
	var a, b sql.NullString
	if err := s.db.QueryRow(`SELECT max(id) FROM outbox`).Scan(&a); err != nil {
		return "", err
	}
	if err := s.db.QueryRow(`SELECT max(id) FROM log WHERE dir = 'out'`).Scan(&b); err != nil {
		return "", err
	}
	return max(a.String, b.String), nil
}

// --- peers ---

// Peer is what we know about a remote key.
type Peer struct {
	Key       string    `json:"key"`
	Name      string    `json:"name,omitempty"`
	About     string    `json:"about,omitempty"`
	Caps      []string  `json:"caps,omitempty"`
	Alias     string    `json:"alias,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen,omitzero"`
	Parked    bool      `json:"parked,omitempty"`
	Addrs     []Addr    `json:"addrs,omitempty"`
}

// Addr is an address hint for a peer. Sources, from most to least
// authoritative: "connect" (given to us out of band), "introduce" and
// "hello" (the peer's own advertised address).
type Addr struct {
	Addr    string    `json:"addr"`
	Source  string    `json:"source"`
	Added   time.Time `json:"added"`
	LastOK  time.Time `json:"last_ok,omitzero"`
	LastErr string    `json:"last_err,omitempty"`
}

var sourceRank = map[string]int{"connect": 3, "introduce": 2, "hello": 1}

// EnsurePeer creates a peer row if it does not exist.
func EnsurePeer(q Q, key string, now time.Time) error {
	_, err := q.Exec(`INSERT INTO peers (key, first_seen) VALUES (?, ?) ON CONFLICT (key) DO NOTHING`, key, ms(now))
	return err
}

// TouchPeer records what a peer said about itself in hello.
func TouchPeer(q Q, key, name, about string, caps []string, now time.Time) error {
	if err := EnsurePeer(q, key, now); err != nil {
		return err
	}
	cj, _ := json.Marshal(caps)
	_, err := q.Exec(`UPDATE peers SET name = ?, about = ?, caps = ?, last_seen = ? WHERE key = ?`, name, about, string(cj), ms(now), key)
	return err
}

// AddAddr records an address hint. A hint from a more authoritative source
// upgrades the stored source.
func AddAddr(q Q, peer, addr, source string, now time.Time) error {
	if addr == "" {
		return nil
	}
	if err := EnsurePeer(q, peer, now); err != nil {
		return err
	}
	var cur string
	err := q.QueryRow(`SELECT source FROM addrs WHERE peer = ? AND addr = ?`, peer, addr).Scan(&cur)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = q.Exec(`INSERT INTO addrs (peer, addr, source, added) VALUES (?, ?, ?, ?)`, peer, addr, source, ms(now))
		return err
	case err != nil:
		return err
	case sourceRank[source] > sourceRank[cur]:
		_, err = q.Exec(`UPDATE addrs SET source = ? WHERE peer = ? AND addr = ?`, source, peer, addr)
		return err
	}
	return nil
}

// AddrResult records the outcome of dialing an address.
func (s *Store) AddrResult(peer, addr string, dialErr error, now time.Time) error {
	if dialErr == nil {
		_, err := s.db.Exec(`UPDATE addrs SET last_ok = ?, last_err = '' WHERE peer = ? AND addr = ?`, ms(now), peer, addr)
		return err
	}
	_, err := s.db.Exec(`UPDATE addrs SET last_err = ? WHERE peer = ? AND addr = ?`, dialErr.Error(), peer, addr)
	return err
}

// Addrs lists a peer's address hints, best first: the address that worked
// most recently, then by source.
func Addrs(q Q, peer string) ([]Addr, error) {
	rows, err := q.Query(`SELECT addr, source, added, last_ok, last_err FROM addrs WHERE peer = ?`, peer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Addr
	for rows.Next() {
		var a Addr
		var added, lastOK int64
		if err := rows.Scan(&a.Addr, &a.Source, &added, &lastOK, &a.LastErr); err != nil {
			return nil, err
		}
		a.Added, a.LastOK = fromMS(added), fromMS(lastOK)
		out = append(out, a)
	}
	sortAddrs(out)
	return out, rows.Err()
}

func sortAddrs(a []Addr) {
	better := func(x, y Addr) bool {
		if !x.LastOK.Equal(y.LastOK) {
			return x.LastOK.After(y.LastOK)
		}
		if sourceRank[x.Source] != sourceRank[y.Source] {
			return sourceRank[x.Source] > sourceRank[y.Source]
		}
		return x.Added.After(y.Added)
	}
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && better(a[j], a[j-1]); j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

const peerCols = `key, name, about, caps, alias, first_seen, last_seen, parked`

func scanPeer(sc interface{ Scan(...any) error }) (*Peer, error) {
	var p Peer
	var caps string
	var first, last int64
	var parked int
	if err := sc.Scan(&p.Key, &p.Name, &p.About, &caps, &p.Alias, &first, &last, &parked); err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(caps), &p.Caps)
	p.FirstSeen, p.LastSeen, p.Parked = fromMS(first), fromMS(last), parked != 0
	return &p, nil
}

// GetPeer returns a peer, or nil if unknown.
func GetPeer(q Q, key string) (*Peer, error) {
	p, err := scanPeer(q.QueryRow(`SELECT `+peerCols+` FROM peers WHERE key = ?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Addrs, err = Addrs(q, key)
	return p, err
}

// Peers lists all known peers, most recently seen first.
func (s *Store) Peers() ([]*Peer, error) {
	rows, err := s.db.Query(`SELECT ` + peerCols + ` FROM peers ORDER BY last_seen DESC, first_seen DESC`)
	if err != nil {
		return nil, err
	}
	out := []*Peer{}
	for rows.Next() {
		p, err := scanPeer(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, p := range out {
		if p.Addrs, err = Addrs(s.db, p.Key); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetParked marks a peer as parked (it said bye, or we did): no automatic
// reconnection until there is new outbound traffic.
func (s *Store) SetParked(key string, parked bool) error {
	_, err := s.db.Exec(`UPDATE peers SET parked = ? WHERE key = ?`, parked, key)
	return err
}

// SetAlias sets a local nickname for a peer.
func (s *Store) SetAlias(key, alias string) error {
	_, err := s.db.Exec(`UPDATE peers SET alias = ? WHERE key = ?`, alias, key)
	return err
}

// --- outbox ---

// OutboxRow is a sent line that the peer has not yet confirmed.
type OutboxRow struct {
	Seq     int64
	Peer    string
	ID      string
	T       string
	Th      string
	Ref     string
	Line    []byte
	Created time.Time
}

// Enqueue appends a line to a peer's outbox.
func Enqueue(q Q, r *OutboxRow) error {
	res, err := q.Exec(`INSERT INTO outbox (peer, id, t, th, ref, line, created) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.Peer, r.ID, r.T, r.Th, r.Ref, r.Line, ms(r.Created))
	if err != nil {
		return err
	}
	r.Seq, err = res.LastInsertId()
	return err
}

// OutboxAfter returns up to limit outbox rows for peer with seq > after, in
// order.
func (s *Store) OutboxAfter(peer string, after int64, limit int) ([]OutboxRow, error) {
	rows, err := s.db.Query(`SELECT seq, id, t, th, ref, line, created FROM outbox WHERE peer = ? AND seq > ? ORDER BY seq LIMIT ?`, peer, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		r := OutboxRow{Peer: peer}
		var created int64
		if err := rows.Scan(&r.Seq, &r.ID, &r.T, &r.Th, &r.Ref, &r.Line, &created); err != nil {
			return nil, err
		}
		r.Created = fromMS(created)
		out = append(out, r)
	}
	return out, rows.Err()
}

// unacked lists the message types that are never acked. They leave the
// outbox when a later acked message in the same thread is acked (in-order
// delivery within a thread means everything before it arrived) or when the
// peer's resume shows it has seen them.
var unacked = []string{"chunk", "introduce"}

// AckOutbox removes the acked message from the outbox, together with any
// earlier never-acked lines in the same thread, and marks the outgoing
// blobs those lines carried as sent. It reports the row's thread and type,
// and whether it was found.
func AckOutbox(q Q, peer, id string, now time.Time) (th, t string, found bool, err error) {
	var seq int64
	err = q.QueryRow(`SELECT seq, th, t FROM outbox WHERE peer = ? AND id = ?`, peer, id).Scan(&seq, &th, &t)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	if _, err = q.Exec(`DELETE FROM outbox WHERE seq = ?`, seq); err != nil {
		return
	}
	if th != "" {
		if _, err = q.Exec(`DELETE FROM outbox WHERE peer = ? AND th = ? AND seq < ? AND t IN ('chunk', 'introduce')`, peer, th, seq); err != nil {
			return
		}
		err = blobsSent(q, peer, th, now)
	}
	return th, t, true, err
}

// blobsSent marks the outgoing blobs of a thread whose chunks have all left
// the outbox as sent: the peer acked, or has seen, the msg that carries them.
func blobsSent(q Q, peer, th string, now time.Time) error {
	_, err := q.Exec(`UPDATE blobs SET status = 'sent', updated = ? WHERE peer = ? AND dir = 'out' AND th = ? AND status = 'queued'
		AND ref NOT IN (SELECT ref FROM outbox WHERE peer = ? AND t = 'chunk')`, ms(now), peer, th, peer)
	return err
}

// PruneSeen drops outbox rows the peer's resume says it already has: in
// each listed thread, everything up to and including the seen id. The
// matching log rows are marked acked, since "seen" means durably received,
// and blobs whose chunks are all seen are sent.
func PruneSeen(q Q, peer string, seen map[string]string, now time.Time) (int64, error) {
	var n int64
	for th, id := range seen {
		if th == "" || id == "" {
			continue
		}
		res, err := q.Exec(`DELETE FROM outbox WHERE peer = ? AND th = ? AND id <= ?`, peer, th, id)
		if err != nil {
			return n, err
		}
		k, _ := res.RowsAffected()
		n += k
		if _, err := q.Exec(`UPDATE log SET acked = 1 WHERE peer = ? AND dir = 'out' AND th = ? AND id <= ? AND acked = 0`, peer, th, id); err != nil {
			return n, err
		}
		if err := blobsSent(q, peer, th, now); err != nil {
			return n, err
		}
	}
	return n, nil
}

// DropRef removes the queued chunks of a blob (the peer refused it).
func DropRef(q Q, peer, ref string) error {
	_, err := q.Exec(`DELETE FROM outbox WHERE peer = ? AND t = 'chunk' AND ref = ?`, peer, ref)
	return err
}

// DropThread removes everything queued for a thread.
func DropThread(q Q, peer, th string) error {
	_, err := q.Exec(`DELETE FROM outbox WHERE peer = ? AND th = ?`, peer, th)
	return err
}

// OutboxCount returns how many lines are waiting for peer ("" for all).
func (s *Store) OutboxCount(peer string) (int, error) {
	var n int
	var err error
	if peer == "" {
		err = s.db.QueryRow(`SELECT count(*) FROM outbox`).Scan(&n)
	} else {
		err = s.db.QueryRow(`SELECT count(*) FROM outbox WHERE peer = ?`, peer).Scan(&n)
	}
	return n, err
}

// ExpireOutbox drops outbox rows created before cutoff (open question 2
// proposes seven days) and returns how many went.
func (s *Store) ExpireOutbox(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM outbox WHERE created < ?`, ms(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PeersWithOutbox lists peers that have queued lines.
func (s *Store) PeersWithOutbox() ([]string, error) {
	return s.strings(`SELECT DISTINCT peer FROM outbox`)
}

func (s *Store) strings(query string, args ...any) ([]string, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// --- log ---

// Record is one entry in the message log. Dir is "in" (received), "out"
// (sent), "sys" (a local event such as a connection coming up) or "mirror"
// (a line of another agent's thread that it shares with this host; Peer is
// that agent, ID the original line's id, Meta says who the other party is).
type Record struct {
	Seq   int64          `json:"seq"`
	Peer  string         `json:"peer"`
	Dir   string         `json:"dir"`
	ID    string         `json:"id"`
	T     string         `json:"t"`
	Th    string         `json:"th,omitempty"`
	Line  []byte         `json:"-"`
	At    time.Time      `json:"at"`
	Acked bool           `json:"acked,omitempty"`
	Read  bool           `json:"read,omitempty"`
	Meta  map[string]any `json:"meta,omitempty"`
}

// Append adds a record, unless one with the same (peer, dir, id) exists, in
// which case it reports inserted == false. That is the receive-side dedup
// that section 9.5 requires.
func Append(q Q, r *Record) (inserted bool, err error) {
	meta := "{}"
	if len(r.Meta) > 0 {
		b, err := json.Marshal(r.Meta)
		if err != nil {
			return false, err
		}
		meta = string(b)
	}
	res, err := q.Exec(`INSERT INTO log (peer, dir, id, t, th, line, at, acked, read, meta) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (peer, dir, id) DO NOTHING`,
		r.Peer, r.Dir, r.ID, r.T, r.Th, r.Line, ms(r.At), r.Acked, r.Read, meta)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	r.Seq, err = res.LastInsertId()
	return true, err
}

// HasLog reports whether a record exists.
func HasLog(q Q, peer, dir, id string) (bool, error) {
	var one int
	err := q.QueryRow(`SELECT 1 FROM log WHERE peer = ? AND dir = ? AND id = ?`, peer, dir, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// SetAcked marks a sent message as acked.
func SetAcked(q Q, peer, id string) error {
	_, err := q.Exec(`UPDATE log SET acked = 1 WHERE peer = ? AND dir = 'out' AND id = ?`, peer, id)
	return err
}

// SetMeta merges keys into a record's metadata.
func (s *Store) SetMeta(seq int64, add map[string]any) error {
	return s.Tx(func(q Q) error {
		var meta string
		if err := q.QueryRow(`SELECT meta FROM log WHERE seq = ?`, seq).Scan(&meta); err != nil {
			return err
		}
		m := map[string]any{}
		json.Unmarshal([]byte(meta), &m)
		for k, v := range add {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		_, err := q.Exec(`UPDATE log SET meta = ? WHERE seq = ?`, string(b), seq)
		return err
	})
}

// Filter selects log records.
type Filter struct {
	Peer       string
	Th         string
	Dirs       []string // default: all
	Types      []string // default: all
	AfterSeq   int64
	UnreadOnly bool
	Inbox      bool // received records plus local events worth a reader's attention
	Limit      int  // default 1000
	Last       bool // with Limit: the last N records, still returned in order
	// Mirrors includes mirrored lines (dir "mirror"), which are left out
	// unless asked for by this or by Dirs: they are other agents' threads,
	// not this agent's business.
	Mirrors bool
}

// InboxSys lists the local (sys) event types that belong in an inbox.
var InboxSys = []string{"blob", "bye", "refused", "shares", "private"}

// Query returns log records matching f, in log order.
func (s *Store) Query(f Filter) ([]Record, error) {
	var where []string
	var args []any
	if f.Peer != "" {
		where = append(where, "peer = ?")
		args = append(args, f.Peer)
	}
	if f.Th != "" {
		where = append(where, "th = ?")
		args = append(args, f.Th)
	}
	if len(f.Dirs) > 0 {
		where = append(where, "dir IN ("+placeholders(len(f.Dirs))+")")
		for _, d := range f.Dirs {
			args = append(args, d)
		}
	} else if !f.Mirrors {
		where = append(where, "dir != 'mirror'")
	}
	if len(f.Types) > 0 {
		where = append(where, "t IN ("+placeholders(len(f.Types))+")")
		for _, t := range f.Types {
			args = append(args, t)
		}
	}
	if f.AfterSeq > 0 {
		where = append(where, "seq > ?")
		args = append(args, f.AfterSeq)
	}
	if f.UnreadOnly {
		where = append(where, "read = 0")
	}
	if f.Inbox {
		where = append(where, "(dir = 'in' OR (dir = 'sys' AND t IN ('blob', 'bye', 'refused', 'shares', 'private')))")
	}
	query := `SELECT seq, peer, dir, id, t, th, line, at, acked, read, meta FROM log`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 1000
	}
	if f.Last {
		query = `SELECT * FROM (` + query + ` ORDER BY seq DESC LIMIT ?) ORDER BY seq`
	} else {
		query += ` ORDER BY seq LIMIT ?`
	}
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var at int64
		var meta string
		if err := rows.Scan(&r.Seq, &r.Peer, &r.Dir, &r.ID, &r.T, &r.Th, &r.Line, &at, &r.Acked, &r.Read, &meta); err != nil {
			return nil, err
		}
		r.At = fromMS(at)
		if meta != "{}" {
			json.Unmarshal([]byte(meta), &r.Meta)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// MarkRead marks records as read.
func (s *Store) MarkRead(seqs []int64) error {
	if len(seqs) == 0 {
		return nil
	}
	return s.Tx(func(q Q) error {
		for _, seq := range seqs {
			if _, err := q.Exec(`UPDATE log SET read = 1 WHERE seq = ?`, seq); err != nil {
				return err
			}
		}
		return nil
	})
}

// MaxSeq returns the newest log sequence number.
func (s *Store) MaxSeq() (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRow(`SELECT max(seq) FROM log`).Scan(&n)
	return n.Int64, err
}

// --- seen ---

// BumpSeen raises the last seen id of a thread to id if it is larger.
func BumpSeen(q Q, peer, th, id string, now time.Time) error {
	if th == "" || id == "" {
		return nil
	}
	_, err := q.Exec(`INSERT INTO seen (peer, th, last_id, updated) VALUES (?, ?, ?, ?)
		ON CONFLICT (peer, th) DO UPDATE SET last_id = max(last_id, excluded.last_id), updated = excluded.updated`,
		peer, th, id, ms(now))
	return err
}

// SeenMap returns the per-thread last seen ids for peer, for threads
// touched since cutoff (zero for all).
func (s *Store) SeenMap(peer string, cutoff time.Time) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT th, last_id FROM seen WHERE peer = ? AND updated >= ?`, peer, ms(cutoff))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var th, id string
		if err := rows.Scan(&th, &id); err != nil {
			return nil, err
		}
		m[th] = id
	}
	return m, rows.Err()
}

// --- threads ---

// Thread is a conversation with one peer.
type Thread struct {
	Peer       string    `json:"peer"`
	Th         string    `json:"th"`
	Subject    string    `json:"subject,omitempty"`
	Origin     string    `json:"origin"`
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
	MyState    string    `json:"my_state"`
	MyNote     string    `json:"my_note,omitempty"`
	MySince    time.Time `json:"my_since"` // when my state or note last changed
	TheirState string    `json:"their_state"`
	TheirNote  string    `json:"their_note,omitempty"`
	TheirSince time.Time `json:"their_since"`
	Unread     int       `json:"unread,omitempty"`
}

// Terminal reports whether a state ends a thread's active life.
func Terminal(state string) bool {
	return state == "done" || state == "failed" || state == "closed"
}

// Open reports whether the thread still wants a live connection.
func (t *Thread) Open() bool {
	return !Terminal(t.MyState) && !Terminal(t.TheirState)
}

// TouchThread creates a thread on first sight (open on both sides since
// now), and otherwise bumps its update time and fills in a missing subject.
func TouchThread(q Q, peer, th, subject, origin string, now time.Time) error {
	if th == "" {
		return nil
	}
	_, err := q.Exec(`INSERT INTO threads (peer, th, subject, origin, created, updated, my_since, their_since) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (peer, th) DO UPDATE SET updated = excluded.updated,
			subject = CASE WHEN threads.subject = '' THEN excluded.subject ELSE threads.subject END`,
		peer, th, subject, origin, ms(now), ms(now), ms(now), ms(now))
	return err
}

// SetThreadState records a state change, ours (mine) or the peer's. The
// side's since timestamp moves only when its state or note differs from
// what it was, so it says how long the side has been in that state.
func SetThreadState(q Q, peer, th string, mine bool, state, note string, now time.Time) error {
	side := "their"
	if mine {
		side = "my"
	}
	_, err := q.Exec(`UPDATE threads SET `+side+`_since = CASE WHEN `+side+`_state = ? AND `+side+`_note = ? THEN `+side+`_since ELSE ? END,
		`+side+`_state = ?, `+side+`_note = ?, updated = ? WHERE peer = ? AND th = ?`,
		state, note, ms(now), state, note, ms(now), peer, th)
	return err
}

const threadCols = `t.peer, t.th, t.subject, t.origin, t.created, t.updated, t.my_state, t.my_note, t.my_since, t.their_state, t.their_note, t.their_since,
	(SELECT count(*) FROM log l WHERE l.peer = t.peer AND l.th = t.th AND l.dir = 'in' AND l.read = 0)`

func scanThread(sc interface{ Scan(...any) error }) (*Thread, error) {
	var t Thread
	var created, updated, mySince, theirSince int64
	err := sc.Scan(&t.Peer, &t.Th, &t.Subject, &t.Origin, &created, &updated, &t.MyState, &t.MyNote, &mySince, &t.TheirState, &t.TheirNote, &theirSince, &t.Unread)
	if err != nil {
		return nil, err
	}
	t.Created, t.Updated = fromMS(created), fromMS(updated)
	t.MySince, t.TheirSince = fromMS(mySince), fromMS(theirSince)
	return &t, nil
}

// Threads lists threads, most recently updated first. peer and th filter
// when non-empty.
func (s *Store) Threads(peer, th string) ([]*Thread, error) {
	query := `SELECT ` + threadCols + ` FROM threads t WHERE (? = '' OR t.peer = ?) AND (? = '' OR t.th = ?) ORDER BY t.updated DESC`
	rows, err := s.db.Query(query, peer, peer, th, th)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Thread{}
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetThread returns one thread or nil.
func GetThread(q Q, peer, th string) (*Thread, error) {
	t, err := scanThread(q.QueryRow(`SELECT `+threadCols+` FROM threads t WHERE t.peer = ? AND t.th = ?`, peer, th))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// OpenThreads counts a peer's threads that are neither done, failed nor
// closed on either side and were active since cutoff.
func (s *Store) OpenThreads(peer string, cutoff time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM threads WHERE peer = ? AND updated >= ?
		AND my_state NOT IN ('done', 'failed', 'closed') AND their_state NOT IN ('done', 'failed', 'closed')`, peer, ms(cutoff)).Scan(&n)
	return n, err
}

// --- blobs ---

// Blob tracks one blob transfer in either direction.
type Blob struct {
	Peer     string    `json:"peer"`
	Dir      string    `json:"dir"`
	Ref      string    `json:"ref"`
	Th       string    `json:"th,omitempty"`
	Name     string    `json:"name,omitempty"`
	Mime     string    `json:"mime,omitempty"`
	Size     int64     `json:"size"` // declared size, -1 if not yet known
	Received int64     `json:"received"`
	NextN    int       `json:"next_n"`
	Path     string    `json:"path,omitempty"`
	Status   string    `json:"status"` // in: pending; received (data in, awaiting the msg that names it); complete; refused. out: queued; sent (the msg that carries it was acked); refused
	Updated  time.Time `json:"updated"`
}

// GetBlob returns a blob or nil.
func GetBlob(q Q, peer, dir, ref string) (*Blob, error) {
	b := Blob{Peer: peer, Dir: dir, Ref: ref}
	var updated int64
	err := q.QueryRow(`SELECT th, name, mime, size, received, next_n, path, status, updated FROM blobs WHERE peer = ? AND dir = ? AND ref = ?`, peer, dir, ref).
		Scan(&b.Th, &b.Name, &b.Mime, &b.Size, &b.Received, &b.NextN, &b.Path, &b.Status, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.Updated = fromMS(updated)
	return &b, nil
}

// PutBlob inserts or replaces a blob row.
func PutBlob(q Q, b *Blob) error {
	_, err := q.Exec(`INSERT INTO blobs (peer, dir, ref, th, name, mime, size, received, next_n, path, status, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (peer, dir, ref) DO UPDATE SET th = excluded.th, name = excluded.name, mime = excluded.mime, size = excluded.size,
			received = excluded.received, next_n = excluded.next_n, path = excluded.path, status = excluded.status, updated = excluded.updated`,
		b.Peer, b.Dir, b.Ref, b.Th, b.Name, b.Mime, b.Size, b.Received, b.NextN, b.Path, b.Status, ms(b.Updated))
	return err
}

// Blobs lists blobs for a peer ("" for all), newest first.
func (s *Store) Blobs(peer string) ([]*Blob, error) {
	rows, err := s.db.Query(`SELECT peer, dir, ref, th, name, mime, size, received, next_n, path, status, updated FROM blobs WHERE ? = '' OR peer = ? ORDER BY updated DESC`, peer, peer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Blob{}
	for rows.Next() {
		var b Blob
		var updated int64
		if err := rows.Scan(&b.Peer, &b.Dir, &b.Ref, &b.Th, &b.Name, &b.Mime, &b.Size, &b.Received, &b.NextN, &b.Path, &b.Status, &updated); err != nil {
			return nil, err
		}
		b.Updated = fromMS(updated)
		out = append(out, &b)
	}
	return out, rows.Err()
}

// --- grants ---

// Grant roles.
const (
	RoleIssued    = "issued"    // we signed it
	RoleHeld      = "held"      // someone granted it to us
	RolePresented = "presented" // a peer showed it to us about itself
)

// GrantRow is a stored grant.
type GrantRow struct {
	Hash     string    `json:"hash"`
	Role     string    `json:"role"`
	Iss      string    `json:"iss"`
	Sub      string    `json:"sub"`
	Caps     []string  `json:"caps"`
	Exp      time.Time `json:"exp"`
	Raw      []byte    `json:"-"`
	Audience string    `json:"audience,omitempty"`
	Created  time.Time `json:"created"`
}

// PutGrant stores a grant under a role. Storing the same grant twice under
// the same role is a no-op.
func PutGrant(q Q, g *GrantRow) error {
	cj, _ := json.Marshal(g.Caps)
	_, err := q.Exec(`INSERT INTO grants (hash, role, iss, sub, caps, exp, raw, audience, created) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (hash, role) DO UPDATE SET audience = CASE WHEN excluded.audience != '' THEN excluded.audience ELSE grants.audience END`,
		g.Hash, g.Role, g.Iss, g.Sub, string(cj), ms(g.Exp), g.Raw, g.Audience, ms(g.Created))
	return err
}

// GrantQuery selects grants. Empty fields match anything.
type GrantQuery struct {
	Role     string
	Iss      string
	Sub      string
	Audience string
	ValidAt  time.Time // if set, only grants not expired at this time
}

// Grants returns matching grants, newest first.
func (s *Store) Grants(gq GrantQuery) ([]*GrantRow, error) {
	rows, err := s.db.Query(`SELECT hash, role, iss, sub, caps, exp, raw, audience, created FROM grants
		WHERE (? = '' OR role = ?) AND (? = '' OR iss = ?) AND (? = '' OR sub = ?) AND (? = '' OR audience = ?) AND (? = 0 OR exp > ?)
		ORDER BY created DESC`,
		gq.Role, gq.Role, gq.Iss, gq.Iss, gq.Sub, gq.Sub, gq.Audience, gq.Audience, ms(gq.ValidAt), ms(gq.ValidAt))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*GrantRow{}
	for rows.Next() {
		var g GrantRow
		var caps string
		var exp, created int64
		if err := rows.Scan(&g.Hash, &g.Role, &g.Iss, &g.Sub, &caps, &exp, &g.Raw, &g.Audience, &created); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(caps), &g.Caps)
		g.Exp, g.Created = fromMS(exp), fromMS(created)
		out = append(out, &g)
	}
	return out, rows.Err()
}

// DeleteGrant removes a grant in every role (local revocation).
func (s *Store) DeleteGrant(hash string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM grants WHERE hash = ?`, hash)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- conversation sharing ---

// SetPrivate keeps a thread out of conversation sharing. It reports
// whether the thread was not private before.
func SetPrivate(q Q, peer, th string, now time.Time) (bool, error) {
	res, err := q.Exec(`INSERT INTO private_threads (peer, th, at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, peer, th, ms(now))
	if err != nil {
		return false, err
	}
	k, _ := res.RowsAffected()
	return k > 0, nil
}

// IsPrivate reports whether a thread is kept out of conversation sharing.
func IsPrivate(q Q, peer, th string) (bool, error) {
	var one int
	err := q.QueryRow(`SELECT 1 FROM private_threads WHERE peer = ? AND th = ?`, peer, th).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// DeleteMirrored forgets what sharer mirrored of its thread th with of.
func DeleteMirrored(q Q, sharer, th, of string) (int64, error) {
	res, err := q.Exec(`DELETE FROM log WHERE peer = ? AND dir = 'mirror' AND th = ? AND json_extract(meta, '$.of') = ?`, sharer, th, of)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MirroredThread is a thread another agent shares with this host.
type MirroredThread struct {
	Sharer  string    `json:"sharer"` // the agent that shares it
	Of      string    `json:"of"`     // the other party
	Th      string    `json:"th"`
	Subject string    `json:"subject,omitempty"`
	Lines   int       `json:"lines"`
	Updated time.Time `json:"updated"`
}

// MirroredThreads lists the threads other agents share with this host.
func (s *Store) MirroredThreads() ([]MirroredThread, error) {
	rows, err := s.db.Query(`SELECT peer, json_extract(meta, '$.of'), th, max(json_extract(meta, '$.subject')), count(*), max(at)
		FROM log WHERE dir = 'mirror' GROUP BY peer, th, json_extract(meta, '$.of') ORDER BY max(at) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MirroredThread{}
	for rows.Next() {
		var m MirroredThread
		var of, subject sql.NullString
		var at int64
		if err := rows.Scan(&m.Sharer, &of, &m.Th, &subject, &m.Lines, &at); err != nil {
			return nil, err
		}
		m.Of, m.Subject, m.Updated = of.String, subject.String, fromMS(at)
		out = append(out, m)
	}
	return out, rows.Err()
}
