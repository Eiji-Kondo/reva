package localdb

import (
	"context"
	"database/sql"
	iofs "io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	userpb "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	typepb "github.com/cs3org/go-cs3apis/cs3/types/v1beta1"
	"github.com/cs3org/reva/v3/pkg/errtypes"
	"github.com/cs3org/reva/v3/pkg/storage/utils/templates"
	"github.com/pkg/errors"
)

// statCached answers from the DB and falls back to the disk on a miss. The
// fallback is what lets Phase 1 serve a tree localfs already populated without
// a migration step.
func (fs *localdb) statCached(ctx context.Context, p string) (*provider.ResourceInfo, error) {
	info, err := fs.getCached(ctx, p)
	if err == nil {
		return info, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if !fs.conf.AutoSync {
		return nil, errtypes.NotFound(p)
	}
	if err := fs.syncPath(ctx, p); err != nil {
		return nil, err
	}
	info, err = fs.getCached(ctx, p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errtypes.NotFound(p)
	}
	return info, err
}

func (fs *localdb) ensureDirCached(ctx context.Context, p string) error {
	info, err := fs.statCached(ctx, p)
	if err != nil {
		return err
	}
	if info.Type != provider.ResourceType_RESOURCE_TYPE_CONTAINER {
		return errtypes.BadRequest("localdb: not a container: " + p)
	}
	// A cached directory may have gained children since the last sync.
	if fs.conf.AutoSync {
		return fs.syncChildren(ctx, p)
	}
	return nil
}

func (fs *localdb) getCached(ctx context.Context, p string) (*provider.ResourceInfo, error) {
	var (
		rowPath string
		size    int64
		mtime   int64
		etag    string
		isDir   bool
		fileID  string
	)
	err := fs.db.QueryRowContext(ctx, fs.q(
		`SELECT path, size, mtime, etag, is_dir, file_id FROM inode WHERE path = ?`), p,
	).Scan(&rowPath, &size, &mtime, &etag, &isDir, &fileID)
	if err != nil {
		return nil, err
	}
	return buildResourceInfo(rowPath, size, mtime, etag, isDir, fileID), nil
}

// syncPath records one node from the disk.
func (fs *localdb) syncPath(ctx context.Context, p string) error {
	fi, err := os.Lstat(fs.absPath(p))
	if err != nil {
		if os.IsNotExist(err) {
			return errtypes.NotFound(p)
		}
		return errors.Wrapf(err, "localdb: stat %s", p)
	}
	return fs.upsert(ctx, p, fi)
}

// syncChildren records the direct children of a container.
func (fs *localdb) syncChildren(ctx context.Context, p string) error {
	entries, err := os.ReadDir(fs.absPath(p))
	if err != nil {
		if os.IsNotExist(err) {
			return errtypes.NotFound(p)
		}
		return errors.Wrapf(err, "localdb: readdir %s", p)
	}
	for _, e := range entries {
		fi, ierr := e.Info()
		if ierr != nil {
			if os.IsNotExist(ierr) {
				continue // raced with a delete
			}
			return errors.Wrapf(ierr, "localdb: info %s", e.Name())
		}
		if err := fs.upsert(ctx, path.Join(p, e.Name()), fi); err != nil {
			return err
		}
	}
	return nil
}

// syncTree walks p and records everything below it. Phase 1 calls it on a home
// at startup and on a moved subtree.
func (fs *localdb) syncTree(ctx context.Context, p string) error {
	return filepath.WalkDir(fs.absPath(p), func(abs string, d iofs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		rel, rerr := fs.relPath(abs)
		if rerr != nil {
			return rerr
		}
		fi, ierr := d.Info()
		if ierr != nil {
			if os.IsNotExist(ierr) {
				return nil
			}
			return ierr
		}
		return fs.upsert(ctx, rel, fi)
	})
}

func (fs *localdb) resyncRef(ctx context.Context, ref *provider.Reference) error {
	p, err := fs.resolvePath(ctx, ref)
	if err != nil {
		return err
	}
	return fs.syncPath(ctx, p)
}

// legacyRef converts a driver path, which carries the home prefix, into the
// reference localfs expects, which is relative to the user's home: localfs
// wraps a path as DataDirectory/<home>/<p>, so passing the prefixed path would
// nest the home twice.
func (fs *localdb) legacyRef(ctx context.Context, ref *provider.Reference) (*provider.Reference, error) {
	p, err := fs.resolvePath(ctx, ref)
	if err != nil {
		return nil, err
	}
	home, err := fs.GetHome(ctx)
	if err != nil {
		return nil, err
	}
	rel := strings.TrimPrefix(p, path.Clean(home))
	if rel == "" {
		rel = "/"
	}
	return &provider.Reference{Path: rel}, nil
}

// purgeSubtree drops a node and everything under it.
func (fs *localdb) purgeSubtree(ctx context.Context, p string) error {
	_, err := fs.db.ExecContext(ctx, fs.q(
		`DELETE FROM inode WHERE path = ? OR path LIKE ?`), p, p+"/%")
	return errors.Wrap(err, "localdb: purge subtree")
}

// upsert writes one row and refreshes every container above it. A write below a
// directory moves that directory's mtime on disk (localfs.propagate walks the
// path with Chtimes), but the cached rows keep the mtime they were written with,
// so a client that polls the drive eTag never sees a change below it: measured
// at 1795678816550043969-4096 across PUT, MKCOL and DELETE while the directory on
// disk had moved on.
func (fs *localdb) upsert(ctx context.Context, p string, fi os.FileInfo) error {
	if err := fs.upsertRow(ctx, p, fi); err != nil {
		return err
	}
	return fs.refreshAncestors(ctx, p)
}

// refreshAncestors re-reads every directory from p's parent up to the root.
func (fs *localdb) refreshAncestors(ctx context.Context, p string) error {
	// The root is included on purpose. With disable_home the caller's home *is* "/",
	// so it is the drive root a client polls, and stopping below it would leave that
	// row exactly as stale as the case this function exists to fix.
	for dir := path.Dir(p); ; dir = path.Dir(dir) {
		if dir == "." {
			break
		}
		fi, err := os.Lstat(fs.absPath(dir))
		if err != nil {
			if os.IsNotExist(err) {
				if dir == "/" {
					break
				}
				continue
			}
			return errors.Wrapf(err, "localdb: stat %s", dir)
		}
		if err := fs.upsertRow(ctx, dir, fi); err != nil {
			return err
		}
		if dir == "/" {
			break
		}
	}
	return nil
}

// upsertRow writes one row. The file id comes from the inode number, so a rename
// does not change identity.
func (fs *localdb) upsertRow(ctx context.Context, p string, fi os.FileInfo) error {
	parent, name := path.Dir(p), fi.Name()
	if p == "/" {
		parent, name = "/", "/"
	}
	// Conflict on path, not file_id: a rename in localfs replaces the inode, so
	// the row has to follow the path to the new inode. Conflict on file_id would
	// insert a second row for a path that already exists and abort the sync.
	_, err := fs.db.ExecContext(ctx, fs.q(`
		INSERT INTO inode (file_id, path, parent, name, size, mtime, etag, is_dir)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (path) DO UPDATE SET
			file_id = excluded.file_id,
			parent = excluded.parent,
			name = excluded.name,
			size = excluded.size,
			mtime = excluded.mtime,
			etag = excluded.etag,
			is_dir = excluded.is_dir`),
		fileID(fi), p, parent, name, fi.Size(), fi.ModTime().Unix(), etagOf(fi), fi.IsDir(),
	)
	return errors.Wrapf(err, "localdb: upsert %s", p)
}

// fileID prefers the inode number so identity survives renames. Where the
// platform does not expose one it falls back to a name/mtime digest, which
// still beats a path.
func fileID(fi os.FileInfo) string {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Ino != 0 {
		return strconv.FormatUint(st.Ino, 10)
	}
	return etagOf(fi)
}

// etagOf is a cheap change token: mtime plus size. That is what the disk can
// tell us without reading the file, which is the point of a cached column.
func etagOf(fi os.FileInfo) string {
	return strconv.FormatInt(fi.ModTime().UnixNano(), 10) + "-" +
		strconv.FormatInt(fi.Size(), 10)
}

func (fs *localdb) absPath(p string) string {
	// localfs keeps bytes under root/data, so both drivers address the same
	// files and a Phase 1 switch needs no migration.
	return path.Join(fs.conf.DataDirectory, p)
}

func (fs *localdb) relPath(abs string) (string, error) {
	root := path.Clean(fs.conf.DataDirectory)
	clean := path.Clean(abs)
	if clean == root {
		return "/", nil
	}
	if !strings.HasPrefix(clean, root+"/") {
		return "", errtypes.InternalError("localdb: path outside root: " + abs)
	}
	return strings.TrimPrefix(clean, root), nil
}

// resolvePath turns a reference into a tree path. Phase 1 uses paths as the
// public handle and the file id as the internal one.
func (fs *localdb) resolvePath(ctx context.Context, ref *provider.Reference) (string, error) {
	if ref == nil {
		return "", errtypes.BadRequest("localdb: nil reference")
	}
	if ref.ResourceId != nil && ref.ResourceId.OpaqueId != "" {
		p, err := fs.GetPathByID(ctx, ref.ResourceId)
		if err != nil {
			return "", err
		}
		if ref.Path != "" {
			return path.Join(p, ref.Path), nil
		}
		return p, nil
	}
	if ref.Path == "" {
		return "", errtypes.BadRequest("localdb: reference has neither path nor id")
	}
	return path.Clean("/" + ref.Path), nil
}

// q rewrites ? placeholders to $N for PostgreSQL, which rejects ?.
func (fs *localdb) q(query string) string {
	if fs.dialect != dialectPostgres || !strings.Contains(query, "?") {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}

func buildResourceInfo(p string, size, mtime int64, etag string, isDir bool, fileID string) *provider.ResourceInfo {
	t := provider.ResourceType_RESOURCE_TYPE_FILE
	if isDir {
		t = provider.ResourceType_RESOURCE_TYPE_CONTAINER
	}
	return &provider.ResourceInfo{
		Id:    &provider.ResourceId{StorageId: driverName, OpaqueId: fileID},
		Path:  p,
		Name:  path.Base(p),
		Size:  uint64(size),
		Type:  t,
		Etag:  etag,
		Mtime: &typepb.Timestamp{Seconds: uint64(mtime)},
	}
}

func userHome(layout string, u *userpb.User) string {
	if layout == "" {
		return "/" + u.Username
	}
	return path.Clean("/" + templates.WithUser(u, layout))
}
