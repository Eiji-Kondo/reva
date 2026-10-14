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

package localfs

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path"
	"testing"
	"time"

	userpb "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	typespb "github.com/cs3org/go-cs3apis/cs3/types/v1beta1"
	"github.com/cs3org/reva/v3/pkg/appctx"
	"github.com/cs3org/reva/v3/pkg/errtypes"
)

func TestMsToTimestamp(t *testing.T) {
	ts := msToTimestamp(1790909115123)
	if ts.Seconds != 1790909115 {
		t.Errorf("Seconds = %d, want 1790909115", ts.Seconds)
	}
	if ts.Nanos != 123000000 {
		t.Errorf("Nanos = %d, want 123000000", ts.Nanos)
	}
}

func TestLocalFSLockLifecycle(t *testing.T) {
	config := &Config{Root: t.TempDir(), UserLayout: "{{.Username}}"}
	config.ApplyDefaults()
	db, err := initializeDB(config.Root, "test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	filePath := path.Join(config.DataDirectory, "einstein", "locked.txt")
	if err := os.MkdirAll(path.Dir(filePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte("locked"), 0600); err != nil {
		t.Fatal(err)
	}
	fs := &localfs{conf: config, db: db}
	ctx := appctx.ContextSetUser(context.Background(), &userpb.User{
		Id: &userpb.UserId{
			OpaqueId: "einstein",
			Idp:      "https://idp.example.test",
			Type:     userpb.UserType_USER_TYPE_PRIMARY,
		},
		Username: "einstein",
	})
	ref := &provider.Reference{Path: "/locked.txt"}
	lock := &provider.Lock{
		LockId:     "opaquelocktoken:test-lock",
		Type:       provider.LockType_LOCK_TYPE_EXCL,
		User:       appctx.ContextMustGetUser(ctx).GetId(),
		Expiration: &typespb.Timestamp{Seconds: uint64(time.Now().Add(time.Hour).Unix())},
	}

	if err := fs.SetLock(ctx, ref, lock); err != nil {
		t.Fatalf("set lock: %v", err)
	}
	reopenedDB, err := initializeDB(config.Root, "test.db")
	if err != nil {
		t.Fatal(err)
	}

	defer reopenedDB.Close()
	fs.db = reopenedDB
	if err := fs.SetLock(ctx, ref, lock); err == nil {
		t.Fatal("setting a second lock succeeded")
	} else if _, ok := err.(errtypes.Conflict); !ok {
		t.Fatalf("second lock error = %T (%v), want conflict", err, err)
	}
	got, err := fs.GetLock(ctx, ref)
	if err != nil {
		t.Fatalf("get lock: %v", err)
	}
	if got.GetLockId() != lock.GetLockId() {
		t.Fatalf("lock id = %q, want %q", got.GetLockId(), lock.GetLockId())
	}
	wrongLock := &provider.Lock{
		LockId: "opaquelocktoken:wrong-lock",
		Type:   lock.GetType(),
		User:   lock.GetUser(),
	}
	if err := fs.Unlock(ctx, ref, wrongLock); err == nil {
		t.Fatal("unlock with wrong token succeeded")
	}
	strangerCtx := appctx.ContextSetUser(context.Background(), &userpb.User{
		Id:       &userpb.UserId{OpaqueId: "marie", Idp: "https://idp.example.test", Type: userpb.UserType_USER_TYPE_PRIMARY},
		Username: "marie",
	})
	if err := fs.Unlock(strangerCtx, ref, &provider.Lock{
		LockId: lock.GetLockId(),
		Type:   lock.GetType(),
		User:   appctx.ContextMustGetUser(strangerCtx).GetId(),
	}); err == nil {
		t.Fatal("unlock by a different user succeeded")
	}
	refreshed := &provider.Lock{
		LockId:     lock.GetLockId(),
		Type:       lock.GetType(),
		User:       lock.GetUser(),
		Expiration: &typespb.Timestamp{Seconds: uint64(time.Now().Add(2 * time.Hour).Unix())},
	}
	if err := fs.RefreshLock(ctx, ref, refreshed, lock.GetLockId()); err != nil {
		t.Fatalf("refresh lock: %v", err)
	}
	if err := fs.Unlock(ctx, ref, refreshed); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if _, err := fs.GetLock(ctx, ref); err == nil {
		t.Fatal("lock remains after unlock")
	}
	expiredLock := &provider.Lock{
		LockId:     "opaquelocktoken:expired-lock",
		Type:       lock.GetType(),
		User:       lock.GetUser(),
		Expiration: &typespb.Timestamp{Seconds: uint64(time.Now().Add(-time.Minute).Unix())},
	}
	if err := fs.SetLock(ctx, ref, expiredLock); err != nil {
		t.Fatalf("set expired lock fixture: %v", err)
	}
	if _, err := fs.GetLock(ctx, ref); err == nil {
		t.Fatal("expired lock is still active")
	}
}

func TestLocalFSSharedLockLifecycle(t *testing.T) {
	config := &Config{Root: t.TempDir(), UserLayout: "{{.Username}}"}
	config.ApplyDefaults()
	config.DisableHome = true
	db, err := initializeDB(config.Root, "test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	filePath := path.Join(config.DataDirectory, "shared-locked.txt")
	if err := os.MkdirAll(path.Dir(filePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte("shared"), 0600); err != nil {
		t.Fatal(err)
	}
	fs := &localfs{conf: config, db: db}
	makeContext := func(username string) context.Context {
		return appctx.ContextSetUser(context.Background(), &userpb.User{
			Id: &userpb.UserId{
				OpaqueId: username,
				Idp:      "https://idp.example.test",
				Type:     userpb.UserType_USER_TYPE_PRIMARY,
			},
			Username: username,
		})
	}
	ref := &provider.Reference{Path: "/shared-locked.txt"}
	aliceCtx, bobCtx := makeContext("alice"), makeContext("bob")
	aliceLock := &provider.Lock{
		LockId: "opaquelocktoken:alice",
		Type:   provider.LockType_LOCK_TYPE_SHARED,
		User:   appctx.ContextMustGetUser(aliceCtx).GetId(),
	}
	bobLock := &provider.Lock{
		LockId: "opaquelocktoken:bob",
		Type:   provider.LockType_LOCK_TYPE_SHARED,
		User:   appctx.ContextMustGetUser(bobCtx).GetId(),
	}
	if err := fs.SetLock(aliceCtx, ref, aliceLock); err != nil {
		t.Fatalf("set first shared lock: %v", err)
	}
	if err := fs.SetLock(bobCtx, ref, bobLock); err != nil {
		t.Fatalf("set second shared lock: %v", err)
	}
	got, err := fs.GetLock(aliceCtx, ref)
	if err != nil {
		t.Fatalf("get shared locks: %v", err)
	}
	entry := got.GetOpaque().GetMap()[lockSetOpaqueKey]
	if entry == nil {
		t.Fatal("multiple shared lock set was not returned in opaque metadata")
	}
	var storedLocks []*provider.Lock
	if err := json.Unmarshal(entry.GetValue(), &storedLocks); err != nil {
		t.Fatalf("decode shared lock set: %v", err)
	}
	if len(storedLocks) != 2 {
		t.Fatalf("stored shared locks = %d, want 2", len(storedLocks))
	}

	exclusiveLock := &provider.Lock{
		LockId: "opaquelocktoken:exclusive",
		Type:   provider.LockType_LOCK_TYPE_EXCL,
		User:   appctx.ContextMustGetUser(aliceCtx).GetId(),
	}
	if err := fs.SetLock(aliceCtx, ref, exclusiveLock); err == nil {
		t.Fatal("exclusive lock succeeded while shared locks existed")
	} else if _, ok := err.(errtypes.Conflict); !ok {
		t.Fatalf("exclusive lock error = %T (%v), want conflict", err, err)
	}
	if err := fs.Unlock(aliceCtx, ref, aliceLock); err != nil {
		t.Fatalf("unlock first shared lock: %v", err)
	}
	got, err = fs.GetLock(bobCtx, ref)
	if err != nil {
		t.Fatalf("get remaining shared lock: %v", err)
	}
	if got.GetLockId() != bobLock.GetLockId() {
		t.Fatalf("remaining lock id = %q, want %q", got.GetLockId(), bobLock.GetLockId())
	}
	if err := fs.Unlock(bobCtx, ref, bobLock); err != nil {
		t.Fatalf("unlock second shared lock: %v", err)
	}
	if err := fs.SetLock(aliceCtx, ref, exclusiveLock); err != nil {
		t.Fatalf("exclusive lock after shared unlock: %v", err)
	}
}

func TestSharedResourcePathUsesShareACL(t *testing.T) {
	config := &Config{
		Root:        t.TempDir(),
		UserLayout:  "{{.Username}}",
		ShareFolder: "/MyShares",
	}
	config.ApplyDefaults()
	db, err := initializeDB(config.Root, "test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	fs := &localfs{conf: config, db: db}
	marieID := &userpb.UserId{
		OpaqueId: "marie",
		Idp:      "https://idp.example.test",
		Type:     userpb.UserType_USER_TYPE_PRIMARY,
	}
	ctx := appctx.ContextSetUser(context.Background(), &userpb.User{
		Id:       marieID,
		Username: "marie",
		Groups:   []string{"scientists"},
	})

	ownerFile := path.Join(config.DataDirectory, "einstein", "project", "shared.txt")
	if err := os.MkdirAll(path.Dir(ownerFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownerFile, []byte("shared"), 0600); err != nil {
		t.Fatal(err)
	}

	resourceID := &provider.ResourceId{
		OpaqueId: "fileid-" + url.QueryEscape("einstein/project/shared.txt"),
	}
	if _, err := fs.GetPathByID(ctx, resourceID); err == nil {
		t.Fatal("expected an unshared foreign resource to be denied")
	}

	if err := fs.addToACLDB(ctx, path.Dir(ownerFile), "u:marie:primary@https://idp.example.test", "r!d"); err != nil {
		t.Fatal(err)
	}
	resolvedPath, err := fs.GetPathByID(ctx, resourceID)
	if err != nil {
		t.Fatalf("resolve viewer share: %v", err)
	}
	if want := path.Join(foreignResourcePathPrefix, "einstein/project/shared.txt"); resolvedPath != want {
		t.Fatalf("resolved path = %q, want %q", resolvedPath, want)
	}

	permissions, err := fs.permissionSet(ctx, &userpb.UserId{
		OpaqueId: "einstein",
		Idp:      marieID.Idp,
		Type:     userpb.UserType_USER_TYPE_PRIMARY,
	}, ownerFile)
	if err != nil {
		t.Fatalf("get viewer permissions: %v", err)
	}
	if !permissions.GetStat() || !permissions.GetInitiateFileDownload() {
		t.Fatalf("viewer permissions do not allow reading: %+v", permissions)
	}
	if permissions.GetInitiateFileUpload() || permissions.GetDelete() {
		t.Fatalf("viewer permissions unexpectedly allow writes: %+v", permissions)
	}

	if err := fs.addToACLDB(ctx, ownerFile, "u:marie:primary@https://idp.example.test", "rw!d"); err != nil {
		t.Fatal(err)
	}
	permissions, err = fs.permissionSet(ctx, &userpb.UserId{
		OpaqueId: "einstein",
		Idp:      marieID.Idp,
		Type:     userpb.UserType_USER_TYPE_PRIMARY,
	}, ownerFile)
	if err != nil {
		t.Fatalf("get editor permissions: %v", err)
	}
	if !permissions.GetInitiateFileUpload() {
		t.Fatalf("editor permissions do not allow writes: %+v", permissions)
	}

	shareTarget := &url.URL{
		Scheme: "cs3",
		Opaque: "localfs/" + resourceID.GetOpaqueId(),
	}
	if err := fs.CreateReference(ctx, "/MyShares/shared.txt", shareTarget); err != nil {
		t.Fatalf("create editor share reference: %v", err)
	}
	lock := &provider.Lock{
		LockId: "opaquelocktoken:shared-reference",
		Type:   provider.LockType_LOCK_TYPE_SHARED,
		User:   marieID,
	}
	if err := fs.SetLock(ctx, &provider.Reference{Path: "/MyShares/shared.txt"}, lock); err != nil {
		t.Fatalf("lock shared reference: %v", err)
	}
	ownerCtx := appctx.ContextSetUser(context.Background(), &userpb.User{
		Id:       &userpb.UserId{OpaqueId: "einstein", Idp: marieID.GetIdp(), Type: userpb.UserType_USER_TYPE_PRIMARY},
		Username: "einstein",
	})
	gotLock, err := fs.GetLock(ownerCtx, &provider.Reference{Path: "/project/shared.txt"})
	if err != nil {
		t.Fatalf("get lock through owner path: %v", err)
	}
	if gotLock.GetLockId() != lock.GetLockId() {
		t.Fatalf("lock through owner path = %q, want %q", gotLock.GetLockId(), lock.GetLockId())
	}
}

func TestGetPathByIDKeepsUserHomeAndRejectsPrivatePaths(t *testing.T) {
	config := &Config{Root: t.TempDir(), UserLayout: "{{.Username}}"}
	config.ApplyDefaults()
	fs := &localfs{conf: config}
	ctx := appctx.ContextSetUser(context.Background(), &userpb.User{
		Id:       &userpb.UserId{OpaqueId: "marie", Type: userpb.UserType_USER_TYPE_PRIMARY},
		Username: "marie",
	})

	homePath, err := fs.GetPathByID(ctx, &provider.ResourceId{
		OpaqueId: "fileid-" + url.QueryEscape("marie/docs/report.txt"),
	})
	if err != nil {
		t.Fatalf("resolve resource in own home: %v", err)
	}
	if homePath != "/docs/report.txt" {
		t.Fatalf("own-home path = %q, want %q", homePath, "/docs/report.txt")
	}

	if _, err := fs.resolve(ctx, &provider.Reference{Path: foreignResourcePathPrefix + "/einstein/secret.txt"}); err == nil {
		t.Fatal("expected direct access to private foreign path to be denied")
	}
	if _, err := fs.GetPathByID(ctx, &provider.ResourceId{OpaqueId: "fileid-..%2Fsecret.txt"}); err == nil {
		t.Fatal("expected traversal resource id to be denied")
	}
}
