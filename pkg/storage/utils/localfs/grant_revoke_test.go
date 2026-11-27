package localfs

import (
	"context"
	"net/url"
	"os"
	"path"
	"testing"

	userpb "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	"github.com/cs3org/reva/v3/pkg/appctx"
	"github.com/cs3org/reva/v3/pkg/storage/utils/grants"
)

// TestRemoveGrantRevokesAccess pins the grant lifecycle of a shared resource:
// AddGrant followed by RemoveGrant must leave the grantee without access.
// AddGrant keys the ACL row with the user type ("u:marie:primary@idp") while
// RemoveGrant used to drop it ("u:marie@idp"), so the revoke matched no row and
// a recipient kept read access after the share was rejected or unshared.
func TestRemoveGrantRevokesAccess(t *testing.T) {
	config := &Config{Root: t.TempDir(), UserLayout: "{{.Username}}", ShareFolder: "/MyShares"}
	config.ApplyDefaults()
	db, err := initializeDB(config.Root, "test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fs := &localfs{conf: config, db: db}

	einsteinID := &userpb.UserId{OpaqueId: "einstein", Idp: "https://idp.example.test", Type: userpb.UserType_USER_TYPE_PRIMARY}
	marieID := &userpb.UserId{OpaqueId: "marie", Idp: "https://idp.example.test", Type: userpb.UserType_USER_TYPE_PRIMARY}
	ctxOwner := appctx.ContextSetUser(context.Background(), &userpb.User{Id: einsteinID, Username: "einstein"})
	ctxRecipient := appctx.ContextSetUser(context.Background(), &userpb.User{Id: marieID, Username: "marie"})

	ownerDir := path.Join(config.DataDirectory, "einstein", "sf")
	if err := os.MkdirAll(ownerDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path.Join(ownerDir, "shared.txt"), []byte("shared"), 0600); err != nil {
		t.Fatal(err)
	}

	resourceID := &provider.ResourceId{StorageId: "localfs", OpaqueId: "fileid-" + url.QueryEscape("einstein/sf")}
	ref := &provider.Reference{ResourceId: resourceID}
	grantee := &provider.Grantee{
		Type: provider.GranteeType_GRANTEE_TYPE_USER,
		Id:   &provider.Grantee_UserId{UserId: marieID},
	}

	if err := fs.AddGrant(ctxOwner, ref, &provider.Grant{Grantee: grantee, Permissions: grants.GetGrantPermissionSet("viewer")}); err != nil {
		t.Fatalf("AddGrant: %v", err)
	}
	perms, err := fs.inheritedGrantedPermissions(ctxRecipient, ownerDir)
	if err != nil {
		t.Fatalf("permissions after AddGrant: %v", err)
	}
	if !perms.GetStat() || !perms.GetInitiateFileDownload() {
		t.Fatalf("grantee has no read access after AddGrant: %+v", perms)
	}

	if err := fs.RemoveGrant(ctxOwner, ref, &provider.Grant{Grantee: grantee}); err != nil {
		t.Fatalf("RemoveGrant: %v", err)
	}
	perms, gerr := fs.inheritedGrantedPermissions(ctxRecipient, ownerDir)
	// A revoked grant surfaces either as an error or as an empty permission set.
	if gerr == nil && (perms.GetStat() || perms.GetInitiateFileDownload()) {
		t.Fatalf("grantee still has read access after RemoveGrant: %+v", perms)
	}

	shareTarget := &url.URL{Scheme: "cs3", Opaque: "localfs/" + resourceID.GetOpaqueId()}
	if err := fs.CreateReference(ctxRecipient, "/MyShares/sf", shareTarget); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.GetMD(ctxRecipient, &provider.Reference{Path: "/MyShares/sf"}, nil); err == nil {
		t.Fatal("share reference resolved after the grant was revoked")
	}
}

// TestRemoveGrantWithoutIdpRevokesAccess covers the shape the gateway sends: the
// share manager stores only the grantee username, so a revoke carries no idp
// while the grant was written with one. The revoke must still clear the grant,
// otherwise unsharing leaves the recipient with access.
func TestRemoveGrantWithoutIdpRevokesAccess(t *testing.T) {
	config := &Config{Root: t.TempDir(), UserLayout: "{{.Username}}", ShareFolder: "/MyShares"}
	config.ApplyDefaults()
	db, err := initializeDB(config.Root, "test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fs := &localfs{conf: config, db: db}

	einsteinID := &userpb.UserId{OpaqueId: "einstein", Idp: "https://idp.example.test", Type: userpb.UserType_USER_TYPE_PRIMARY}
	marieID := &userpb.UserId{OpaqueId: "marie", Idp: "https://idp.example.test", Type: userpb.UserType_USER_TYPE_PRIMARY}
	// The share manager only knows the username, so the id it hands back has no idp.
	marieFromShare := &userpb.UserId{OpaqueId: "marie", Type: userpb.UserType_USER_TYPE_PRIMARY}
	ctxOwner := appctx.ContextSetUser(context.Background(), &userpb.User{Id: einsteinID, Username: "einstein"})
	ctxRecipient := appctx.ContextSetUser(context.Background(), &userpb.User{Id: marieID, Username: "marie"})

	ownerDir := path.Join(config.DataDirectory, "einstein", "sf")
	if err := os.MkdirAll(ownerDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path.Join(ownerDir, "shared.txt"), []byte("shared"), 0600); err != nil {
		t.Fatal(err)
	}
	resourceID := &provider.ResourceId{StorageId: "localfs", OpaqueId: "fileid-" + url.QueryEscape("einstein/sf")}
	ref := &provider.Reference{ResourceId: resourceID}
	userGrantee := func(id *userpb.UserId) *provider.Grantee {
		return &provider.Grantee{Type: provider.GranteeType_GRANTEE_TYPE_USER, Id: &provider.Grantee_UserId{UserId: id}}
	}

	if err := fs.AddGrant(ctxOwner, ref, &provider.Grant{Grantee: userGrantee(marieID), Permissions: grants.GetGrantPermissionSet("viewer")}); err != nil {
		t.Fatalf("AddGrant: %v", err)
	}
	if perms, err := fs.inheritedGrantedPermissions(ctxRecipient, ownerDir); err != nil || !perms.GetStat() {
		t.Fatalf("grantee has no read access after AddGrant: %+v %v", perms, err)
	}

	if err := fs.RemoveGrant(ctxOwner, ref, &provider.Grant{Grantee: userGrantee(marieFromShare)}); err != nil {
		t.Fatalf("RemoveGrant without idp: %v", err)
	}
	perms, gerr := fs.inheritedGrantedPermissions(ctxRecipient, ownerDir)
	if gerr == nil && (perms.GetStat() || perms.GetInitiateFileDownload()) {
		t.Fatalf("grantee still has read access after an idp-less RemoveGrant: %+v", perms)
	}

	// A grant written without the idp must not leave an effective row behind
	// when it is revoked with the full id.
	if err := fs.AddGrant(ctxOwner, ref, &provider.Grant{Grantee: userGrantee(marieFromShare), Permissions: grants.GetGrantPermissionSet("viewer")}); err != nil {
		t.Fatalf("AddGrant without idp: %v", err)
	}
	if err := fs.RemoveGrant(ctxOwner, ref, &provider.Grant{Grantee: userGrantee(marieID)}); err != nil {
		t.Fatalf("RemoveGrant with idp: %v", err)
	}
	perms, gerr = fs.inheritedGrantedPermissions(ctxRecipient, ownerDir)
	if gerr == nil && (perms.GetStat() || perms.GetInitiateFileDownload()) {
		t.Fatalf("grantee still has read access after re-grant and idp-ful RemoveGrant: %+v", perms)
	}
}
