package localdb

import (
	"database/sql"
	"fmt"
	"path"

	// Provides sqlite, MySQL and PostgreSQL drivers, matching localfs.
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/mattn/go-sqlite3"
	"github.com/pkg/errors"

	"github.com/cs3org/reva/v3/cmd/revad/pkg/config"
	"github.com/cs3org/reva/v3/pkg/sharedconf"
	"github.com/cs3org/reva/v3/pkg/storage/utils/localfs"
)

// dialect selects the SQL flavour. PostgreSQL rejects ? placeholders, so the
// statement rewriter needs to know which one is in use.
type dialect int

const (
	dialectSQLite dialect = iota
	dialectMySQL
	dialectPostgres
)

const (
	maxOpenConns = 25
	maxIdleConns = 5
)

// schema creates the inode table. It is deliberately separate from the five
// localfs tables: those hold side-cars (trash, locks, favourites, share
// pointers, arbitrary metadata) and stay owned by localfs during Phase 1.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS inode (
		file_id TEXT PRIMARY KEY,
		path    TEXT NOT NULL UNIQUE,
		parent  TEXT NOT NULL,
		name    TEXT NOT NULL,
		size    BIGINT NOT NULL DEFAULT 0,
		mtime   BIGINT NOT NULL DEFAULT 0,
		etag    TEXT NOT NULL DEFAULT '',
		is_dir  BOOLEAN NOT NULL DEFAULT FALSE
	)`,
	`CREATE INDEX IF NOT EXISTS inode_parent ON inode (parent)`,
}

// openDB opens the metadata DB and applies the inode schema. Engine selection
// mirrors localfs so both drivers can point at the same database.
func openDB(c *localfs.Config) (dialect, *sql.DB, error) {
	engine := c.DBEngine
	if engine == "" {
		engine = sharedconf.GetDBInfo(config.Database{}).Engine
	}

	var (
		d   dialect
		db  *sql.DB
		err error
	)
	switch engine {
	case "", "sqlite":
		d = dialectSQLite
		db, err = sql.Open("sqlite3", path.Join(c.Root, "localdb.db"))
	case "postgres":
		d = dialectPostgres
		info := sharedconf.GetDBInfo(config.Database{
			Engine:     engine,
			DBUsername: c.DBUsername,
			DBPassword: c.DBPassword,
			DBHost:     c.DBHost,
			DBPort:     c.DBPort,
			DBName:     c.DBName,
		})
		db, err = sql.Open("pgx", fmt.Sprintf(
			"host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
			info.DBHost, info.DBPort, info.DBUsername, info.DBPassword, info.DBName))
	default:
		d = dialectMySQL
		info := sharedconf.GetDBInfo(config.Database{
			Engine:     engine,
			DBUsername: c.DBUsername,
			DBPassword: c.DBPassword,
			DBHost:     c.DBHost,
			DBPort:     c.DBPort,
			DBName:     c.DBName,
		})
		db, err = sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s:%d)/%s",
			info.DBUsername, info.DBPassword, info.DBHost, info.DBPort, info.DBName))
	}
	if err != nil {
		return d, nil, errors.Wrap(err, "localdb: open metadata DB")
	}

	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return d, nil, errors.Wrap(err, "localdb: ping metadata DB")
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return d, nil, errors.Wrap(err, "localdb: apply inode schema")
		}
	}
	return d, db, nil
}
