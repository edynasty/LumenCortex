package migrate

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	upSet2   = map[string]string{"0002_t.sql": `CREATE TABLE t2(x INTEGER) STRICT;`}
	downSet2 = map[string]string{"0002_t.down.sql": `DROP TABLE t2;`}
)

// stateFiles are the files a refused schema change must leave byte-identical.
var stateFiles = []string{dbFile, dbFile + "-wal"}

func TestLockWaitIsThirtySeconds(t *testing.T) {
	if LockWait != 30*time.Second {
		t.Fatalf("LockWait=%v, the approved maximum wait is 30s", LockWait)
	}
}

// TestSharedHolderBlocksUpgrade: while another handle holds db.lock shared, a migration cannot take
// it exclusively. Open fails with ErrDatabaseInUse after LockWait, writes nothing, and succeeds once
// the holder closes.
func TestSharedHolderBlocksUpgrade(t *testing.T) {
	shorten(t, 200*time.Millisecond, 0)
	dir := newFixture(t, fixtureGo)
	holder := open(t, dir, embeddedSet(t))
	hashes := fileHashes(t, dir, stateFiles...)
	backupsBefore := backups(t, dir)
	next := syntheticSet(t, upSet2, downSet2)

	start := time.Now()
	ws, err := Open(context.Background(), dir, next)
	if !errors.Is(err, ErrDatabaseInUse) {
		t.Fatalf("Open while db.lock is held shared: ws=%v err=%v, want ErrDatabaseInUse", ws, err)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Errorf("gave up after %v, before LockWait", elapsed)
	}
	if got := fileHashes(t, dir, stateFiles...); !reflect.DeepEqual(got, hashes) {
		t.Error("refused migration wrote to the database or WAL")
	}
	if got := backups(t, dir); !reflect.DeepEqual(got, backupsBefore) {
		t.Errorf("refused migration wrote backups: %v -> %v", backupsBefore, got)
	}
	closeWS(t, holder)

	ws = open(t, dir, next)
	defer closeWS(t, ws)
	if ws.From != 1 || ws.To != 2 {
		t.Errorf("after the holder closed: From=%d To=%d, want 1 -> 2", ws.From, ws.To)
	}
}

// TestSharedHolderBlocksDowngrade is the same for the rollback path.
func TestSharedHolderBlocksDowngrade(t *testing.T) {
	shorten(t, 200*time.Millisecond, 0)
	dir := newFixture(t, fixtureGo)
	set := syntheticSet(t, upSet2, downSet2)
	closeWS(t, open(t, dir, set))
	holder := open(t, dir, set)
	hashes := fileHashes(t, dir, stateFiles...)
	backupsBefore := backups(t, dir)

	_, _, err := Downgrade(context.Background(), dir, set, 1)
	if !errors.Is(err, ErrDatabaseInUse) {
		t.Fatalf("Downgrade while db.lock is held shared: err=%v, want ErrDatabaseInUse", err)
	}
	if got := fileHashes(t, dir, stateFiles...); !reflect.DeepEqual(got, hashes) {
		t.Error("refused downgrade wrote to the database or WAL")
	}
	if got := backups(t, dir); !reflect.DeepEqual(got, backupsBefore) {
		t.Errorf("refused downgrade wrote backups: %v -> %v", backupsBefore, got)
	}
	closeWS(t, holder)

	from, now, err := Downgrade(context.Background(), dir, set, 1)
	if err != nil || from != 2 || now != 1 {
		t.Fatalf("Downgrade after the holder closed: from=%d now=%d err=%v", from, now, err)
	}
}

// TestSharedOpensCoexist: a second Open of an up-to-date database succeeds while the first is open.
func TestSharedOpensCoexist(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	set := embeddedSet(t)
	w1 := open(t, dir, set)
	w2 := open(t, dir, set)
	if w1.From != 0 || w1.To != 1 || w2.From != 1 || w2.To != 1 {
		t.Errorf("w1 %d->%d, w2 %d->%d; want 0->1 and 1->1 (no second migration)", w1.From, w1.To, w2.From, w2.To)
	}
	closeWS(t, w2)
	closeWS(t, w1)
}

// TestWaitHonorsContext: a context cancelled while waiting for db.lock returns context.Canceled
// promptly and writes nothing, even though LockWait is far from expiring.
func TestWaitHonorsContext(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	holder := open(t, dir, embeddedSet(t))
	defer closeWS(t, holder)
	hashes := fileHashes(t, dir, stateFiles...)
	next := syntheticSet(t, upSet2, downSet2)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := Open(ctx, dir, next)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Open with a cancelled context: err=%v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("cancellation took %v", elapsed)
	}
	if got := fileHashes(t, dir, stateFiles...); !reflect.DeepEqual(got, hashes) {
		t.Error("cancelled wait wrote to the database or WAL")
	}

	// Downgrade waits the same way.
	set := syntheticSet(t, upSet2, downSet2)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if _, _, err := Downgrade(ctx2, dir, set, 1); !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrDatabaseInUse) {
		t.Errorf("Downgrade waiting for a shared holder: err=%v", err)
	}
}

// TestConcurrentOpen opens one old database from several goroutines at once (each Open takes its own
// descriptor of db.lock, as separate processes would). Every Open succeeds, exactly one migrates and
// writes one backup, and no .partial is left.
func TestConcurrentOpen(t *testing.T) {
	const rounds, openers = 15, 4
	for r := range rounds {
		dir := newFixture(t, fixtureGo)
		set := embeddedSet(t)
		start := make(chan struct{})
		ws := make([]*Workspace, openers)
		errs := make([]error, openers)
		var wg sync.WaitGroup
		for i := range openers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ws[i], errs[i] = Open(context.Background(), dir, set)
			}()
		}
		close(start)
		wg.Wait()
		migrated := 0
		for i, err := range errs {
			if err != nil {
				t.Errorf("round %d opener %d: %v", r, i, err)
				continue
			}
			if ws[i].To != 1 {
				t.Errorf("round %d opener %d: To=%d", r, i, ws[i].To)
			}
			if ws[i].From == 0 {
				migrated++
			}
		}
		if migrated < 1 {
			t.Errorf("round %d: nobody reports migrating", r)
		}
		if got := backups(t, dir); len(got) != 1 || strings.HasSuffix(got[0], ".partial") {
			t.Errorf("round %d: backups=%v, want exactly one durable backup", r, got)
		}
		for _, w := range ws {
			if w != nil {
				closeWS(t, w)
			}
		}
	}
}

const (
	helperEnv     = "LCX_MIGRATE_HELPER"
	helperDirEnv  = "LCX_MIGRATE_HELPER_DIR"
	helperHold    = "hold"
	helperOpen    = "open"
	helperCrashAt = "crash-after-commit"
)

// TestHelperProcess is the child of the cross-process tests; it does nothing in a normal run.
//
//	hold:               Open (holding db.lock shared), report "held", sleep until killed.
//	open:               Open, report "from to", Close, exit 0.
//	crash-after-commit: migrate and die with exit 9 right after the first migration commits.
func TestHelperProcess(t *testing.T) {
	mode, dir := os.Getenv(helperEnv), os.Getenv(helperDirEnv)
	if mode == "" {
		t.Skip("helper process only")
	}
	if mode == helperCrashAt {
		testHookAfterCommit = func(int) { os.Exit(9) }
	}
	set, err := Embedded()
	if err != nil {
		fmt.Println("error", err)
		os.Exit(2)
	}
	ws, err := Open(context.Background(), dir, set)
	if err != nil {
		fmt.Println("error", err)
		os.Exit(3)
	}
	switch mode {
	case helperHold:
		fmt.Println("held")
		time.Sleep(5 * time.Minute)
		os.Exit(4)
	case helperOpen:
		fmt.Println("opened", ws.From, ws.To)
		if err := ws.Close(); err != nil {
			fmt.Println("error", err)
			os.Exit(5)
		}
		os.Exit(0)
	}
	os.Exit(6)
}

func helper(t *testing.T, mode, dir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, helperDirEnv+"="+dir)
	cmd.Stderr = os.Stderr
	return cmd
}

// TestSharedLockCrossProcess: a shared holder in another process refuses a migration; kill -9
// releases the kernel lock and the migration then succeeds.
func TestSharedLockCrossProcess(t *testing.T) {
	shorten(t, 300*time.Millisecond, 0)
	dir := newFixture(t, fixtureGo)
	closeWS(t, open(t, dir, embeddedSet(t)))

	cmd := helper(t, helperHold, dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(out).ReadString('\n')
		line <- s
	}()
	select {
	case s := <-line:
		if s != "held\n" {
			t.Fatalf("helper said %q", s)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("helper did not report held")
	}

	next := syntheticSet(t, upSet2, downSet2)
	if _, err := Open(context.Background(), dir, next); !errors.Is(err, ErrDatabaseInUse) {
		t.Fatalf("Open while a child process holds db.lock shared: err=%v, want ErrDatabaseInUse", err)
	}
	if _, _, err := Downgrade(context.Background(), dir, next, 0); err == nil || errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("Downgrade while a child holds db.lock: err=%v", err)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	ws := open(t, dir, next)
	defer closeWS(t, ws)
	if ws.From != 1 || ws.To != 2 {
		t.Errorf("after kill -9 of the holder: From=%d To=%d, want 1 -> 2", ws.From, ws.To)
	}
}

// TestConcurrentProcessOpen: several processes open the same old database at once. All succeed, one
// backup is written, and the database is at the latest version.
func TestConcurrentProcessOpen(t *testing.T) {
	const procs = 4
	for r := range 3 {
		dir := newFixture(t, fixtureGo)
		cmds := make([]*exec.Cmd, procs)
		outs := make([]*bytes.Buffer, procs)
		for i := range procs {
			cmds[i] = helper(t, helperOpen, dir)
			outs[i] = &bytes.Buffer{}
			cmds[i].Stdout = outs[i]
		}
		for _, c := range cmds {
			if err := c.Start(); err != nil {
				t.Fatal(err)
			}
		}
		for i, c := range cmds {
			if err := c.Wait(); err != nil {
				t.Errorf("round %d process %d: %v: %s", r, i, err, outs[i])
			}
			if !strings.HasPrefix(outs[i].String(), "opened ") || !strings.HasSuffix(strings.TrimSpace(outs[i].String()), " 1") {
				t.Errorf("round %d process %d said %q", r, i, outs[i])
			}
		}
		if got := backups(t, dir); len(got) != 1 || strings.HasSuffix(got[0], ".partial") {
			t.Errorf("round %d: backups=%v, want exactly one durable backup", r, got)
		}
		if hits := filesWithSentinel(t, dir); len(hits) != 0 {
			t.Errorf("round %d: reasoning bytes in %v", r, hits)
		}
		if _, err := os.Stat(filepath.Join(dir, pendingMarker)); !os.IsNotExist(err) {
			t.Errorf("round %d: marker left (stat: %v)", r, err)
		}
	}
}

// TestCrashAfterCommitIsRecovered: a process killed after the migration committed but before the
// rebuild and checkpoint leaves checkpoint.pending behind. The next Open, under the exclusive lock,
// finishes the work: no reasoning bytes remain in the database, WAL or backup, and the marker is gone.
func TestCrashAfterCommitIsRecovered(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	cmd := helper(t, helperCrashAt, dir)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 9 {
		t.Fatalf("helper: err=%v out=%s, want exit 9", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, pendingMarker)); err != nil {
		t.Fatalf("checkpoint.pending missing after the crash: %v", err)
	}
	if uv := qInt(t, inspect(t, filepath.Join(dir, dbFile)), `PRAGMA user_version`); uv != 1 {
		t.Fatalf("user_version=%d after the crash, want 1 (the migration committed)", uv)
	}

	ws := open(t, dir, embeddedSet(t))
	if ws.From != 1 || ws.To != 1 {
		t.Errorf("recovery open From=%d To=%d, want 1 -> 1", ws.From, ws.To)
	}
	closeWS(t, ws)
	if _, err := os.Stat(filepath.Join(dir, pendingMarker)); !os.IsNotExist(err) {
		t.Errorf("marker survived recovery (stat: %v)", err)
	}
	if hits := filesWithSentinel(t, dir); len(hits) != 0 {
		t.Errorf("reasoning bytes in %v after recovery", hits)
	}
}

// TestRecoveryDefersToSharedHolders: a pending rebuild must not block an Open while another handle
// holds db.lock; the Open succeeds and the marker stays for a later Open.
func TestRecoveryDefersToSharedHolders(t *testing.T) {
	shorten(t, 300*time.Millisecond, 0)
	dir := newFixture(t, fixtureGo)
	cmd := helper(t, helperCrashAt, dir)
	if err := cmd.Run(); err == nil {
		t.Fatal("helper should have crashed")
	}
	first := open(t, dir, embeddedSet(t)) // takes the exclusive lock, completes the rebuild
	if _, err := os.Stat(filepath.Join(dir, pendingMarker)); !os.IsNotExist(err) {
		t.Fatalf("marker left after the recovery open (stat: %v)", err)
	}
	closeWS(t, first)

	// Recreate the marker while a holder keeps db.lock shared.
	holder := open(t, dir, embeddedSet(t))
	defer closeWS(t, holder)
	if err := createMarker(dir, markerVacuum); err != nil {
		t.Fatal(err)
	}
	second, err := Open(context.Background(), dir, embeddedSet(t))
	if err != nil {
		t.Fatalf("Open with a pending rebuild and a shared holder: %v", err)
	}
	defer closeWS(t, second)
	if _, err := os.Stat(filepath.Join(dir, pendingMarker)); err != nil {
		t.Errorf("marker removed without the rebuild: %v", err)
	}
}

// TestBackupPartialCleanup: an interrupted backup attempt (.partial) is deleted by the next
// migration, and a durable backup is written.
func TestBackupPartialCleanup(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	bdir := filepath.Join(dir, "backups")
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(bdir, "lumencortex.db.v0.20200101T000000Z.partial")
	if err := os.WriteFile(planted, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	closeWS(t, open(t, dir, embeddedSet(t)))
	if _, err := os.Stat(planted); !os.IsNotExist(err) {
		t.Errorf("planted .partial survived the migration (stat err=%v)", err)
	}
	if got := backups(t, dir); len(got) != 1 || strings.HasSuffix(got[0], ".partial") {
		t.Errorf("backups=%v, want exactly one durable backup and no .partial", got)
	}
}

// TestBackupFailureLeavesNoAcceptedBackup: a failure after VACUUM INTO (an interrupted backup) deletes
// the .partial, leaves the live database untouched, and a later Open succeeds.
func TestBackupFailureLeavesNoAcceptedBackup(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	boom := errors.New("injected failure after VACUUM INTO")
	var sawPartial bool
	testHookAfterVACUUM = func(partial string) error {
		_, err := os.Stat(partial)
		sawPartial = err == nil
		return boom
	}
	t.Cleanup(func() { testHookAfterVACUUM = nil })

	if _, err := Open(context.Background(), dir, embeddedSet(t)); !errors.Is(err, boom) {
		t.Fatalf("Open with a failing backup: err=%v, want the injected error", err)
	}
	if !sawPartial {
		t.Error("the hook ran without a .partial on disk; the failure point is not after VACUUM INTO")
	}
	if uv := qInt(t, inspect(t, filepath.Join(dir, dbFile)), `PRAGMA user_version`); uv != 0 {
		t.Errorf("user_version=%d after a failed backup, want 0", uv)
	}
	for _, n := range backups(t, dir) {
		t.Errorf("backup %s left behind by a failed backup", n)
	}
	if _, err := os.Stat(filepath.Join(dir, pendingMarker)); !os.IsNotExist(err) {
		t.Errorf("marker left although nothing changed (stat: %v)", err)
	}

	testHookAfterVACUUM = nil
	ws := open(t, dir, embeddedSet(t))
	closeWS(t, ws)
	if got := backups(t, dir); len(got) != 1 || strings.HasSuffix(got[0], ".partial") {
		t.Errorf("backups after recovery=%v, want exactly one durable backup", got)
	}
}

// TestCheckpointPendingMarker: a reader that ignores db.lock and holds a snapshot makes the
// post-migration TRUNCATE checkpoint incomplete, so checkpoint.pending stays. Once the reader leaves,
// the next Open completes the checkpoint, removes the marker, and no reasoning bytes remain in the
// file or WAL.
func TestCheckpointPendingMarker(t *testing.T) {
	shorten(t, 5*time.Second, 50*time.Millisecond)
	dir := newFixture(t, fixtureGo)
	path := filepath.Join(dir, dbFile)

	rdb := inspect(t, path)
	tx, err := rdb.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}

	ws := open(t, dir, embeddedSet(t))
	if ws.From != 0 || ws.To != 1 {
		t.Fatalf("From=%d To=%d", ws.From, ws.To)
	}
	closeWS(t, ws)
	if _, err := os.Stat(filepath.Join(dir, pendingMarker)); err != nil {
		t.Fatalf("checkpoint.pending missing after an incomplete checkpoint: %v", err)
	}
	kind, err := readMarker(dir)
	if err != nil || kind != markerCheckpoint {
		t.Errorf("marker kind=%q err=%v, want the plain checkpoint kind after the rebuild", kind, err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	ws = open(t, dir, embeddedSet(t))
	if ws.From != 1 || ws.To != 1 {
		t.Errorf("From=%d To=%d, want 1 -> 1", ws.From, ws.To)
	}
	closeWS(t, ws)
	if _, err := os.Stat(filepath.Join(dir, pendingMarker)); !os.IsNotExist(err) {
		t.Fatalf("checkpoint.pending not removed after the reader left (stat: %v)", err)
	}
	if hits := filesWithSentinel(t, dir); len(hits) != 0 {
		t.Errorf("reasoning bytes in %v after the completed checkpoint", hits)
	}
}

// TestReasoningInLegacyWALIsTruncated: the previous owner left its rows, reasoning included, only in the
// WAL. Migrating folds them into the main file and the post-migration TRUNCATE checkpoint must cut the
// WAL itself: a checkpoint that only copies frames (PASSIVE) leaves the old frames readable on disk.
func TestReasoningInLegacyWALIsTruncated(t *testing.T) {
	for _, kind := range allFixtures {
		t.Run(string(kind), func(t *testing.T) {
			dir := newFixtureWithWAL(t, kind)
			wal, err := os.ReadFile(filepath.Join(dir, dbFile+"-wal"))
			if err != nil || !bytes.Contains(wal, []byte(sentinel)) {
				t.Fatalf("fixture WAL does not hold the reasoning sentinel (err=%v); the test would prove nothing", err)
			}
			// A bystander that ignores db.lock keeps a connection open, so SQLite does not
			// checkpoint and delete the WAL when the migrator closes its own connections.
			bystander := inspect(t, filepath.Join(dir, dbFile))
			qInt(t, bystander, `SELECT count(*) FROM sessions`)

			ws := open(t, dir, embeddedSet(t))
			if ws.From != 0 || ws.To != 1 {
				t.Fatalf("From=%d To=%d", ws.From, ws.To)
			}
			if n := qInt(t, ws.DB, `SELECT count(*) FROM session_messages`); n == 0 {
				t.Error("transcript rows held only in the WAL were lost")
			}
			if hits := filesWithSentinel(t, dir); len(hits) != 0 {
				t.Errorf("reasoning bytes in %v while the workspace is open", hits)
			}
			closeWS(t, ws)
			if hits := filesWithSentinel(t, dir); len(hits) != 0 {
				t.Errorf("reasoning bytes in %v after Close (bystander still open)", hits)
			}
		})
	}
}
