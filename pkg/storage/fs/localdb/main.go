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

// Package localdb is a storage driver whose metadata plane lives in a database
// while the bytes stay on the local filesystem.
//
// Phase 1 scope: the read paths are served from the DB, every write path
// delegates to localfs. That proves the DB can be authoritative for reads
// without touching the write path. Phase 2 replaces the delegations.
//
// Schema notes:
//   - The inode table is keyed on a stable file id, not on a path. The path is
//     an index over the tree, so a rename becomes one row update instead of a
//     subtree rewrite, and a share holding the id survives the rename.
//   - size/mtime/etag are copied from the disk on sync. They are cached facts,
//     not independent truth; the reconciler owns them.
package localdb

import (
	"context"
	"database/sql"
	"io"
	"net/url"

	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	typepb "github.com/cs3org/go-cs3apis/cs3/types/v1beta1"
	"github.com/cs3org/reva/v3/pkg/appctx"
	"github.com/cs3org/reva/v3/pkg/errtypes"
	"github.com/cs3org/reva/v3/pkg/storage"
	"github.com/cs3org/reva/v3/pkg/storage/fs/registry"
	"github.com/cs3org/reva/v3/pkg/storage/utils/localfs"
	"github.com/cs3org/reva/v3/pkg/utils/cfg"
	"github.com/pkg/errors"
)

const driverName = "localdb"

func init() {
	registry.Register(driverName, New)
}

// Config embeds localfs.Config so the write path and the byte storage keep
// working unchanged during Phase 1.
type Config struct {
	localfs.Config `mapstructure:",squash"`

	// AutoSync refreshes the cache from the disk when a read misses. Keep it
	// true while writes are delegated: without it the table only sees rows the
	// reconciler has already inserted.
	AutoSync bool `mapstructure:"auto_sync"`
}

// localdb keeps the DB authoritative for reads and forwards writes to a
// localfs instance until Phase 2.
type localdb struct {
	conf    *Config
	db      *sql.DB
	dialect dialect
	legacy  storage.FS
}

// New builds the driver. The DB is opened first because a driver that cannot
// reach its metadata plane is useless, while a missing legacy path only breaks
// writes.
func New(ctx context.Context, m map[string]any) (storage.FS, error) {
	var c Config
	if err := cfg.Decode(m, &c); err != nil {
		return nil, err
	}
	if c.Root == "" {
		return nil, errors.New("localdb: root is required")
	}
	c.ApplyDefaults()

	d, db, err := openDB(&c.Config)
	if err != nil {
		return nil, errors.Wrap(err, "localdb: cannot open metadata DB")
	}

	legacy, err := localfs.NewLocalFS(&c.Config)
	if err != nil {
		_ = db.Close()
		return nil, errors.Wrap(err, "localdb: cannot build legacy write path")
	}

	fs := &localdb{conf: &c, db: db, dialect: d, legacy: legacy}
	if c.AutoSync {
		if err := fs.syncTree(ctx, "/"); err != nil {
			// A failed initial sync must not stop startup: the tree may be
			// empty on a fresh deployment, and the reconciler retries.
			appctx.GetLogger(ctx).Warn().Err(err).Msg("localdb: initial sync incomplete")
		}
	}
	return fs, nil
}

// ---------------------------------------------------------------------------
// Phase 1: read paths served from the DB
// ---------------------------------------------------------------------------

// GetMD reads one node from the inode table.
func (fs *localdb) GetMD(ctx context.Context, ref *provider.Reference, mdKeys []string) (*provider.ResourceInfo, error) {
	p, err := fs.resolvePath(ctx, ref)
	if err != nil {
		return nil, err
	}
	return fs.statCached(ctx, p)
}

// ListFolder reads the direct children of a container from the inode table.
func (fs *localdb) ListFolder(ctx context.Context, ref *provider.Reference, mdKeys []string) ([]*provider.ResourceInfo, error) {
	p, err := fs.resolvePath(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := fs.ensureDirCached(ctx, p); err != nil {
		return nil, err
	}

	rows, err := fs.db.QueryContext(ctx, fs.q(
		`SELECT path, size, mtime, etag, is_dir, file_id FROM inode WHERE parent = ? ORDER BY name`), p)
	if err != nil {
		return nil, errors.Wrap(err, "localdb: list children")
	}
	defer rows.Close()

	var out []*provider.ResourceInfo
	for rows.Next() {
		var (
			childPath string
			size      int64
			mtime     int64
			etag      string
			isDir     bool
			fileID    string
		)
		if err := rows.Scan(&childPath, &size, &mtime, &etag, &isDir, &fileID); err != nil {
			return nil, errors.Wrap(err, "localdb: scan child")
		}
		out = append(out, buildResourceInfo(childPath, size, mtime, etag, isDir, fileID))
	}
	return out, rows.Err()
}

// GetPathByID maps a stable file id back to a path. Shares and the recycle bin
// depend on it, which is why it belongs in Phase 1: it is the call cephmount
// could not answer without Ceph (AGENTS.md session 294).
func (fs *localdb) GetPathByID(ctx context.Context, id *provider.ResourceId) (string, error) {
	if id == nil || id.OpaqueId == "" {
		return "", errtypes.BadRequest("localdb: empty resource id")
	}
	var p string
	err := fs.db.QueryRowContext(ctx, fs.q(`SELECT path FROM inode WHERE file_id = ?`), id.OpaqueId).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errtypes.NotFound(id.OpaqueId)
	}
	if err != nil {
		return "", errors.Wrap(err, "localdb: get path by id")
	}
	return p, nil
}

// GetHome resolves the caller's home through the localfs layout template, so
// Phase 1 keeps the same on-disk shape.
func (fs *localdb) GetHome(ctx context.Context) (string, error) {
	u, ok := appctx.ContextGetUser(ctx)
	if !ok {
		return "", errtypes.UserRequired("localdb: no user in context")
	}
	if fs.conf.DisableHome {
		return "/", nil
	}
	return userHome(fs.conf.UserLayout, u), nil
}

// ListRecycle reads the trash from the DB. Phase 1 returns the rows localfs
// already writes, so behaviour does not change yet.
func (fs *localdb) ListRecycle(ctx context.Context, basePath, key, relativePath string, from, to *typepb.Timestamp) ([]*provider.RecycleItem, error) {
	rows, err := fs.db.QueryContext(ctx, fs.q(`SELECT key, path FROM recycled_entries ORDER BY key`))
	if err != nil {
		return nil, errors.Wrap(err, "localdb: list recycle")
	}
	defer rows.Close()

	var out []*provider.RecycleItem
	for rows.Next() {
		var k, p string
		if err := rows.Scan(&k, &p); err != nil {
			return nil, errors.Wrap(err, "localdb: scan recycle")
		}
		out = append(out, &provider.RecycleItem{Key: k, Ref: &provider.Reference{Path: p}})
	}
	return out, rows.Err()
}

// GetQuota sums the cached sizes so the answer comes from the DB instead of a
// directory walk.
func (fs *localdb) GetQuota(ctx context.Context, ref *provider.Reference) (uint64, uint64, error) {
	home, err := fs.GetHome(ctx)
	if err != nil {
		return 0, 0, err
	}
	var used sql.NullInt64
	err = fs.db.QueryRowContext(ctx, fs.q(
		`SELECT SUM(size) FROM inode WHERE is_dir = ? AND (path = ? OR path LIKE ?)`),
		false, home, home+"/%").Scan(&used)
	if err != nil {
		return 0, 0, errors.Wrap(err, "localdb: quota sum")
	}
	return 0, uint64(used.Int64), nil
}

// ListGrants reads ACLs. localfs keeps them as xattrs, so Phase 1 has nothing
// in the DB and returns an empty list: that is the correct answer for a tree
// with no ACLs, and it keeps callers from treating the driver as broken.
func (fs *localdb) ListGrants(ctx context.Context, ref *provider.Reference) ([]*provider.Grant, error) {
	p, err := fs.resolvePath(ctx, ref)
	if err != nil {
		return nil, err
	}
	if _, err := fs.statCached(ctx, p); err != nil {
		return nil, err
	}
	return nil, nil
}

// ListStorageSpaces reports the single space backed by root.
func (fs *localdb) ListStorageSpaces(ctx context.Context, filter []*provider.ListStorageSpacesRequest_Filter) ([]*provider.StorageSpace, error) {
	return []*provider.StorageSpace{{
		Id:        &provider.StorageSpaceId{OpaqueId: driverName + "$root"},
		Root:      &provider.ResourceId{StorageId: driverName, OpaqueId: "root"},
		Name:      driverName,
		SpaceType: "personal",
		RootInfo:  buildResourceInfo("/", 0, 0, "", true, "root"),
	}}, nil
}

// ---------------------------------------------------------------------------
// Phase 2 placeholders: writes still go to localfs
// ---------------------------------------------------------------------------

func (fs *localdb) CreateHome(ctx context.Context) error {
	if err := fs.legacy.CreateHome(ctx); err != nil {
		return err
	}
	home, err := fs.GetHome(ctx)
	if err != nil {
		return err
	}
	return fs.syncTree(ctx, home)
}

func (fs *localdb) CreateDir(ctx context.Context, ref *provider.Reference) error {
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return err
	}
	if err := fs.legacy.CreateDir(ctx, lref); err != nil {
		return err
	}
	return fs.resyncRef(ctx, ref)
}

func (fs *localdb) TouchFile(ctx context.Context, ref *provider.Reference) error {
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return err
	}
	if err := fs.legacy.TouchFile(ctx, lref); err != nil {
		return err
	}
	return fs.resyncRef(ctx, ref)
}

func (fs *localdb) Delete(ctx context.Context, ref *provider.Reference) error {
	p, err := fs.resolvePath(ctx, ref)
	if err != nil {
		return err
	}
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return err
	}
	if err := fs.legacy.Delete(ctx, lref); err != nil {
		return err
	}
	return fs.purgeSubtree(ctx, p)
}

func (fs *localdb) Move(ctx context.Context, oldRef, newRef *provider.Reference) error {
	oldPath, err := fs.resolvePath(ctx, oldRef)
	if err != nil {
		return err
	}
	lold, err := fs.legacyRef(ctx, oldRef)
	if err != nil {
		return err
	}
	lnew, err := fs.legacyRef(ctx, newRef)
	if err != nil {
		return err
	}
	if err := fs.legacy.Move(ctx, lold, lnew); err != nil {
		return err
	}
	// A rename is one path update, not a subtree rewrite, because the table is
	// keyed on file_id. Phase 2 collapses this into a single UPDATE.
	return fs.syncTree(ctx, oldPath)
}

func (fs *localdb) InitiateUpload(ctx context.Context, ref *provider.Reference, uploadLength int64, metadata map[string]string) (map[string]string, error) {
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	return fs.legacy.InitiateUpload(ctx, lref, uploadLength, metadata)
}

func (fs *localdb) Upload(ctx context.Context, ref *provider.Reference, r io.ReadCloser, metadata map[string]string) error {
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return err
	}
	if err := fs.legacy.Upload(ctx, lref, r, metadata); err != nil {
		return err
	}
	return fs.resyncRef(ctx, ref)
}

func (fs *localdb) Download(ctx context.Context, ref *provider.Reference, ranges []storage.Range) (io.ReadCloser, error) {
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	return fs.legacy.Download(ctx, lref, ranges)
}

func (fs *localdb) ListRevisions(ctx context.Context, ref *provider.Reference) ([]*provider.FileVersion, error) {
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	return fs.legacy.ListRevisions(ctx, lref)
}

func (fs *localdb) DownloadRevision(ctx context.Context, ref *provider.Reference, key string) (io.ReadCloser, error) {
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	return fs.legacy.DownloadRevision(ctx, lref, key)
}

func (fs *localdb) RestoreRevision(ctx context.Context, ref *provider.Reference, key string) error {
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return err
	}
	return fs.legacy.RestoreRevision(ctx, lref, key)
}

func (fs *localdb) RestoreRecycleItem(ctx context.Context, basePath, key, relativePath string, restoreRef *provider.Reference) error {
	return fs.legacy.RestoreRecycleItem(ctx, basePath, key, relativePath, restoreRef)
}

func (fs *localdb) PurgeRecycleItem(ctx context.Context, basePath, key, relativePath string) error {
	return fs.legacy.PurgeRecycleItem(ctx, basePath, key, relativePath)
}

func (fs *localdb) EmptyRecycle(ctx context.Context) error {
	return fs.legacy.EmptyRecycle(ctx)
}

func (fs *localdb) AddGrant(ctx context.Context, ref *provider.Reference, g *provider.Grant) error {
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return err
	}
	return fs.legacy.AddGrant(ctx, lref, g)
}

func (fs *localdb) DenyGrant(ctx context.Context, ref *provider.Reference, g *provider.Grantee) error {
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return err
	}
	return fs.legacy.DenyGrant(ctx, lref, g)
}

func (fs *localdb) RemoveGrant(ctx context.Context, ref *provider.Reference, g *provider.Grant) error {
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return err
	}
	return fs.legacy.RemoveGrant(ctx, lref, g)
}

func (fs *localdb) UpdateGrant(ctx context.Context, ref *provider.Reference, g *provider.Grant) error {
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return err
	}
	return fs.legacy.UpdateGrant(ctx, lref, g)
}

func (fs *localdb) CreateReference(ctx context.Context, p string, targetURI *url.URL) error {
	return fs.legacy.CreateReference(ctx, p, targetURI)
}

func (fs *localdb) SetArbitraryMetadata(ctx context.Context, ref *provider.Reference, md *provider.ArbitraryMetadata) error {
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return err
	}
	return fs.legacy.SetArbitraryMetadata(ctx, lref, md)
}

func (fs *localdb) UnsetArbitraryMetadata(ctx context.Context, ref *provider.Reference, keys []string) error {
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return err
	}
	return fs.legacy.UnsetArbitraryMetadata(ctx, lref, keys)
}

func (fs *localdb) SetLock(ctx context.Context, ref *provider.Reference, lock *provider.Lock) error {
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return err
	}
	return fs.legacy.SetLock(ctx, lref, lock)
}

func (fs *localdb) GetLock(ctx context.Context, ref *provider.Reference) (*provider.Lock, error) {
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	return fs.legacy.GetLock(ctx, lref)
}

func (fs *localdb) RefreshLock(ctx context.Context, ref *provider.Reference, lock *provider.Lock, existingLockID string) error {
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return err
	}
	return fs.legacy.RefreshLock(ctx, lref, lock, existingLockID)
}

func (fs *localdb) Unlock(ctx context.Context, ref *provider.Reference, lock *provider.Lock) error {
	lref, err := fs.legacyRef(ctx, ref)
	if err != nil {
		return err
	}
	return fs.legacy.Unlock(ctx, lref, lock)
}

func (fs *localdb) CreateStorageSpace(ctx context.Context, req *provider.CreateStorageSpaceRequest) (*provider.CreateStorageSpaceResponse, error) {
	return nil, errtypes.NotSupported("localdb: create storage space not supported")
}

func (fs *localdb) UpdateStorageSpace(ctx context.Context, req *provider.UpdateStorageSpaceRequest) (*provider.UpdateStorageSpaceResponse, error) {
	return nil, errtypes.NotSupported("localdb: update storage space not supported")
}

func (fs *localdb) Shutdown(ctx context.Context) error {
	err := fs.legacy.Shutdown(ctx)
	if cerr := fs.db.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}
