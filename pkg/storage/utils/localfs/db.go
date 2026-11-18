// Copyright 2018-2024 CERN
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// In applying this license, CERN does not waive the privileges and immunities
// granted to it by virtue of its status as an Intergovernmental Organization
// or submit itself to any jurisdiction.

package localfs

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"strconv"
	"strings"

	_ "github.com/glebarez/go-sqlite"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pkg/errors"

	"github.com/cs3org/reva/v3/cmd/revad/pkg/config"
	"github.com/cs3org/reva/v3/pkg/sharedconf"
)

// dbDialect selects the SQL dialect used for schema and upsert statements.
// sqlite and MySQL/MariaDB differ in reserved words, column types and the
// upsert clause, so the statements are chosen at runtime.
type dbDialect int

const (
	dialectSQLite dbDialect = iota
	dialectMySQL
	dialectPostgres
)

const (
	// One short query at a time per request. A handful of connections covers
	// concurrent Stat calls; more only adds contention on sqlite locks.
	maxOpenConns = 8
	maxIdleConns = 8
)

// openDB opens the localfs metadata DB. When the engine is unset it follows the
// shared DB engine, so a deployment that moves [shared] to mysql moves localfs
// too. An unset or sqlite engine keeps the embedded per-root sqlite file.
func openDB(c *Config) (dbDialect, *sql.DB, error) {
	engine := c.DBEngine
	if engine == "" {
		engine = sharedconf.GetDBInfo(config.Database{}).Engine
	}

	if engine == "" || engine == "sqlite" {
		dbName := "localfs.db"
		if !c.DisableHome {
			dbName = "localhomefs.db"
		}
		db, err := initializeDB(c.Root, dbName)
		if err != nil {
			return dialectSQLite, nil, err
		}
		setPoolLimits(db)
		return dialectSQLite, db, nil
	}

	info := sharedconf.GetDBInfo(config.Database{
		Engine:     engine,
		DBUsername: c.DBUsername,
		DBPassword: c.DBPassword,
		DBHost:     c.DBHost,
		DBPort:     c.DBPort,
		DBName:     c.DBName,
	})
	if engine == "postgres" {
		db, err := initializePostgresDB(info)
		if err != nil {
			return dialectPostgres, nil, err
		}
		setPoolLimits(db)
		return dialectPostgres, db, nil
	}
	db, err := initializeMySQLDB(info)
	if err != nil {
		return dialectMySQL, nil, err
	}
	setPoolLimits(db)
	return dialectMySQL, db, nil
}

// setPoolLimits bounds the connection pool. database/sql puts no finalizer on
// Rows and only Rows.close releases the connection, so an abandoned result set
// holds a connection until the process exits. Uncapped, one leak path can take
// the service down: on a deployment this showed up as MariaDB answering 1040
// Too many connections to every client, which failed authentication and turned
// every request into a 401. The cap bounds the damage instead.
func setPoolLimits(db *sql.DB) {
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
}

func initializeDB(root, dbName string) (*sql.DB, error) {
	dbPath := path.Join(root, dbName)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, errors.Wrap(err, "localfs: error opening DB connection")
	}
	if err := migrate(db, dialectSQLite); err != nil {
		return nil, err
	}
	return db, nil
}

func initializeMySQLDB(info config.Database) (*sql.DB, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s",
		info.DBUsername, info.DBPassword, info.DBHost, info.DBPort, info.DBName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, errors.Wrap(err, "localfs: error opening MySQL DB connection")
	}
	if err := migrate(db, dialectMySQL); err != nil {
		return nil, err
	}
	return db, nil
}

func initializePostgresDB(info config.Database) (*sql.DB, error) {
	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		info.DBHost, info.DBPort, info.DBUsername, info.DBPassword, info.DBName)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, errors.Wrap(err, "localfs: error opening PostgreSQL DB connection")
	}
	if err := migrate(db, dialectPostgres); err != nil {
		return nil, err
	}
	return db, nil
}

func migrate(db *sql.DB, dialect dbDialect) error {
	for _, stmt := range schema(dialect) {
		if _, err := db.Exec(stmt); err != nil {
			return errors.Wrap(err, "localfs: error creating schema")
		}
	}
	return nil
}

// schema returns the DDL for the metadata DB. MySQL/MariaDB needs VARCHAR keys
// (TEXT cannot be a PRIMARY KEY), backticked reserved words (`key`, `value`) and
// BIGINT for expires_at (a UnixNano value overflows a 32-bit INTEGER).
// PostgreSQL accepts TEXT keys and BIGINT directly, but - unlike SQLite -
// it rejects unquoted reserved words, so key/value are double quoted.
func schema(dialect dbDialect) []string {
	if dialect == dialectPostgres {
		return []string{
			`CREATE TABLE IF NOT EXISTS recycled_entries (key TEXT PRIMARY KEY, path TEXT)`,
			`CREATE TABLE IF NOT EXISTS user_interaction (resource TEXT, grantee TEXT, role TEXT DEFAULT '', favorite INTEGER DEFAULT 0, PRIMARY KEY (resource, grantee))`,
			`CREATE TABLE IF NOT EXISTS metadata (resource TEXT, "key" TEXT, "value" TEXT, PRIMARY KEY (resource, "key"))`,
			`CREATE TABLE IF NOT EXISTS share_references (resource TEXT PRIMARY KEY, target TEXT)`,
			`CREATE TABLE IF NOT EXISTS resource_locks (resource TEXT NOT NULL, lock_id TEXT NOT NULL, lock_json TEXT NOT NULL, expires_at BIGINT NOT NULL, PRIMARY KEY (resource, lock_id))`,
		}
	}
	if dialect == dialectMySQL {
		return []string{
			"CREATE TABLE IF NOT EXISTS recycled_entries (`key` VARCHAR(255) PRIMARY KEY, path TEXT)",
			"CREATE TABLE IF NOT EXISTS user_interaction (resource VARCHAR(255), grantee VARCHAR(255), role TEXT, favorite INT DEFAULT 0, PRIMARY KEY (resource, grantee))",
			"CREATE TABLE IF NOT EXISTS metadata (resource VARCHAR(255), `key` VARCHAR(255), `value` TEXT, PRIMARY KEY (resource, `key`))",
			"CREATE TABLE IF NOT EXISTS share_references (resource VARCHAR(255) PRIMARY KEY, target TEXT)",
			"CREATE TABLE IF NOT EXISTS resource_locks (resource VARCHAR(255) NOT NULL, lock_id VARCHAR(255) NOT NULL, lock_json TEXT NOT NULL, expires_at BIGINT NOT NULL, PRIMARY KEY (resource, lock_id))",
		}
	}
	return []string{
		"CREATE TABLE IF NOT EXISTS recycled_entries (key TEXT PRIMARY KEY, path TEXT)",
		"CREATE TABLE IF NOT EXISTS user_interaction (resource TEXT, grantee TEXT, role TEXT DEFAULT '', favorite INTEGER DEFAULT 0, PRIMARY KEY (resource, grantee))",
		"CREATE TABLE IF NOT EXISTS metadata (resource TEXT, key TEXT, value TEXT, PRIMARY KEY (resource, key))",
		"CREATE TABLE IF NOT EXISTS share_references (resource TEXT PRIMARY KEY, target TEXT)",
		"CREATE TABLE IF NOT EXISTS resource_locks (resource TEXT NOT NULL, lock_id TEXT NOT NULL, lock_json TEXT NOT NULL, expires_at INTEGER NOT NULL, PRIMARY KEY (resource, lock_id))",
	}
}

// sql picks the statement for the connection's dialect. PostgreSQL shares
// the SQLite statements because both implement the standard ON CONFLICT
// upsert clause.
func (fs *localfs) sql(sqliteStmt, mysqlStmt string) string {
	if fs.dialect == dialectMySQL {
		return mysqlStmt
	}
	return sqliteStmt
}

// q adapts a statement to the connection's dialect. PostgreSQL is the only
// backend that rejects ? placeholders (it wants $1..$N) and backquoted
// identifiers (it wants double quotes), so the rewrite is a no-op elsewhere.
func (fs *localfs) q(query string) string {
	if fs.dialect != dialectPostgres {
		return query
	}
	query = strings.ReplaceAll(query, "`", "\"")
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] != '?' {
			b.WriteByte(query[i])
			continue
		}
		n++
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(n))
	}
	return b.String()
}

func (fs *localfs) addToRecycledDB(ctx context.Context, key, fileName string) error {
	stmt, err := fs.db.Prepare(fs.q("INSERT INTO recycled_entries VALUES (?, ?)"))
	if err != nil {
		return errors.Wrap(err, "localfs: error preparing statement")
	}
	_, err = stmt.Exec(key, fileName)
	if err != nil {
		return errors.Wrap(err, "localfs: error executing insert statement")
	}
	return nil
}

func (fs *localfs) getRecycledEntry(ctx context.Context, key string) (string, error) {
	var filePath string
	err := fs.db.QueryRow(fs.q("SELECT path FROM recycled_entries WHERE `key`=?"), key).Scan(&filePath)
	if err != nil {
		return "", err
	}
	return filePath, nil
}

func (fs *localfs) removeFromRecycledDB(ctx context.Context, key string) error {
	stmt, err := fs.db.Prepare(fs.q("DELETE FROM recycled_entries WHERE `key`=?"))
	if err != nil {
		return errors.Wrap(err, "localfs: error preparing statement")
	}
	_, err = stmt.Exec(key)
	if err != nil {
		return errors.Wrap(err, "localfs: error executing delete statement")
	}
	return nil
}

func (fs *localfs) addToACLDB(ctx context.Context, resource, grantee, role string) error {
	stmt, err := fs.db.Prepare(fs.q(fs.sql(
		"INSERT INTO user_interaction (resource, grantee, role) VALUES (?, ?, ?) ON CONFLICT(resource, grantee) DO UPDATE SET role=?",
		"INSERT INTO user_interaction (resource, grantee, role) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE role=?")))
	if err != nil {
		return errors.Wrap(err, "localfs: error preparing statement")
	}
	_, err = stmt.Exec(resource, grantee, role, role)
	if err != nil {
		return errors.Wrap(err, "localfs: error executing insert statement")
	}
	return nil
}

func (fs *localfs) getACLs(ctx context.Context, resource string) (*sql.Rows, error) {
	grants, err := fs.db.Query(fs.q("SELECT grantee, role FROM user_interaction WHERE resource=?"), resource)
	if err != nil {
		return nil, err
	}
	return grants, nil
}

func (fs *localfs) removeFromACLDB(ctx context.Context, resource, grantee string) error {
	stmt, err := fs.db.Prepare(fs.q("UPDATE user_interaction SET role='' WHERE resource=? AND grantee=?"))
	if err != nil {
		return errors.Wrap(err, "localfs: error preparing statement")
	}
	_, err = stmt.Exec(resource, grantee)
	if err != nil {
		return errors.Wrap(err, "localfs: error executing delete statement")
	}
	return nil
}

func (fs *localfs) addToFavoritesDB(ctx context.Context, resource, grantee string) error {
	stmt, err := fs.db.Prepare(fs.q(fs.sql(
		"INSERT INTO user_interaction (resource, grantee, favorite) VALUES (?, ?, 1) ON CONFLICT(resource, grantee) DO UPDATE SET favorite=1",
		"INSERT INTO user_interaction (resource, grantee, favorite) VALUES (?, ?, 1) ON DUPLICATE KEY UPDATE favorite=1")))
	if err != nil {
		return errors.Wrap(err, "localfs: error preparing statement")
	}
	_, err = stmt.Exec(resource, grantee)
	if err != nil {
		return errors.Wrap(err, "localfs: error executing insert statement")
	}
	return nil
}

func (fs *localfs) removeFromFavoritesDB(ctx context.Context, resource, grantee string) error {
	stmt, err := fs.db.Prepare(fs.q("UPDATE user_interaction SET favorite=0 WHERE resource=? AND grantee=?"))
	if err != nil {
		return errors.Wrap(err, "localfs: error preparing statement")
	}
	_, err = stmt.Exec(resource, grantee)
	if err != nil {
		return errors.Wrap(err, "localfs: error executing delete statement")
	}
	return nil
}

func (fs *localfs) addToMetadataDB(ctx context.Context, resource, key, value string) error {
	stmt, err := fs.db.Prepare(fs.q(fs.sql(
		"INSERT INTO metadata (resource, `key`, `value`) VALUES (?, ?, ?) ON CONFLICT(resource, `key`) DO UPDATE SET `value`=?",
		"INSERT INTO metadata (resource, `key`, `value`) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE `value`=?")))
	if err != nil {
		return errors.Wrap(err, "localfs: error preparing statement")
	}
	_, err = stmt.Exec(resource, key, value, value)
	if err != nil {
		return errors.Wrap(err, "localfs: error executing insert statement")
	}
	return nil
}

func (fs *localfs) removeFromMetadataDB(ctx context.Context, resource, key string) error {
	stmt, err := fs.db.Prepare(fs.q("DELETE FROM metadata WHERE resource=? AND `key`=?"))
	if err != nil {
		return errors.Wrap(err, "localfs: error preparing statement")
	}
	_, err = stmt.Exec(resource, key)
	if err != nil {
		return errors.Wrap(err, "localfs: error executing delete statement")
	}
	return nil
}

func (fs *localfs) getMetadata(ctx context.Context, resource string) (*sql.Rows, error) {
	grants, err := fs.db.Query(fs.q("SELECT `key`, `value` FROM metadata WHERE resource=?"), resource)
	if err != nil {
		return nil, err
	}
	return grants, nil
}

func (fs *localfs) addToReferencesDB(ctx context.Context, resource, target string) error {
	stmt, err := fs.db.Prepare(fs.q(fs.sql(
		"INSERT INTO share_references (resource, target) VALUES (?, ?) ON CONFLICT(resource) DO UPDATE SET target=?",
		"INSERT INTO share_references (resource, target) VALUES (?, ?) ON DUPLICATE KEY UPDATE target=?")))
	if err != nil {
		return errors.Wrap(err, "localfs: error preparing statement")
	}
	_, err = stmt.Exec(resource, target, target)
	if err != nil {
		return errors.Wrap(err, "localfs: error executing insert statement")
	}
	return nil
}

func (fs *localfs) getReferenceEntry(ctx context.Context, resource string) (string, error) {
	var target string
	err := fs.db.QueryRow(fs.q("SELECT target FROM share_references WHERE resource=?"), resource).Scan(&target)
	if err != nil {
		return "", err
	}
	return target, nil
}

func (fs *localfs) copyMD(s string, t string) (err error) {
	stmt, err := fs.db.Prepare(fs.q("UPDATE user_interaction SET resource=? WHERE resource=?"))
	if err != nil {
		return errors.Wrap(err, "localfs: error preparing statement")
	}
	_, err = stmt.Exec(t, s)
	if err != nil {
		return errors.Wrap(err, "localfs: error executing delete statement")
	}

	stmt, err = fs.db.Prepare(fs.q("UPDATE metadata SET resource=? WHERE resource=?"))
	if err != nil {
		return errors.Wrap(err, "localfs: error preparing statement")
	}
	_, err = stmt.Exec(t, s)
	if err != nil {
		return errors.Wrap(err, "localfs: error executing delete statement")
	}

	stmt, err = fs.db.Prepare(fs.q("UPDATE share_references SET resource=? WHERE resource=?"))
	if err != nil {
		return errors.Wrap(err, "localfs: error preparing statement")
	}
	_, err = stmt.Exec(t, s)
	if err != nil {
		return errors.Wrap(err, "localfs: error executing delete statement")
	}
	return nil
}
