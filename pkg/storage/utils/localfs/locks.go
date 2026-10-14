// Copyright 2018-2024 CERN
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// In applying this license, CERN does not waive the privileges and immunities
// granted to it by virtue of its status as an Intergovernmental Organization
// or submit itself to any jurisdiction.

package localfs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path"
	"time"

	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	typespb "github.com/cs3org/go-cs3apis/cs3/types/v1beta1"
	"github.com/cs3org/reva/v3/pkg/errtypes"
	"github.com/mattn/go-sqlite3"
)

const lockSetOpaqueKey = "reva-localfs-lock-set"

func (fs *localfs) lockResourcePath(ctx context.Context, ref *provider.Reference) (string, error) {
	resourcePath, err := fs.resolve(ctx, ref)
	if err != nil {
		return "", err
	}
	if fs.isShareFolderChild(ctx, resourcePath) {
		resourcePath, err = fs.resolveShareReference(ctx, resourcePath, true)
		if err != nil {
			return "", err
		}
	}
	resourcePath = fs.wrap(ctx, resourcePath)
	info, err := os.Stat(resourcePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", errtypes.NotFound("localfs: lock resource does not exist")
		}
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errtypes.NotSupported("localfs: locks are only supported on existing files")
	}
	return path.Clean(resourcePath), nil
}

func (fs *localfs) getLocksAtPath(ctx context.Context, resourcePath string) ([]*provider.Lock, error) {
	now := time.Now().UnixNano()
	if _, err := fs.db.ExecContext(ctx,
		"DELETE FROM resource_locks WHERE resource=? AND expires_at<=?", resourcePath, now,
	); err != nil {
		return nil, err
	}
	rows, err := fs.db.QueryContext(ctx,
		"SELECT lock_json FROM resource_locks WHERE resource=? ORDER BY lock_id", resourcePath,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var locks []*provider.Lock
	for rows.Next() {
		var lockJSON string
		if err := rows.Scan(&lockJSON); err != nil {
			return nil, err
		}
		var lock provider.Lock
		if err := json.Unmarshal([]byte(lockJSON), &lock); err != nil {
			return nil, err
		}
		locks = append(locks, &lock)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return locks, nil
}

func (fs *localfs) GetLock(ctx context.Context, ref *provider.Reference) (*provider.Lock, error) {
	resourcePath, err := fs.lockResourcePath(ctx, ref)
	if err != nil {
		return nil, err
	}
	locks, err := fs.getLocksAtPath(ctx, resourcePath)
	if err != nil {
		return nil, err
	}
	if len(locks) == 0 {
		return nil, errtypes.NotFound("localfs: lock not found")
	}
	if len(locks) == 1 {
		return locks[0], nil
	}

	lockSetJSON, err := json.Marshal(locks)
	if err != nil {
		return nil, err
	}
	first := *locks[0]
	first.Opaque = &typespb.Opaque{
		Map: map[string]*typespb.OpaqueEntry{
			lockSetOpaqueKey: {Decoder: "plain", Value: lockSetJSON},
		},
	}
	return &first, nil
}

func (fs *localfs) SetLock(ctx context.Context, ref *provider.Reference, lock *provider.Lock) error {
	if lock == nil || lock.GetLockId() == "" {
		return errtypes.BadRequest("localfs: lock id is required")
	}
	if lock.GetType() != provider.LockType_LOCK_TYPE_EXCL &&
		lock.GetType() != provider.LockType_LOCK_TYPE_WRITE &&
		lock.GetType() != provider.LockType_LOCK_TYPE_SHARED {
		return errtypes.NotSupported("localfs: lock type is not supported")
	}
	resourcePath, err := fs.lockResourcePath(ctx, ref)
	if err != nil {
		return err
	}
	storedLock, expiresAt := normalizedLock(lock)
	lockJSON, err := json.Marshal(storedLock)
	if err != nil {
		return err
	}

	tx, err := fs.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixNano()
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM resource_locks WHERE resource=? AND expires_at<=?", resourcePath, now,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO resource_locks (resource, lock_id, lock_json, expires_at) VALUES (?, ?, ?, ?)",
		resourcePath, lock.GetLockId(), string(lockJSON), expiresAt,
	); err != nil {
		var sqliteErr sqlite3.Error
		if errors.As(err, &sqliteErr) && sqliteErr.ExtendedCode == sqlite3.ErrConstraintPrimaryKey {
			return errtypes.Conflict("localfs: lock id is already in use")
		}
		return err
	}

	rows, err := tx.QueryContext(ctx,
		"SELECT lock_id, lock_json FROM resource_locks WHERE resource=? AND lock_id<>?",
		resourcePath, lock.GetLockId(),
	)
	if err != nil {
		return err
	}
	conflict := false
	for rows.Next() {
		var otherID, otherJSON string
		if err := rows.Scan(&otherID, &otherJSON); err != nil {
			rows.Close()
			return err
		}
		var other provider.Lock
		if err := json.Unmarshal([]byte(otherJSON), &other); err != nil {
			rows.Close()
			return err
		}
		if lock.GetType() != provider.LockType_LOCK_TYPE_SHARED ||
			other.GetType() != provider.LockType_LOCK_TYPE_SHARED {
			conflict = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if conflict {
		return errtypes.Conflict("localfs: incompatible lock already exists")
	}
	return tx.Commit()
}

func (fs *localfs) RefreshLock(ctx context.Context, ref *provider.Reference, lock *provider.Lock, existingLockID string) error {
	if lock == nil || lock.GetLockId() == "" {
		return errtypes.BadRequest("localfs: lock id is required")
	}
	resourcePath, err := fs.lockResourcePath(ctx, ref)
	if err != nil {
		return err
	}
	current, err := fs.getLockByID(ctx, resourcePath, lock.GetLockId())
	if err != nil {
		return err
	}
	if existingLockID != "" && current.GetLockId() != existingLockID {
		return errtypes.BadRequest("localfs: lock id does not match")
	}
	if current.GetLockId() != lock.GetLockId() || !sameLockHolder(current, lock) {
		return errtypes.BadRequest("localfs: caller does not hold the lock")
	}
	return fs.replaceLock(ctx, resourcePath, current, lock)
}

func (fs *localfs) getLockByID(ctx context.Context, resourcePath, lockID string) (*provider.Lock, error) {
	var lockJSON string
	err := fs.db.QueryRowContext(ctx,
		"SELECT lock_json FROM resource_locks WHERE resource=? AND lock_id=? AND expires_at>?",
		resourcePath, lockID, time.Now().UnixNano(),
	).Scan(&lockJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, errtypes.NotFound("localfs: lock not found")
		}
		return nil, err
	}
	var lock provider.Lock
	if err := json.Unmarshal([]byte(lockJSON), &lock); err != nil {
		return nil, err
	}
	return &lock, nil
}

func (fs *localfs) replaceLock(ctx context.Context, resourcePath string, current, updated *provider.Lock) error {
	storedLock, expiresAt := normalizedLock(updated)
	lockJSON, err := json.Marshal(storedLock)
	if err != nil {
		return err
	}
	result, err := fs.db.ExecContext(ctx,
		"UPDATE resource_locks SET lock_json=?, expires_at=? WHERE resource=? AND lock_id=? AND expires_at>?",
		string(lockJSON), expiresAt, resourcePath, current.GetLockId(), time.Now().UnixNano(),
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return errtypes.BadRequest("localfs: lock changed while refreshing")
	}
	return nil
}

func (fs *localfs) Unlock(ctx context.Context, ref *provider.Reference, lock *provider.Lock) error {
	if lock == nil || lock.GetLockId() == "" {
		return errtypes.BadRequest("localfs: lock id is required")
	}
	resourcePath, err := fs.lockResourcePath(ctx, ref)
	if err != nil {
		return err
	}
	current, err := fs.getLockByID(ctx, resourcePath, lock.GetLockId())
	if err != nil {
		return err
	}
	if !sameLockHolder(current, lock) {
		return errtypes.BadRequest("localfs: lock holder does not match")
	}
	result, err := fs.db.ExecContext(ctx,
		"DELETE FROM resource_locks WHERE resource=? AND lock_id=? AND expires_at>?",
		resourcePath, lock.GetLockId(), time.Now().UnixNano(),
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return errtypes.BadRequest("localfs: lock changed while unlocking")
	}
	return nil
}

func normalizedLock(lock *provider.Lock) (*provider.Lock, int64) {
	stored := *lock
	expiresAt := int64(lock.GetExpiration().GetSeconds())*int64(time.Second) +
		int64(lock.GetExpiration().GetNanos())
	if lock.GetExpiration() == nil {
		expiration := time.Now().Add(time.Hour)
		expiresAt = expiration.UnixNano()
		stored.Expiration = &typespb.Timestamp{
			Seconds: uint64(expiration.Unix()),
			Nanos:   uint32(expiration.Nanosecond()),
		}
	}
	return &stored, expiresAt
}

func sameLockHolder(a, b *provider.Lock) bool {
	if a.GetAppName() != b.GetAppName() {
		return false
	}
	au, bu := a.GetUser(), b.GetUser()
	if au == nil || bu == nil {
		return au == nil && bu == nil
	}
	return au.GetOpaqueId() == bu.GetOpaqueId() &&
		au.GetIdp() == bu.GetIdp() &&
		au.GetType() == bu.GetType()
}
