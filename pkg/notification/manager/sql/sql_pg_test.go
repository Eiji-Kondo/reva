// Copyright 2018-2025 CERN
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

package sql_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/cs3org/reva/v3/pkg/notification"
	sqlmanager "github.com/cs3org/reva/v3/pkg/notification/manager/sql"
	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

// These specs run against a real PostgreSQL server, because the point of the
// postgres branch is the SQL dialect: ? placeholders, ON CONFLICT and
// RETURNING cannot be exercised on sqlite. Point REVA_TEST_PG_DSN at a scratch
// database; the spec drops and recreates its own tables.
var _ = Describe("SQL manager for notifications on postgres", func() {
	var (
		db  *sql.DB
		mgr notification.Manager
		dsn string
	)

	BeforeEach(func() {
		dsn = os.Getenv("REVA_TEST_PG_DSN")
		if dsn == "" {
			Skip("REVA_TEST_PG_DSN not set; skipping postgres specs")
		}

		var err error
		db, err = sql.Open("pgx", dsn)
		Expect(err).ToNot(HaveOccurred())
		Expect(db.Ping()).To(Succeed())

		_, err = db.Exec("DROP TABLE IF EXISTS notification_recipients")
		Expect(err).ToNot(HaveOccurred())
		_, err = db.Exec("DROP TABLE IF EXISTS notifications")
		Expect(err).ToNot(HaveOccurred())

		Expect(sqlmanager.EnsurePostgresSchema(db)).To(Succeed())

		mgr, err = sqlmanager.New("pgx", db)
		Expect(err).ToNot(HaveOccurred())
	})

	AfterEach(func() {
		if db != nil {
			_ = db.Close()
		}
	})

	count := func(query string, args ...any) int {
		var n int
		Expect(db.QueryRow(query, args...).Scan(&n)).To(Succeed())
		return n
	}

	It("creates a notification and its recipients", func() {
		n := notification.Notification{
			Ref:          "pg-create",
			TemplateName: "tpl",
			Recipients:   []string{"jdoe", "testuser"},
		}
		Expect(mgr.UpsertNotification(n)).To(Succeed())

		got, err := mgr.GetNotification("pg-create")
		Expect(err).ToNot(HaveOccurred())
		Expect(got.TemplateName).To(Equal("tpl"))
		Expect(got.Recipients).To(ConsistOf("jdoe", "testuser"))
		Expect(count("SELECT COUNT(*) FROM notifications")).To(Equal(1))
	})

	It("updates in place without duplicating the row", func() {
		first := notification.Notification{
			Ref:          "pg-update",
			TemplateName: "before",
			Recipients:   []string{"a", "b"},
		}
		Expect(mgr.UpsertNotification(first)).To(Succeed())

		second := notification.Notification{
			Ref:          "pg-update",
			TemplateName: "after",
			Recipients:   []string{"c"},
		}
		Expect(mgr.UpsertNotification(second)).To(Succeed())

		Expect(count("SELECT COUNT(*) FROM notifications")).To(Equal(1))
		got, err := mgr.GetNotification("pg-update")
		Expect(err).ToNot(HaveOccurred())
		Expect(got.TemplateName).To(Equal("after"))
		// The old recipients must be gone, not appended to.
		Expect(got.Recipients).To(Equal([]string{"c"}))
	})

	It("keeps two notifications separate", func() {
		for _, ref := range []string{"pg-a", "pg-b"} {
			n := notification.Notification{
				Ref:          ref,
				TemplateName: "tpl-" + ref,
				Recipients:   []string{"user-" + ref},
			}
			Expect(mgr.UpsertNotification(n)).To(Succeed())
		}
		Expect(count("SELECT COUNT(*) FROM notifications")).To(Equal(2))
		Expect(count("SELECT COUNT(*) FROM notification_recipients")).To(Equal(2))

		got, err := mgr.GetNotification("pg-b")
		Expect(err).ToNot(HaveOccurred())
		Expect(got.Recipients).To(Equal([]string{"user-pg-b"}))
	})

	It("reports NotFoundError for an unknown ref", func() {
		_, err := mgr.GetNotification("pg-missing")
		Expect(err).To(HaveOccurred())
		_, isNotFound := err.(*notification.NotFoundError)
		Expect(isNotFound).To(BeTrue())
	})

	It("deletes the notification and cascades to recipients", func() {
		n := notification.Notification{
			Ref:          "pg-delete",
			TemplateName: "tpl",
			Recipients:   []string{"x", "y"},
		}
		Expect(mgr.UpsertNotification(n)).To(Succeed())
		Expect(count("SELECT COUNT(*) FROM notification_recipients")).To(Equal(2))

		Expect(mgr.DeleteNotification("pg-delete")).To(Succeed())

		// The old implementation opened a transaction and never committed, so
		// this is the assertion that catches a lost delete.
		Expect(count("SELECT COUNT(*) FROM notifications")).To(Equal(0))
		Expect(count("SELECT COUNT(*) FROM notification_recipients")).To(Equal(0))

		_, err := mgr.GetNotification("pg-delete")
		_, isNotFound := err.(*notification.NotFoundError)
		Expect(isNotFound).To(BeTrue())
	})

	It("returns NotFoundError when deleting twice", func() {
		n := notification.Notification{Ref: "pg-twice", TemplateName: "tpl", Recipients: []string{"z"}}
		Expect(mgr.UpsertNotification(n)).To(Succeed())
		Expect(mgr.DeleteNotification("pg-twice")).To(Succeed())

		err := mgr.DeleteNotification("pg-twice")
		Expect(err).To(HaveOccurred())
		_, isNotFound := err.(*notification.NotFoundError)
		Expect(isNotFound).To(BeTrue())
	})

	It("rejects an invalid notification", func() {
		err := mgr.UpsertNotification(notification.Notification{})
		Expect(err).To(HaveOccurred())
		_, isInvalid := err.(*notification.InvalidNotificationError)
		Expect(isInvalid).To(BeTrue())
	})

	It("is safe when the same ref is upserted concurrently", func() {
		const workers = 4
		errs := make(chan error, workers)
		for i := 0; i < workers; i++ {
			go func(i int) {
				errs <- mgr.UpsertNotification(notification.Notification{
					Ref:          "pg-race",
					TemplateName: fmt.Sprintf("tpl-%d", i),
					Recipients:   []string{fmt.Sprintf("user-%d", i)},
				})
			}(i)
		}
		for i := 0; i < workers; i++ {
			Expect(<-errs).ToNot(HaveOccurred())
		}
		Expect(count("SELECT COUNT(*) FROM notifications")).To(Equal(1))
	})
})

// NewMysql is what revad calls through the manager registry, so this spec also
// covers the config decode and the engine switch, not just the SQL statements.
var _ = Describe("NewMysql with db_engine=postgres", func() {
	It("builds a manager from the config map revad passes in", func() {
		dsn := os.Getenv("REVA_TEST_PG_DSN")
		if dsn == "" {
			Skip("REVA_TEST_PG_DSN not set; skipping postgres specs")
		}

		opts := map[string]any{"db_engine": "postgres"}
		for _, f := range strings.Fields(dsn) {
			k, v, ok := strings.Cut(f, "=")
			if !ok {
				continue
			}
			switch k {
			case "host":
				opts["db_host"] = v
			case "user":
				opts["db_username"] = v
			case "password":
				opts["db_password"] = v
			case "dbname":
				opts["db_name"] = v
			case "port":
				p, err := strconv.Atoi(v)
				Expect(err).ToNot(HaveOccurred())
				opts["db_port"] = p
			}
		}

		mgr, err := sqlmanager.NewMysql(context.Background(), opts)
		Expect(err).ToNot(HaveOccurred())

		n := notification.Notification{Ref: "pg-via-config", TemplateName: "tpl", Recipients: []string{"u1"}}
		Expect(mgr.UpsertNotification(n)).To(Succeed())

		got, err := mgr.GetNotification("pg-via-config")
		Expect(err).ToNot(HaveOccurred())
		Expect(got.Recipients).To(Equal([]string{"u1"}))
		Expect(mgr.DeleteNotification("pg-via-config")).To(Succeed())
	})
})
