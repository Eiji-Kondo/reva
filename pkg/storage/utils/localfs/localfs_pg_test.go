package localfs

import (
	"database/sql"
	"os"
	"testing"

	"github.com/cs3org/reva/v3/cmd/revad/pkg/config"
)

// pgDSN is set by the test environment to run the dialect tests against a real
// PostgreSQL server, e.g.
//
//	REVA_TEST_POSTGRES="host=127.0.0.1 port=5432 user=postgres password=test dbname=reva sslmode=disable"
//
// The tests are skipped when it is unset so the default suite stays hermetic.
func pgDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("REVA_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("REVA_TEST_POSTGRES not set")
	}
	return dsn
}

func newPGFS(t *testing.T) *localfs {
	t.Helper()
	dsn := pgDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Start from a clean schema so the test is repeatable.
	for _, stmt := range []string{
		"DROP TABLE IF EXISTS recycled_entries",
		"DROP TABLE IF EXISTS user_interaction",
		"DROP TABLE IF EXISTS metadata",
		"DROP TABLE IF EXISTS share_references",
		"DROP TABLE IF EXISTS resource_locks",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("drop: %v", err)
		}
	}
	if err := migrate(db, dialectPostgres); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &localfs{db: db, dialect: dialectPostgres}
}

func TestQPostgres(t *testing.T) {
	fs := &localfs{dialect: dialectPostgres}
	got := fs.q("SELECT path FROM recycled_entries WHERE `key`=? AND x=?")
	want := `SELECT path FROM recycled_entries WHERE "key"=$1 AND x=$2`
	if got != want {
		t.Errorf("q() = %q, want %q", got, want)
	}
}

func TestQOtherDialectsUntouched(t *testing.T) {
	for _, d := range []dbDialect{dialectSQLite, dialectMySQL} {
		fs := &localfs{dialect: d}
		in := "SELECT `key` FROM t WHERE a=? AND b=?"
		if got := fs.q(in); got != in {
			t.Errorf("dialect %d: q() rewrote %q to %q", d, in, got)
		}
	}
}

func TestPostgresSchemaAndUpsert(t *testing.T) {
	fs := newPGFS(t)
	ctx := t.Context()

	if err := fs.addToACLDB(ctx, "/res", "user1", "owner"); err != nil {
		t.Fatalf("addToACLDB insert: %v", err)
	}
	// Second call must take the ON CONFLICT DO UPDATE path, not fail.
	if err := fs.addToACLDB(ctx, "/res", "user1", "editor"); err != nil {
		t.Fatalf("addToACLDB upsert: %v", err)
	}

	rows, err := fs.getACLs(ctx, "/res")
	if err != nil {
		t.Fatalf("getACLs: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var grantee, role string
		if err := rows.Scan(&grantee, &role); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if role != "editor" {
			t.Errorf("role = %q, want editor (upsert did not update)", role)
		}
		n++
	}
	if n != 1 {
		t.Errorf("got %d ACL rows, want 1", n)
	}
}

func TestPostgresMetadataAndRecycle(t *testing.T) {
	fs := newPGFS(t)
	ctx := t.Context()

	// metadata uses the reserved words key/value, which need quoting.
	if err := fs.addToMetadataDB(ctx, "/res", "k1", "v1"); err != nil {
		t.Fatalf("addToMetadataDB: %v", err)
	}
	if err := fs.addToMetadataDB(ctx, "/res", "k1", "v2"); err != nil {
		t.Fatalf("addToMetadataDB upsert: %v", err)
	}
	rows, err := fs.getMetadata(ctx, "/res")
	if err != nil {
		t.Fatalf("getMetadata: %v", err)
	}
	defer rows.Close()
	var got string
	found := 0
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if k == "k1" {
			got = v
			found++
		}
	}
	if found != 1 || got != "v2" {
		t.Errorf("metadata k1 = %q (%d rows), want v2 (1 row)", got, found)
	}

	if err := fs.addToRecycledDB(ctx, "rk", "file.txt"); err != nil {
		t.Fatalf("addToRecycledDB: %v", err)
	}
	got, err = fs.getRecycledEntry(ctx, "rk")
	if err != nil {
		t.Fatalf("getRecycledEntry: %v", err)
	}
	if got != "file.txt" {
		t.Errorf("recycled = %q, want file.txt", got)
	}
}

func TestPostgresShareReferences(t *testing.T) {
	fs := newPGFS(t)
	ctx := t.Context()

	if err := fs.addToReferencesDB(ctx, "/res", "/target1"); err != nil {
		t.Fatalf("addToReferencesDB: %v", err)
	}
	if err := fs.addToReferencesDB(ctx, "/res", "/target2"); err != nil {
		t.Fatalf("addToReferencesDB upsert: %v", err)
	}
	got, err := fs.getReferenceEntry(ctx, "/res")
	if err != nil {
		t.Fatalf("getShareReferenceDB: %v", err)
	}
	if got != "/target2" {
		t.Errorf("target = %q, want /target2", got)
	}
}

func TestPostgresOpenDB(t *testing.T) {
	pgDSN(t)
	cfg := &Config{
		Root:       t.TempDir(),
		DBEngine:   "postgres",
		DBHost:     "127.0.0.1",
		DBPort:     5432,
		DBUsername: "postgres",
		DBPassword: "test",
		DBName:     "reva",
	}
	dialect, db, err := openDB(cfg)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	if dialect != dialectPostgres {
		t.Errorf("dialect = %v, want dialectPostgres", dialect)
	}
}

var _ = config.Database{}
