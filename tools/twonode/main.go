// Command twonode drives two localdb instances that share one filesystem and
// one metadata DB, to answer one question: does the same file get the same eTag
// on both nodes?
//
// Each node is a separate driver instance with its own *sql.DB pool and its own
// root directory. The roots are bind mounts of one host directory, which is
// what NFS gives a client: one set of inodes seen through two mount points.
// Separate pools matter because two revad processes would each open their own.
//
// Step 0 runs the same upload against plain localfs, so a failure further down
// can be attributed to localdb rather than to this code.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	userpb "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	"github.com/cs3org/reva/v3/pkg/appctx"
	"github.com/cs3org/reva/v3/pkg/storage"
	"github.com/cs3org/reva/v3/pkg/storage/fs/localdb"
	"github.com/cs3org/reva/v3/pkg/storage/utils/localfs"
	"github.com/rs/zerolog"
)

var (
	rootA       = flag.String("root-a", "/tmp/twonode/nodeA", "root seen by node A")
	rootB       = flag.String("root-b", "/tmp/twonode/nodeB", "root seen by node B")
	keep        = flag.Bool("keep", false, "skip the delete step so the DB can be inspected")
	autoSync    = flag.Bool("auto-sync", true, "let the driver re-read the disk on a cache miss")
	disableHome = flag.Bool("disable-home", true, "serve the root as the user's home")
)

func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "FATAL: "+f+"\n", a...)
	os.Exit(1)
}

// home is the prefix the driver serves the tree under: "" when disable_home is
// set, "/einstein" when the caller's home is a real subtree.
var home string

func pth(name string) string {
	if home == "" {
		if name == "" {
			return "/"
		}
		return "/" + name
	}
	return path.Join(home, name)
}

func newFS(root string) storage.FS {
	fs, err := localdb.New(context.Background(), map[string]any{
		"root":         root,
		"disable_home": *disableHome,
		"auto_sync":    *autoSync,
		"db_engine":    "postgres",
		"db_host":      "127.0.0.1",
		"db_port":      15432,
		"db_username":  "postgres",
		"db_password":  "pw",
		"db_name":      "reva",
	})
	if err != nil {
		fatalf("cannot build localdb on %s: %v", root, err)
	}
	return fs
}

func newPlainLocalfs(root string) storage.FS {
	c := &localfs.Config{Root: root, DisableHome: true}
	c.ApplyDefaults()
	if err := os.MkdirAll(c.Root, 0o755); err != nil {
		fatalf("mkdir ctl: %v", err)
	}
	fs, err := localfs.NewLocalFS(c)
	if err != nil {
		fatalf("cannot build localfs on %s: %v", root, err)
	}
	return fs
}

func ctxFor() context.Context {
	u := &userpb.User{Id: &userpb.UserId{OpaqueId: "einstein", Idp: "local"}, Username: "einstein"}
	l := zerolog.New(os.Stderr).Level(zerolog.WarnLevel)
	return appctx.WithLogger(appctx.ContextSetUser(context.Background(), u), &l)
}

type row struct {
	etag string
	id   string
	size uint64
}

func md(fs storage.FS, ctx context.Context, p string) row {
	info, err := fs.GetMD(ctx, &provider.Reference{Path: p}, nil)
	if err != nil {
		fatalf("GetMD %s: %v", p, err)
	}
	return row{etag: info.Etag, id: info.Id.OpaqueId, size: info.Size}
}

func (r row) String() string {
	return fmt.Sprintf("etag=%s id=%s size=%d", r.etag, r.id, r.size)
}

var failures int

func check(label string, ok bool, detail string) {
	if ok {
		fmt.Printf("  [PASS] %-48s %s\n", label, detail)
		return
	}
	failures++
	fmt.Printf("  [FAIL] %-48s %s\n", label, detail)
}

// uploadVia runs the two calls a data gateway makes. InitiateUpload returns an
// upload id, and localfs.Upload looks the session up by that id, which arrives
// in the reference path rather than in the destination path.
func uploadVia(fs storage.FS, ctx context.Context, p, body string) error {
	res, err := fs.InitiateUpload(ctx, &provider.Reference{Path: p},
		int64(len(body)), map[string]string{})
	if err != nil {
		return fmt.Errorf("InitiateUpload: %w", err)
	}
	id := res["tus"]
	if id == "" {
		return fmt.Errorf("InitiateUpload returned no upload id: %v", res)
	}
	err = fs.Upload(ctx, &provider.Reference{Path: id},
		io.NopCloser(strings.NewReader(body)), map[string]string{})
	time.Sleep(20 * time.Millisecond)
	return err
}

func diskSize(root, name string) int64 {
	rel := strings.TrimPrefix(pth(name), "/")
	fi, err := os.Stat(filepath.Join(root, "data", rel))
	if err != nil {
		return -1
	}
	return fi.Size()
}

func appendTo(root, name, s string) {
	rel := strings.TrimPrefix(pth(name), "/")
	fh, err := os.OpenFile(filepath.Join(root, "data", rel), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fatalf("open for append: %v", err)
	}
	if _, err := fh.WriteString(s); err != nil {
		fatalf("append: %v", err)
	}
	fh.Close()
	time.Sleep(20 * time.Millisecond)
}

func main() {
	flag.Parse()
	for _, r := range []string{*rootA, *rootB} {
		if err := os.MkdirAll(filepath.Join(r, "data"), 0o755); err != nil {
			fatalf("mkdir %s: %v", r, err)
		}
	}

	if !*disableHome {
		home = "/einstein"
	}

	ctx := ctxFor()
	a := newFS(*rootA)
	b := newFS(*rootB)
	if home != "" {
		if err := a.CreateHome(ctx); err != nil {
			fatalf("CreateHome: %v", err)
		}
	}

	fmt.Println("== 0. control: the same upload against plain localfs, no DB ==")
	ctlRoot := filepath.Join(*rootA, "ctl")
	errC := uploadVia(newPlainLocalfs(ctlRoot), ctx, "/ctl.txt", "hello")
	fmt.Printf("  localfs Upload returned: %v\n", errC)
	fmt.Printf("  bytes on disk: %d (expected 5)\n", diskSize(ctlRoot, "ctl.txt"))
	check("plain localfs accepts the same upload", errC == nil, fmt.Sprint(errC))

	fmt.Println("\n== 1. create through node A, read from node B ==")
	if err := a.TouchFile(ctx, &provider.Reference{Path: pth("shared.txt")}); err != nil {
		fatalf("node A TouchFile: %v", err)
	}
	ra := md(a, ctx, pth("shared.txt"))
	rb := md(b, ctx, pth("shared.txt"))
	fmt.Printf("  node A: %s\n", ra)
	fmt.Printf("  node B: %s\n", rb)
	check("same eTag on both nodes", ra.etag == rb.etag, fmt.Sprintf("A=%s B=%s", ra.etag, rb.etag))
	check("same file id on both nodes", ra.id == rb.id, fmt.Sprintf("A=%s B=%s", ra.id, rb.id))

	fmt.Println("\n== 2. upload 10 bytes through node A (the real reva path) ==")
	errA := uploadVia(a, ctx, pth("shared.txt"), "0123456789")
	fmt.Printf("  Upload returned: %v\n", errA)
	fmt.Printf("  bytes on disk: %d (expected 10)\n", diskSize(*rootA, "shared.txt"))
	check("Upload reported success", errA == nil, fmt.Sprint(errA))
	ra2 := md(a, ctx, pth("shared.txt"))
	fmt.Printf("  node A still says: %s\n", ra2)
	if ra2.size == 0 && diskSize(*rootA, "shared.txt") == 10 {
		fmt.Println("  -> the bytes landed but the cache row was never refreshed")
	}

	fmt.Println("\n== 2c. the other Upload contract: a plain file path, as simple.go sends ==")
	// pkg/rhttp/datatx/manager/simple passes r.URL.Path, not an upload id.
	// localfs.Upload resolves its argument with GetUpload, so a path finds no
	// session. Recording the result separates the two contracts.
	errP := a.Upload(ctx, &provider.Reference{Path: pth("pathtest.txt")},
		io.NopCloser(strings.NewReader("PATHCONTRACT")), map[string]string{})
	fmt.Printf("  Upload(path) returned: %v\n", errP)
	fmt.Printf("  bytes on disk: %d (12 would mean it wrote)\n", diskSize(*rootA, "pathtest.txt"))
	check("a plain path is rejected by localfs.Upload", errP != nil, fmt.Sprint(errP))

	fmt.Println("\n== 2b. force a child resync through ListFolder ==")
	// ListFolder calls ensureDirCached, which runs syncChildren on every read
	// when AutoSync is on. That is the reconciler path, and it needs no
	// precondition: it simply stats each child and upserts it.
	if _, err := a.ListFolder(ctx, &provider.Reference{Path: pth("")}, nil); err != nil {
		fmt.Printf("  ListFolder returned: %v\n", err)
	}
	ra2b := md(a, ctx, pth("shared.txt"))
	fmt.Printf("  node A after syncChildren: %s\n", ra2b)
	check("the child row picked up the new size", ra2b.size == 10, fmt.Sprintf("size=%d", ra2b.size))
	check("the child row picked up the new eTag", ra2b.etag != ra.etag, fmt.Sprintf("%s -> %s", ra.etag, ra2b.etag))
	fmt.Println("  -> a failure here with a unique-violation on inode_path_key means")
	fmt.Println("     the upsert cannot follow a file whose inode changed")

	fmt.Println("\n== 3. after a reconcile pass (fresh driver), do both nodes agree? ==")
	// A fresh driver runs syncTree at startup, which is what a restart or a
	// reconciler pass does. This separates cache staleness from eTag identity.
	reconciled := newFS(*rootA)
	ra3 := md(reconciled, ctx, pth("shared.txt"))
	rb3 := md(b, ctx, pth("shared.txt"))
	fmt.Printf("  node A (reconciled): %s\n", ra3)
	fmt.Printf("  node B             : %s\n", rb3)
	check("both nodes agree on the new eTag", ra3.etag == rb3.etag, fmt.Sprintf("A=%s B=%s", ra3.etag, rb3.etag))
	check("both nodes agree on size 10", ra3.size == 10 && rb3.size == 10, fmt.Sprintf("A=%d B=%d", ra3.size, rb3.size))
	check("eTag changed from the empty file", ra3.etag != ra.etag, fmt.Sprintf("%s -> %s", ra.etag, ra3.etag))
	check("the file id followed the new inode", ra3.id != ra.id, fmt.Sprintf("%s -> %s", ra.id, ra3.id))

	fmt.Println("\n== 4. upload through node B, then reconcile node A ==")
	errB := uploadVia(b, ctx, pth("shared.txt"), "0123456789ABCDEF")
	fmt.Printf("  Upload on B returned: %v\n", errB)
	fmt.Printf("  bytes on disk: %d (expected 16)\n", diskSize(*rootB, "shared.txt"))
	reconciledA := newFS(*rootA)
	ra4 := md(reconciledA, ctx, pth("shared.txt"))
	rb4 := md(b, ctx, pth("shared.txt"))
	fmt.Printf("  node A (reconciled): %s\n", ra4)
	fmt.Printf("  node B             : %s\n", rb4)
	check("A sees B's write after reconciling", ra4.size == 16, fmt.Sprintf("A=%d", ra4.size))
	check("both nodes agree after B's write", ra4.etag == rb4.etag, fmt.Sprintf("A=%s B=%s", ra4.etag, rb4.etag))
	check("the id changed again on B's upload", ra4.id != ra3.id, fmt.Sprintf("%s -> %s", ra3.id, ra4.id))

	fmt.Println("\n== 5. out-of-band write: does a DB hit ever look at the disk? ==")
	// Nothing goes through the driver here. This is what a second writer outside
	// reva creates: another process, or an NFS client on another host writing
	// into the same export.
	before := ra4.size
	appendTo(*rootA, "shared.txt", "XXXXXXXXXX")
	ra5 := md(reconciledA, ctx, pth("shared.txt"))
	rb5 := md(b, ctx, pth("shared.txt"))
	fmt.Printf("  disk is now %d bytes; node A says %s\n", diskSize(*rootA, "shared.txt"), ra5)
	fmt.Printf("                            node B says %s\n", rb5)
	check("both nodes still agree with each other", ra5.etag == rb5.etag, fmt.Sprintf("A=%s B=%s", ra5.etag, rb5.etag))
	if ra5.size == before {
		fmt.Println("  -> STALE: the cached row was served and the disk was never stat'ed")
	} else {
		fmt.Println("  -> fresh: the disk was re-read")
	}

	fmt.Println("\n== 6. delete through node B, read through node A ==")
	if *keep {
		fmt.Println("  skipped (-keep), the DB can be inspected now")
	} else if err := b.Delete(ctx, &provider.Reference{Path: pth("shared.txt")}); err != nil {
		fmt.Printf("  node B Delete: %v\n", err)
	} else {
		_, errA := reconciledA.GetMD(ctx, &provider.Reference{Path: pth("shared.txt")}, nil)
		_, errB := b.GetMD(ctx, &provider.Reference{Path: pth("shared.txt")}, nil)
		fmt.Printf("  after B deletes: A err=%v\n", errA)
		fmt.Printf("                   B err=%v\n", errB)
		check("A no longer finds the file", errA != nil, fmt.Sprint(errA))
		check("B no longer finds the file", errB != nil, fmt.Sprint(errB))
	}

	fmt.Println()
	if failures == 0 {
		fmt.Println("RESULT: every check passed")
		return
	}
	fmt.Printf("RESULT: %d check(s) FAILED\n", failures)
	os.Exit(1)
}
