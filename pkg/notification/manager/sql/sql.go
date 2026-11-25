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

package sql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/glebarez/go-sqlite"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/cs3org/reva/v3/cmd/revad/pkg/config"
	"github.com/cs3org/reva/v3/pkg/notification"
	"github.com/cs3org/reva/v3/pkg/notification/manager/registry"
	"github.com/cs3org/reva/v3/pkg/sharedconf"
	"github.com/cs3org/reva/v3/pkg/utils/cfg"
)

func init() {
	registry.Register("sql", NewMysql)
}

type Config struct {
	config.Database `mapstructure:",squash"`
}

func (c *Config) ApplyDefaults() {
	c.Database = sharedconf.GetDBInfo(c.Database)
}

// dialect selects the SQL syntax. The three engines differ in the upsert clause
// and in placeholder style, so statements are built per connection.
type dialect int

const (
	dialectSQLite dialect = iota
	dialectMySQL
	dialectPostgres
)

const (
	// PostgreSQL forks a backend process per connection; on a small box an
	// unbounded pool is what takes the database down.
	pgMaxOpenConns = 8
	pgMaxIdleConns = 2
)

type mgr struct {
	driver  string
	dialect dialect
	db      *sql.DB
}

// NewMysql returns an instance of the sql notifications manager. The name is
// historical: the engine comes from db_engine ([shared] when unset), so this
// driver serves sqlite, mysql/mariadb and postgres alike.
func NewMysql(ctx context.Context, m map[string]any) (notification.Manager, error) {
	var c Config
	if err := cfg.Decode(m, &c); err != nil {
		return nil, err
	}
	c.ApplyDefaults()

	switch c.Engine {
	case "postgres":
		dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
			c.DBHost, c.DBPort, c.DBUsername, c.DBPassword, c.DBName)
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return nil, err
		}
		// Unlike mysql and sqlite, a postgres deployment cannot be assumed to
		// have the tables: db_changes.sql is a manual step for mysql only.
		if err := EnsurePostgresSchema(db); err != nil {
			_ = db.Close()
			return nil, err
		}
		return New("pgx", db)
	case "sqlite", "sqlite3":
		db, err := sql.Open("sqlite", c.DBName)
		if err != nil {
			return nil, err
		}
		return New("sqlite3", db)
	default:
		db, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s:%d)/%s",
			c.DBUsername, c.DBPassword, c.DBHost, c.DBPort, c.DBName))
		if err != nil {
			return nil, err
		}
		return New("mysql", db)
	}
}

// New returns a new Notifications driver connecting to the given sql.DB.
func New(driver string, db *sql.DB) (notification.Manager, error) {
	d := dialectFor(driver)
	if d == dialectPostgres {
		db.SetMaxOpenConns(pgMaxOpenConns)
		db.SetMaxIdleConns(pgMaxIdleConns)
	}
	return &mgr{
		driver:  driver,
		dialect: d,
		db:      db,
	}, nil
}

func dialectFor(driver string) dialect {
	switch strings.ToLower(driver) {
	case "pgx", "postgres", "postgresql":
		return dialectPostgres
	case "sqlite", "sqlite3", "glebarez":
		return dialectSQLite
	default:
		return dialectMySQL
	}
}

// rebind rewrites ? placeholders to $1..$N, which is what PostgreSQL requires.
func (m *mgr) rebind(q string) string {
	if m.dialect != dialectPostgres {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] != '?' {
			b.WriteByte(q[i])
			continue
		}
		n++
		fmt.Fprintf(&b, "$%d", n)
	}
	return b.String()
}

// UpsertNotification creates or updates a notification.
func (m *mgr) UpsertNotification(n notification.Notification) error {
	if err := n.CheckNotification(); err != nil {
		return err
	}

	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var notificationID int64
	if m.dialect == dialectMySQL {
		// REPLACE deletes the conflicting row and inserts a new one, so the id
		// changes and ON DELETE CASCADE already dropped the old recipients.
		res, err := tx.Exec("REPLACE INTO notifications (ref, template_name) VALUES (?, ?)", n.Ref, n.TemplateName)
		if err != nil {
			return err
		}
		notificationID, err = res.LastInsertId()
		if err != nil {
			return err
		}
	} else {
		// SQLite and PostgreSQL reject REPLACE INTO. RETURNING gives the id of
		// either the inserted or the updated row, which LastInsertId cannot do.
		q := m.rebind(`INSERT INTO notifications (ref, template_name) VALUES (?, ?)
			ON CONFLICT (ref) DO UPDATE SET template_name = excluded.template_name
			RETURNING id`)
		if err := tx.QueryRow(q, n.Ref, n.TemplateName).Scan(&notificationID); err != nil {
			return err
		}
	}

	// Upserting replaces the recipient set rather than appending to it, which is
	// what the mysql path got for free from the REPLACE cascade.
	if _, err := tx.Exec(m.rebind("DELETE FROM notification_recipients WHERE notification_id = ?"), notificationID); err != nil {
		return err
	}
	insert := m.rebind("INSERT INTO notification_recipients (notification_id, recipient) VALUES (?, ?)")
	for _, recipient := range n.Recipients {
		if _, err := tx.Exec(insert, notificationID, recipient); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// GetNotification reads a notification.
func (m *mgr) GetNotification(ref string) (*notification.Notification, error) {
	query := m.rebind(`
		SELECT n.id, n.ref, n.template_name, nr.recipient
		FROM notifications AS n
		JOIN notification_recipients AS nr ON n.id = nr.notification_id
		WHERE n.ref = ?
	`)

	rows, err := m.db.Query(query, ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var n notification.Notification
	n.Ref = ref
	n.Recipients = make([]string, 0)
	count := 0

	for rows.Next() {
		var id int64
		var recipient string
		if err := rows.Scan(&id, &n.Ref, &n.TemplateName, &recipient); err != nil {
			return nil, err
		}
		n.Recipients = append(n.Recipients, recipient)
		count++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, &notification.NotFoundError{Ref: ref}
	}

	return &n, nil
}

// DeleteNotification deletes a notification.
func (m *mgr) DeleteNotification(ref string) error {
	// A single statement is its own transaction; the previous version opened a
	// transaction and returned without committing, so the delete was lost.
	result, err := m.db.Exec(m.rebind("DELETE FROM notifications WHERE ref = ?"), ref)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return &notification.NotFoundError{Ref: ref}
	}

	return nil
}

// ensurePostgresSchema creates the tables the manager needs. The DDL mirrors
// pkg/notification/db_changes.sql with postgres types: SERIAL instead of
// AUTO_INCREMENT and no backticks.
func EnsurePostgresSchema(db *sql.DB) error {
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS notifications (
			id SERIAL PRIMARY KEY,
			ref VARCHAR(3072) UNIQUE NOT NULL,
			template_name VARCHAR(320) NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS notification_recipients (
			id SERIAL PRIMARY KEY,
			notification_id INTEGER NOT NULL REFERENCES notifications (id) ON DELETE CASCADE,
			recipient VARCHAR(320) NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS notifications_ix0 ON notifications (ref)`,
		`CREATE INDEX IF NOT EXISTS notification_recipients_ix0 ON notification_recipients (notification_id)`,
		`CREATE INDEX IF NOT EXISTS notification_recipients_ix1 ON notification_recipients (recipient)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("notifications: creating postgres schema: %w", err)
		}
	}
	return nil
}
