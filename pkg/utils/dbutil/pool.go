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

package dbutil

import (
	"time"

	"gorm.io/gorm"
)

// Pool limits are deliberately small. database/sql defaults to an unbounded
// number of open connections, which is harmless for MySQL and SQLite but not for
// PostgreSQL: it forks one backend process per connection, each with its own
// ~2-5 MiB of private memory. On a 236 MiB box an unbounded pool exhausts RAM and
// the OOM killer takes the database down.
const (
	MaxOpenConns    = 8
	MaxIdleConns    = 2
	ConnMaxLifetime = 30 * time.Minute
	ConnMaxIdleTime = 5 * time.Minute
)

// ConfigurePool applies bounded connection pool settings to a gorm handle.
func ConfigurePool(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	sqlDB.SetMaxOpenConns(MaxOpenConns)
	sqlDB.SetMaxIdleConns(MaxIdleConns)
	sqlDB.SetConnMaxLifetime(ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(ConnMaxIdleTime)
	return nil
}
