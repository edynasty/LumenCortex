package session

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/mattn/go-sqlite3"
)

// A second connection pool on the store's database file whose connections count the write
// transactions they commit and roll back. It lives in the tests only: the production store carries no
// counter. SQLite calls the commit hook once per committed write transaction, so a read-only
// transaction is not counted.
var (
	txCommits   atomic.Int64
	txRollbacks atomic.Int64
)

func init() {
	sql.Register("sqlite3_txcount", &sqlite3.SQLiteDriver{ConnectHook: func(c *sqlite3.SQLiteConn) error {
		c.RegisterCommitHook(func() int { txCommits.Add(1); return 0 })
		c.RegisterRollbackHook(func() { txRollbacks.Add(1) })
		return nil
	}})
}

// countingStore opens a Store over the file of base through the counting driver.
func countingStore(t *testing.T, base *Store) *Store {
	t.Helper()
	db, err := sql.Open("sqlite3_txcount", "file:"+filepath.ToSlash(base.Path())+"?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000&_synchronous=NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return &Store{db: db, path: base.Path()}
}

// writes runs fn and returns the write transactions it committed and rolled back.
func writes(fn func()) (commits, rollbacks int64) {
	c0, r0 := txCommits.Load(), txRollbacks.Load()
	fn()
	return txCommits.Load() - c0, txRollbacks.Load() - r0
}

// Design 3.1: every Store write is one transaction. One AdmitRequest is exactly one committed write
// transaction, refused on a fence included (the refusal is recorded), and a request refused before
// anything is written commits none. AppendAnswered and SettleRequest are one each.
func TestEachStoreWriteIsExactlyOneTransaction(t *testing.T) {
	ctx := context.Background()
	base := provStore(t)
	gapSession(t, base, "s", "idle", "")
	gapSession(t, base, "fenced", "idle", "")
	addFence(t, base, "session", "fenced")
	store := countingStore(t, base)

	// Control: the counter sees each of two separate writes, so a count of one means one transaction.
	if commits, _ := writes(func() {
		gapSession(t, store, "c1", "idle", "")
		gapSession(t, store, "c2", "idle", "")
	}); commits != 2 {
		t.Fatalf("control: two session creates counted as %d write transactions", commits)
	}
	check := func(name string, wantCommits int64, fn func() error, wantErr error) {
		t.Helper()
		var err error
		commits, _ := writes(func() { err = fn() })
		if wantErr == nil && err != nil || wantErr != nil && !errors.Is(err, wantErr) {
			t.Fatalf("%s: err = %v, want %v", name, err, wantErr)
		}
		if commits != wantCommits {
			t.Errorf("%s: %d write transactions, want %d", name, commits, wantCommits)
		}
	}
	check("AdmitRequest", 1, func() error { _, err := store.AdmitRequest(ctx, mainAdmission("s", "r1")); return err }, nil)
	check("AdmitRequest on a fenced session", 1, func() error { _, err := store.AdmitRequest(ctx, mainAdmission("fenced", "rf")); return err }, ErrFenced)
	bad := mainAdmission("s", "r-bad")
	bad.PendingInputDigest = "not-a-digest"
	if commits, _ := writes(func() {
		if _, err := store.AdmitRequest(ctx, bad); err == nil {
			t.Error("a malformed admission was accepted")
		}
	}); commits != 0 {
		t.Errorf("a refused admission committed %d write transactions, want 0", commits)
	}
	check("AppendAnswered", 1, func() error { _, err := store.AppendAnswered(ctx, "s", "r1", assistant("done")); return err }, nil)
	admit(t, store, mainAdmission("s", "r2"))
	check("SettleRequest", 1, func() error {
		return store.SettleRequest(ctx, "r2", Settlement{To: RequestUnknown, Reason: "provider_error"})
	}, nil)
	check("AdmitRequest of an auto judge", 1, func() error { _, err := store.AdmitRequest(ctx, autoAdmission("s", "ra")); return err }, nil)
}

// Three provider attempts of one step - a capacity rejection retried, a failover, and the answer - take
// three admission transactions and three settlement transactions, one each per attempt and nothing
// else: no extra write is hidden in the path of a retry.
func TestEveryAttemptTakesOneAdmissionTransaction(t *testing.T) {
	ctx := context.Background()
	base := provStore(t)
	gapSession(t, base, "s", "idle", "")
	store := countingStore(t, base)
	type attempt struct {
		id     string
		settle func() error
	}
	attempts := []attempt{
		{"cap", func() error {
			return store.SettleRequest(ctx, "cap", Settlement{To: RequestUnknown, Reason: "capacity_rejected"})
		}},
		{"failover", func() error {
			return store.SettleRequest(ctx, "failover", Settlement{To: RequestUnknown, Reason: "provider_error"})
		}},
		{"answer", func() error { _, err := store.AppendAnswered(ctx, "s", "answer", assistant("done")); return err }},
	}
	var admissions, settlements int64
	for i, a := range attempts {
		adm := mainAdmission("s", a.id)
		adm.Attempt = i
		c, _ := writes(func() { admit(t, store, adm) })
		admissions += c
		c, _ = writes(func() {
			if err := a.settle(); err != nil {
				t.Fatalf("%s: %v", a.id, err)
			}
		})
		settlements += c
	}
	if admissions != 3 || settlements != 3 {
		t.Fatalf("admission transactions = %d, settlement transactions = %d, want 3 and 3", admissions, settlements)
	}
	if n := count(t, base, `SELECT count(*) FROM request_envelopes WHERE session_id = 's'`); n != 3 {
		t.Fatalf("%d envelopes, want one per attempt", n)
	}
}
