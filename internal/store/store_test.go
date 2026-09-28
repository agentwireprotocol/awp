package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// oldThreads is the threads table before the per-side since columns.
const oldThreads = `CREATE TABLE threads (
	peer        TEXT NOT NULL,
	th          TEXT NOT NULL,
	subject     TEXT NOT NULL DEFAULT '',
	origin      TEXT NOT NULL DEFAULT '',
	created     INTEGER NOT NULL,
	updated     INTEGER NOT NULL,
	my_state    TEXT NOT NULL DEFAULT 'open',
	my_note     TEXT NOT NULL DEFAULT '',
	their_state TEXT NOT NULL DEFAULT 'open',
	their_note  TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (peer, th)
)`

// An older database gets the since columns at open, filled from updated,
// and opening it again leaves them alone.
func TestThreadSinceMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "awp.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(oldThreads); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO threads (peer, th, created, updated, their_state) VALUES ('k', 'thr_1', 1000, 5000, 'working')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		th, err := GetThread(s.DB(), "k", "thr_1")
		if err != nil {
			t.Fatal(err)
		}
		want := fromMS(5000)
		if th.TheirState != "working" || !th.MySince.Equal(want) || !th.TheirSince.Equal(want) {
			t.Fatalf("open %d: thread %+v", i, th)
		}
		s.Close()
	}
}

// A side's since moves when its state or note changes, and only then.
func TestSetThreadStateSince(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "awp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	get := func() *Thread {
		t.Helper()
		th, err := GetThread(s.DB(), "k", "thr_1")
		if err != nil || th == nil {
			t.Fatalf("thread: %v %v", th, err)
		}
		return th
	}
	t0 := time.UnixMilli(1000)
	if err := TouchThread(s.DB(), "k", "thr_1", "Subject", "us", t0); err != nil {
		t.Fatal(err)
	}
	if th := get(); !th.MySince.Equal(t0) || !th.TheirSince.Equal(t0) {
		t.Fatalf("new thread: %+v", th)
	}

	t1 := t0.Add(time.Minute)
	SetThreadState(s.DB(), "k", "thr_1", true, "working", "cloning", t1)
	if th := get(); !th.MySince.Equal(t1) || !th.TheirSince.Equal(t0) || !th.Updated.Equal(t1) {
		t.Fatalf("after my state: %+v", th)
	}
	// Saying the same again does not restart the clock.
	t2 := t1.Add(time.Minute)
	SetThreadState(s.DB(), "k", "thr_1", true, "working", "cloning", t2)
	if th := get(); !th.MySince.Equal(t1) || !th.Updated.Equal(t2) {
		t.Fatalf("after repeat: %+v", th)
	}
	// A new note does.
	t3 := t2.Add(time.Minute)
	SetThreadState(s.DB(), "k", "thr_1", true, "working", "testing", t3)
	if th := get(); !th.MySince.Equal(t3) {
		t.Fatalf("after new note: %+v", th)
	}
	// The other side keeps its own clock.
	t4 := t3.Add(time.Minute)
	SetThreadState(s.DB(), "k", "thr_1", false, "done", "", t4)
	if th := get(); !th.TheirSince.Equal(t4) || !th.MySince.Equal(t3) {
		t.Fatalf("after their state: %+v", th)
	}
	// Activity in the thread does not touch either.
	t5 := t4.Add(time.Minute)
	TouchThread(s.DB(), "k", "thr_1", "", "them", t5)
	if th := get(); !th.MySince.Equal(t3) || !th.TheirSince.Equal(t4) || !th.Updated.Equal(t5) {
		t.Fatalf("after touch: %+v", th)
	}
}
