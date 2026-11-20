package localfs

import (
	"testing"
	"time"
)

// TestPostgresDuplicateKeyDetected exercises the lock insert path against a real
// PostgreSQL server. A second insert of the same (resource, lock_id) must be
// reported by isDuplicateKeyErr, which is what turns it into a Conflict error
// instead of an opaque 500.
func TestPostgresDuplicateKeyDetected(t *testing.T) {
	fs := newPGFS(t)
	ctx := t.Context()
	path := "/res/dup.txt"
	exp := time.Now().Add(time.Hour).UnixNano()

	insert := func() error {
		_, err := fs.db.ExecContext(ctx,
			fs.q("INSERT INTO resource_locks (resource, lock_id, lock_json, expires_at) VALUES (?, ?, ?, ?)"),
			path, "same", "{}", exp,
		)
		return err
	}

	if err := insert(); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := insert()
	if err == nil {
		t.Fatal("duplicate insert succeeded, want unique violation")
	}
	if !isDuplicateKeyErr(err) {
		t.Errorf("isDuplicateKeyErr(%v) = false, want true", err)
	}
}

// TestPostgresExpirySweep checks the DELETE ... expires_at<=? sweep that SetLock
// runs before inserting, using a nanosecond value that overflows int32.
func TestPostgresExpirySweep(t *testing.T) {
	fs := newPGFS(t)
	ctx := t.Context()
	path := "/res/sweep.txt"

	insert := func(lockID string, exp int64) {
		t.Helper()
		if _, err := fs.db.ExecContext(ctx,
			fs.q("INSERT INTO resource_locks (resource, lock_id, lock_json, expires_at) VALUES (?, ?, ?, ?)"),
			path, lockID, "{}", exp,
		); err != nil {
			t.Fatalf("insert %s: %v", lockID, err)
		}
	}

	insert("old", time.Now().Add(-time.Hour).UnixNano())
	insert("live", time.Now().Add(time.Hour).UnixNano())

	now := time.Now().UnixNano()
	if _, err := fs.db.ExecContext(ctx,
		fs.q("DELETE FROM resource_locks WHERE resource=? AND expires_at<=?"), path, now,
	); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	locks, err := fs.getLocksAtPath(ctx, path)
	if err != nil {
		t.Fatalf("getLocksAtPath: %v", err)
	}
	if len(locks) != 1 {
		t.Fatalf("got %d locks, want 1 (only the live one)", len(locks))
	}
}
