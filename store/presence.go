package store

import (
	"database/sql"
	"errors"
	"time"
)

const presenceSchema = `
CREATE TABLE IF NOT EXISTS presence (
	origin   TEXT PRIMARY KEY,
	seq      INTEGER NOT NULL,
	name     TEXT NOT NULL DEFAULT '',
	ts       INTEGER NOT NULL,
	received INTEGER NOT NULL,
	via      TEXT NOT NULL DEFAULT '',
	hops     INTEGER NOT NULL DEFAULT 0,
	doc      BLOB NOT NULL
);
`

// PresenceRow is the newest signed presence document from one origin.
type PresenceRow struct {
	Origin   string
	Seq      int64
	Name     string
	TS       time.Time // the origin's timestamp
	Received time.Time
	Via      string // the peer it arrived from
	Hops     int
	Doc      []byte // exactly as signed
}

// PutPresence stores a document if it is newer (by seq) than what we have.
// It reports whether it was stored and whether the origin is new to us.
func (s *Store) PutPresence(r *PresenceRow) (stored, isNew bool, err error) {
	err = s.Tx(func(q Q) error {
		var seq int64
		err := q.QueryRow(`SELECT seq FROM presence WHERE origin = ?`, r.Origin).Scan(&seq)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			isNew = true
		case err != nil:
			return err
		case seq >= r.Seq:
			return nil
		}
		_, err = q.Exec(`INSERT INTO presence (origin, seq, name, ts, received, via, hops, doc) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (origin) DO UPDATE SET seq = excluded.seq, name = excluded.name, ts = excluded.ts,
				received = excluded.received, via = excluded.via, hops = excluded.hops, doc = excluded.doc`,
			r.Origin, r.Seq, r.Name, ms(r.TS), ms(r.Received), r.Via, r.Hops, r.Doc)
		stored = err == nil
		return err
	})
	return stored, isNew, err
}

// PresenceSince lists documents whose origin timestamp is at or after
// cutoff, most recent first.
func (s *Store) PresenceSince(cutoff time.Time) ([]*PresenceRow, error) {
	rows, err := s.db.Query(`SELECT origin, seq, name, ts, received, via, hops, doc FROM presence WHERE ts >= ? ORDER BY ts DESC`, ms(cutoff))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*PresenceRow{}
	for rows.Next() {
		var r PresenceRow
		var ts, received int64
		if err := rows.Scan(&r.Origin, &r.Seq, &r.Name, &ts, &received, &r.Via, &r.Hops, &r.Doc); err != nil {
			return nil, err
		}
		r.TS, r.Received = fromMS(ts), fromMS(received)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// ExpirePresence deletes documents older than cutoff and returns them.
func (s *Store) ExpirePresence(cutoff time.Time) ([]*PresenceRow, error) {
	rows, err := s.db.Query(`SELECT origin, name, ts FROM presence WHERE ts < ?`, ms(cutoff))
	if err != nil {
		return nil, err
	}
	var old []*PresenceRow
	for rows.Next() {
		var r PresenceRow
		var ts int64
		if err := rows.Scan(&r.Origin, &r.Name, &ts); err != nil {
			rows.Close()
			return nil, err
		}
		r.TS = fromMS(ts)
		old = append(old, &r)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(old) == 0 {
		return old, err
	}
	_, err = s.db.Exec(`DELETE FROM presence WHERE ts < ?`, ms(cutoff))
	return old, err
}

// TrimPresence keeps at most max origins, dropping the least recent. It
// bounds what a peer can make us store by inventing origins.
func (s *Store) TrimPresence(max int) error {
	_, err := s.db.Exec(`DELETE FROM presence WHERE origin NOT IN (SELECT origin FROM presence ORDER BY ts DESC LIMIT ?)`, max)
	return err
}
