//go:build cgo

package runtime

import (
	"database/sql"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mattn/go-sqlite3"
)

// A test-only count of committed write transactions per state database: a ConnectHook on the driver
// database/sql registered as "sqlite3" registers a commit hook on every connection opened afterwards
// and counts it under the connection's main database file. Production code has no counter. Counting
// per file keeps engines of other tests in the same binary out of the count.

var (
	commitMu      sync.Mutex
	commitCounts  = map[string]*atomic.Int64{}
	commitHookErr error
	commitOnce    sync.Once
)

func commitCounter(path string) *atomic.Int64 {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	commitMu.Lock()
	defer commitMu.Unlock()
	c, ok := commitCounts[path]
	if !ok {
		c = &atomic.Int64{}
		commitCounts[path] = c
	}
	return c
}

// installCommitCounter installs the hook once per test binary. It must run before the engine whose
// commits are counted opens its pool.
func installCommitCounter(t *testing.T) {
	t.Helper()
	commitOnce.Do(func() {
		db, err := sql.Open("sqlite3", ":memory:")
		if err != nil {
			commitHookErr = err
			return
		}
		drv, ok := db.Driver().(*sqlite3.SQLiteDriver)
		db.Close()
		if !ok {
			commitHookErr = sql.ErrConnDone
			return
		}
		drv.ConnectHook = func(c *sqlite3.SQLiteConn) error {
			counter := commitCounter(c.GetFilename("main"))
			c.RegisterCommitHook(func() int { counter.Add(1); return 0 })
			return nil
		}
	})
	if commitHookErr != nil {
		t.Fatalf("commit counter not installed: %v", commitHookErr)
	}
}

// commitsOn returns the commits counted so far on the database file path.
func commitsOn(path string) int64 { return commitCounter(path).Load() }
