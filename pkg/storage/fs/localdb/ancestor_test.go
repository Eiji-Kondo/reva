package localdb

import (
	"context"
	"io"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	userpb "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	"github.com/cs3org/reva/v3/pkg/appctx"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func testCtx(t *testing.T) context.Context {
	t.Helper()
	u := &userpb.User{
		Id:       &userpb.UserId{OpaqueId: "einstein", Idp: "localhost", Type: userpb.UserType_USER_TYPE_PRIMARY},
		Username: "einstein",
		Mail:     "einstein@example.org",
	}
	l := zerolog.New(io.Discard)
	return appctx.WithLogger(appctx.ContextSetUser(context.Background(), u), &l)
}

func newTestFSWith(t *testing.T, m map[string]any) *localdb {
	t.Helper()
	fs, err := New(testCtx(t), m)
	require.NoError(t, err, "New")
	t.Cleanup(func() { _ = fs.Shutdown(testCtx(t)) })
	return fs.(*localdb)
}

// upload goes through the two calls a data gateway makes.
func upload(t *testing.T, drv *localdb, p, body string) {
	t.Helper()
	ctx := testCtx(t)
	res, err := drv.InitiateUpload(ctx, &provider.Reference{Path: p},
		int64(len(body)), map[string]string{})
	require.NoError(t, err, "InitiateUpload")
	id := res["tus"]
	require.NotEmpty(t, id, "upload id")
	require.NoError(t, drv.Upload(ctx, &provider.Reference{Path: id},
		io.NopCloser(strings.NewReader(body)), map[string]string{}), "Upload")
	time.Sleep(20 * time.Millisecond)
}

// diskMtimeSec is the directory mtime the row has to carry.
func diskMtimeSec(t *testing.T, drv *localdb, p string) int64 {
	t.Helper()
	fi, err := os.Stat(path.Join(drv.conf.DataDirectory, p))
	require.NoError(t, err)
	return fi.ModTime().Unix()
}

func cachedRow(t *testing.T, drv *localdb, p string) (etag string, mtimeSec int64) {
	t.Helper()
	info, err := drv.getCached(testCtx(t), p)
	require.NoError(t, err, "cached row for %s", p)
	return info.Etag, int64(info.Mtime.Seconds)
}

// TestWriteRefreshesAncestorRows: an upload moves its parent's mtime on disk
// (localfs renames the upload into place), so the cached parent row has to be
// re-read. Before the fix the row kept the mtime it was written with and a
// client polling the drive eTag never saw the change.
func TestWriteRefreshesAncestorRows(t *testing.T) {
	drv := newTestFSWith(t, map[string]any{
		"root":      t.TempDir(),
		"auto_sync": true,
	})
	ctx := testCtx(t)
	require.NoError(t, drv.CreateHome(ctx))
	home, err := drv.GetHome(ctx)
	require.NoError(t, err)
	dir := home + "/sub"
	require.NoError(t, drv.CreateDir(ctx, &provider.Reference{Path: dir}))

	beforeEtag, beforeMtime := cachedRow(t, drv, dir)
	upload(t, drv, dir+"/file.txt", "hello")
	afterEtag, afterMtime := cachedRow(t, drv, dir)

	onDisk := diskMtimeSec(t, drv, dir)
	t.Logf("%s: etag %s -> %s; mtime row %d disk %d", dir, beforeEtag, afterEtag, afterMtime, onDisk)
	require.Equal(t, onDisk, afterMtime, "the cached mtime must equal the directory mtime on disk")
	require.NotEqual(t, beforeEtag, afterEtag, "the parent eTag must move on an upload")
	require.GreaterOrEqual(t, afterMtime, beforeMtime)
}

// TestDeleteRefreshesAncestorRows is the plain parent case: deleting a file has
// to move the container that held it.
func TestDeleteRefreshesAncestorRows(t *testing.T) {
	drv := newTestFSWith(t, map[string]any{
		"root":      t.TempDir(),
		"auto_sync": true,
	})
	ctx := testCtx(t)
	require.NoError(t, drv.CreateHome(ctx))
	home, err := drv.GetHome(ctx)
	require.NoError(t, err)
	dir := home + "/sub"
	require.NoError(t, drv.CreateDir(ctx, &provider.Reference{Path: dir}))
	upload(t, drv, dir+"/file.txt", "hello")

	beforeEtag, _ := cachedRow(t, drv, dir)
	require.NoError(t, drv.Delete(ctx, &provider.Reference{Path: dir + "/file.txt"}))
	afterEtag, afterMtime := cachedRow(t, drv, dir)

	onDisk := diskMtimeSec(t, drv, dir)
	t.Logf("%s: etag %s -> %s; mtime row %d disk %d", dir, beforeEtag, afterEtag, afterMtime, onDisk)
	require.Equal(t, onDisk, afterMtime, "the cached mtime must equal the directory mtime on disk")
	require.NotEqual(t, beforeEtag, afterEtag, "the parent eTag must move on a delete")
}

// TestDeleteRefreshesEveryAncestor: localfs.propagate walks Chtimes to the root,
// so deleting two levels down moves the root as well. Only re-reading every
// ancestor, not just the direct parent, keeps the drive eTag in step.
func TestDeleteRefreshesEveryAncestor(t *testing.T) {
	drv := newTestFSWith(t, map[string]any{
		"root":         t.TempDir(),
		"disable_home": true,
		"auto_sync":    true,
	})
	ctx := testCtx(t)
	require.NoError(t, drv.syncPath(ctx, "/"))
	require.NoError(t, drv.CreateDir(ctx, &provider.Reference{Path: "/a"}))
	require.NoError(t, drv.CreateDir(ctx, &provider.Reference{Path: "/a/b"}))
	upload(t, drv, "/a/b/c.txt", "hello")

	beforeRoot, _ := cachedRow(t, drv, "/")
	require.NoError(t, drv.Delete(ctx, &provider.Reference{Path: "/a/b/c.txt"}))
	afterRoot, afterMtime := cachedRow(t, drv, "/")

	onDisk := diskMtimeSec(t, drv, "/")
	t.Logf("root: etag %s -> %s; mtime row %d disk %d", beforeRoot, afterRoot, afterMtime, onDisk)
	require.Equal(t, onDisk, afterMtime, "the root row must be re-read after a delete two levels down")
	require.NotEqual(t, beforeRoot, afterRoot, "the drive eTag must move on a delete two levels down")
}

// TestWriteRefreshesTheRootRowWithDisableHome: with disable_home the caller's
// home *is* "/", so the root row is the drive root a client polls. A walk that
// stops below the root leaves exactly that row stale.
func TestWriteRefreshesTheRootRowWithDisableHome(t *testing.T) {
	drv := newTestFSWith(t, map[string]any{
		"root":         t.TempDir(),
		"disable_home": true,
		"auto_sync":    true,
	})
	ctx := testCtx(t)
	home, err := drv.GetHome(ctx)
	require.NoError(t, err)
	require.Equal(t, "/", home, "disable_home must make the home the root")
	require.NoError(t, drv.syncPath(ctx, "/"))

	beforeEtag, _ := cachedRow(t, drv, "/")
	require.NoError(t, drv.CreateDir(ctx, &provider.Reference{Path: "/newdir"}))
	afterEtag, afterMtime := cachedRow(t, drv, "/")

	onDisk := diskMtimeSec(t, drv, "/")
	t.Logf("root: etag %s -> %s; mtime row %d disk %d", beforeEtag, afterEtag, afterMtime, onDisk)
	require.Equal(t, onDisk, afterMtime, "the root row must be re-read from disk after a write below it")
	require.NotEqual(t, beforeEtag, afterEtag, "the root eTag is the drive eTag under disable_home and must move")
}
