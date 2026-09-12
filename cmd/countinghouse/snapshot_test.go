package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sweeney/countinghouse/internal/prices"
)

// ---------------------------------------------------------------------------
// Taking a consistent snapshot of the archive.
//
// The archive is opened in WAL mode (common/db sets `PRAGMA journal_mode=WAL`), which
// means recent writes live in a separate `-wal` file until a checkpoint folds them into
// the main database. `identity/common/backup@v0.3.0` copies the archive with
// `os.ReadFile` + `os.WriteFile` on the main file alone:
//
//	func copyDB(src, dst string) error {
//	    data, err := os.ReadFile(src)
//	    if err != nil { return err }
//	    return os.WriteFile(dst, data, 0600)
//	}
//
// So the uploaded object is the database as of the last checkpoint — staleness unbounded
// on a quiet archive that never reaches the 1000-page auto-checkpoint — and at worst a
// torn image if a checkpoint runs during the read. The whole argument for keeping this
// file is that it is primary durable state and not re-fetchable forever; a
// possibly-stale, possibly-torn copy is not a backup of it.
//
// `VACUUM INTO` is one statement, is pure SQL (so the CGO-free build survives), and
// writes a complete, consistent, WAL-free database — safely, while the collector is
// still writing.
//
// The fix belongs upstream, where it would also fix identity and config. Until that is
// released, countinghouse takes its own snapshot rather than shipping a README sentence
// that the code does not honour.
// ---------------------------------------------------------------------------

// archiveWithUncheckpointedWrites builds a real price archive holding `n` slots and
// returns its path, deliberately leaving the writes in the WAL.
func archiveWithUncheckpointedWrites(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.db")

	store, err := prices.Open(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}

	from := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	slots := make([]prices.Slot, 0, n)
	for i := 0; i < n; i++ {
		f := from.Add(time.Duration(i) * prices.SlotLength)
		to := f.Add(prices.SlotLength)
		slots = append(slots, prices.Slot{
			TariffCode: "E-1R-AGILE-24-10-01-A", ValidFrom: f, ValidTo: &to,
			ExcVATPence: 20, IncVATPence: 21, RetrievedAt: f,
		})
	}
	if _, err := store.Put(context.Background(), slots); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Deliberately NOT closed and NOT checkpointed: this is the live state the backup
	// has to cope with. Closing would checkpoint and hide the bug entirely.
	t.Cleanup(func() { store.Close() }) //nolint:errcheck
	return path
}

// countSlots opens a database file read-only and counts archived slots.
func countSlots(t *testing.T, path string) int {
	t.Helper()
	n, err := trySlots(path)
	if err != nil {
		t.Fatalf("count in %s: %v", path, err)
	}
	return n
}

// trySlots is countSlots without the fatal, for asserting that a file is NOT usable.
func trySlots(path string) (int, error) {
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		return 0, err
	}
	defer db.Close() //nolint:errcheck
	var n int
	err = db.QueryRow("SELECT COUNT(*) FROM unit_price").Scan(&n)
	return n, err
}

// The headline assertion: a snapshot taken while the archive is live must contain
// everything the archive holds, including writes still sitting in the WAL.
func TestSnapshotIncludesUncheckpointedWrites(t *testing.T) {
	const slots = 200
	src := archiveWithUncheckpointedWrites(t, slots)

	// Demonstrate the premise rather than assuming it: reproduce exactly what
	// common/backup@v0.3.0 does, and show what it produces.
	naive := filepath.Join(t.TempDir(), "naive.db")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(naive, data, 0o600); err != nil {
		t.Fatal(err)
	}
	switch got, err := trySlots(naive); {
	case err != nil:
		// The stronger result, and what actually happens on a fresh archive: the
		// SCHEMA is still in the WAL too, so the copy is a database with no tables in
		// it at all. Not a stale backup — an unusable one.
		t.Logf("a plain file copy is not even readable: %v", err)
	case got == slots:
		t.Skip("the archive auto-checkpointed, so a plain file copy happens to be " +
			"complete here; this test cannot demonstrate the difference on this platform")
	default:
		t.Logf("a plain file copy holds %d of %d slots — the rest are still in the WAL", got, slots)
	}

	dst := filepath.Join(t.TempDir(), "snapshot.db")
	if err := snapshotDB(src, dst); err != nil {
		t.Fatalf("snapshotDB: %v", err)
	}

	if got := countSlots(t, dst); got != slots {
		t.Errorf("snapshot holds %d of %d slots; a backup missing recent writes is not a "+
			"backup of the archive", got, slots)
	}
}

// The snapshot must be a standalone database: no -wal or -shm companion needed to read
// it, because the restore path is "download one object and open it".
func TestSnapshotIsSelfContained(t *testing.T) {
	src := archiveWithUncheckpointedWrites(t, 50)
	dst := filepath.Join(t.TempDir(), "snapshot.db")
	if err := snapshotDB(src, dst); err != nil {
		t.Fatal(err)
	}

	for _, companion := range []string{dst + "-wal", dst + "-shm"} {
		if _, err := os.Stat(companion); err == nil {
			t.Errorf("%s exists; the snapshot must be a single self-contained file", companion)
		}
	}
	if got := countSlots(t, dst); got != 50 {
		t.Errorf("snapshot holds %d slots, want 50", got)
	}
}

// The snapshot carries the archive's file mode. It is a full copy of the price history,
// and a world-readable one in /tmp would undo the 0600 the live file is created with.
func TestSnapshotIsNotWorldReadable(t *testing.T) {
	src := archiveWithUncheckpointedWrites(t, 10)
	dst := filepath.Join(t.TempDir(), "snapshot.db")
	if err := snapshotDB(src, dst); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("snapshot mode = %04o, want 0600", perm)
	}
}

// VACUUM INTO refuses an existing destination, so the caller must clear it — otherwise
// the second backup of the process's life fails and the first is the only one there is.
func TestSnapshotOverwritesAnExistingDestination(t *testing.T) {
	src := archiveWithUncheckpointedWrites(t, 10)
	dst := filepath.Join(t.TempDir(), "snapshot.db")

	if err := os.WriteFile(dst, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := snapshotDB(src, dst); err != nil {
		t.Fatalf("snapshotDB over an existing file: %v — VACUUM INTO refuses a path that "+
			"already exists, so the caller has to clear it", err)
	}
	if got := countSlots(t, dst); got != 10 {
		t.Errorf("snapshot holds %d slots, want 10", got)
	}
}

// A missing source is an error, not an empty backup. Uploading an empty database over a
// good one is the worst available outcome.
func TestSnapshotOfAMissingArchiveFails(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "snapshot.db")
	if err := snapshotDB(filepath.Join(t.TempDir(), "nope.db"), dst); err == nil {
		t.Error("snapshotting a missing archive succeeded; an empty backup must never be " +
			"uploaded in place of a real one")
	}
}
