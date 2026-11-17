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
	"log"
	"os"
	"reflect"
	"testing"
	"time"

	userpb "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	"github.com/cs3org/reva/v3/pkg/appctx"
	"github.com/cs3org/reva/v3/pkg/permissions"
	projects_catalogue "github.com/cs3org/reva/v3/pkg/projects"
	"github.com/cs3org/reva/v3/pkg/spaces"
)

// You can use testing.T, if you want to test the code without benchmarking
func setupSuite(tb testing.TB) (projects_catalogue.Catalogue, error, func(tb testing.TB) error) {
	ctx := context.Background()
	dbName := "test_db.sqlite"
	cfg := map[string]any{
		"db_engine": "sqlite",
		"db_name":   dbName,
	}
	mgr, err := New(ctx, cfg)
	if err != nil {
		return nil, err, nil
	}

	// Return a function to teardown the test
	return mgr, nil, func(tb testing.TB) error {
		log.Println("teardown suite")
		return os.Remove(dbName)
	}
}

func TestListProjects(t *testing.T) {

	spaceID := spaces.EncodeSpaceID("/path/to/project")

	tests := []struct {
		description string
		projects    []*Project
		user        *userpb.User
		expected    []*provider.StorageSpace
	}{
		{
			description: "empty list",
			projects:    []*Project{},
			user:        &userpb.User{Id: &userpb.UserId{OpaqueId: "opaque", Idp: "idp"}},
			expected:    []*provider.StorageSpace{},
		},
		{
			description: "user is owner of the projects",
			projects: []*Project{
				{
					StorageID: "storage_id",
					SpaceID:   spaceID,
					Path:      "/path/to/project",
					Name:      "project",
					Owner:     "owner",
					Readers:   "project-readers",
					Writers:   "project-writers",
					Admins:    "project-admins",
				},
			},
			user: &userpb.User{Id: &userpb.UserId{OpaqueId: "owner", Idp: "idp"}},
			expected: []*provider.StorageSpace{
				{
					Id: &provider.StorageSpaceId{
						OpaqueId: spaces.EncodeStorageSpaceID("storage_id", spaceID),
					},
					Owner: &userpb.User{
						Id: &userpb.UserId{
							OpaqueId: "owner",
						},
					},
					Name:      "project",
					SpaceType: spaces.SpaceTypeProject.AsString(),
					RootInfo: &provider.ResourceInfo{
						Path:          "/path/to/project",
						PermissionSet: permissions.NewManagerRole().CS3ResourcePermissions(),
					},
					PermissionSet: permissions.NewManagerRole().CS3ResourcePermissions(),
				},
			},
		},
		{
			description: "user part of the readers group",
			projects: []*Project{
				{
					StorageID: "storage_id",
					SpaceID:   spaceID,
					Path:      "/path/to/project",
					Name:      "project",
					Owner:     "unknown",
					Readers:   "project-readers",
					Writers:   "project-writers",
					Admins:    "project-admins",
				},
			},
			user: &userpb.User{Id: &userpb.UserId{OpaqueId: "owner", Idp: "idp"}, Groups: []string{"project-readers"}},
			expected: []*provider.StorageSpace{
				{
					Id: &provider.StorageSpaceId{
						OpaqueId: spaces.EncodeStorageSpaceID("storage_id", spaceID),
					},
					Owner: &userpb.User{
						Id: &userpb.UserId{
							OpaqueId: "unknown",
						},
					},
					Name:      "project",
					SpaceType: spaces.SpaceTypeProject.AsString(),
					RootInfo: &provider.ResourceInfo{
						Path:          "/path/to/project",
						PermissionSet: permissions.NewViewerRole().CS3ResourcePermissions(),
					},
					PermissionSet: permissions.NewViewerRole().CS3ResourcePermissions(),
				},
			},
		},
		{
			description: "user part of the writers group",
			projects: []*Project{
				{
					StorageID: "storage_id",
					SpaceID:   spaceID,
					Path:      "/path/to/project",
					Name:      "project",
					Owner:     "unknown",
					Readers:   "project-readers",
					Writers:   "project-writers",
					Admins:    "project-admins",
				},
			},
			user: &userpb.User{Id: &userpb.UserId{OpaqueId: "owner", Idp: "idp"}, Groups: []string{"project-writers"}},
			expected: []*provider.StorageSpace{
				{
					Id: &provider.StorageSpaceId{
						OpaqueId: spaces.EncodeStorageSpaceID("storage_id", spaceID),
					},
					Owner: &userpb.User{
						Id: &userpb.UserId{
							OpaqueId: "unknown",
						},
					},
					Name:      "project",
					SpaceType: spaces.SpaceTypeProject.AsString(),
					RootInfo: &provider.ResourceInfo{
						Path:          "/path/to/project",
						PermissionSet: permissions.NewEditorRole().CS3ResourcePermissions(),
					},
					PermissionSet: permissions.NewEditorRole().CS3ResourcePermissions(),
				},
			},
		},
		{
			description: "user part of the admins group",
			projects: []*Project{
				{
					StorageID: "storage_id",
					SpaceID:   spaceID,
					Path:      "/path/to/project",
					Name:      "project",
					Owner:     "unknown",
					Readers:   "project-readers",
					Writers:   "project-writers",
					Admins:    "project-admins",
				},
			},
			user: &userpb.User{Id: &userpb.UserId{OpaqueId: "owner", Idp: "idp"}, Groups: []string{"project-admins"}},
			expected: []*provider.StorageSpace{
				{
					Id: &provider.StorageSpaceId{
						OpaqueId: spaces.EncodeStorageSpaceID("storage_id", spaceID),
					},
					Owner: &userpb.User{
						Id: &userpb.UserId{
							OpaqueId: "unknown",
						},
					},
					Name:      "project",
					SpaceType: spaces.SpaceTypeProject.AsString(),
					RootInfo: &provider.ResourceInfo{
						Path:          "/path/to/project",
						PermissionSet: permissions.NewManagerRole().CS3ResourcePermissions(),
					},
					PermissionSet: permissions.NewManagerRole().CS3ResourcePermissions(),
				},
			},
		},
		{
			description: "user part of the admins and readers group",
			projects: []*Project{
				{
					StorageID: "storage_id",
					SpaceID:   spaceID,
					Path:      "/path/to/project",
					Name:      "project",
					Owner:     "unknown",
					Readers:   "project-readers",
					Writers:   "project-writers",
					Admins:    "project-admins",
				},
			},
			user: &userpb.User{Id: &userpb.UserId{OpaqueId: "owner", Idp: "idp"}, Groups: []string{"project-readers", "project-admins"}},
			expected: []*provider.StorageSpace{
				{
					Id: &provider.StorageSpaceId{
						OpaqueId: spaces.EncodeStorageSpaceID("storage_id", spaceID),
					},
					Owner: &userpb.User{
						Id: &userpb.UserId{
							OpaqueId: "unknown",
						},
					},
					Name:      "project",
					SpaceType: spaces.SpaceTypeProject.AsString(),
					RootInfo: &provider.ResourceInfo{
						Path:          "/path/to/project",
						PermissionSet: permissions.NewManagerRole().CS3ResourcePermissions(),
					},
					PermissionSet: permissions.NewManagerRole().CS3ResourcePermissions(),
				},
			},
		},
		{
			description: "user is neither the owner nor part of the projects' groups",
			projects: []*Project{
				{
					StorageID: "storage_id",
					SpaceID:   spaceID,
					Path:      "/path/to/project",
					Name:      "project",
					Owner:     "unknown",
					Readers:   "project-readers",
					Writers:   "project-writers",
					Admins:    "project-admins",
				},
			},
			user:     &userpb.User{Id: &userpb.UserId{OpaqueId: "owner", Idp: "idp"}, Groups: []string{"something-readers"}},
			expected: []*provider.StorageSpace{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			ctx := context.Background()
			catalogue, err, teardown := setupSuite(t)

			if err != nil {
				t.Error(err)
			}

			if err != nil {
				t.Fatalf("not expected error while creating projects driver: %+v", err)
			}

			defer func() {
				if err := teardown(t); err != nil {
					t.Fatalf("failed to teardown test suite: %+v", err)
				}
			}()

			catmgr := catalogue.(*ProjectsManager)
			for _, proj := range tt.projects {
				catmgr.db.Create(&proj)
			}

			ctx = appctx.ContextSetUser(ctx, tt.user)
			got, err := catalogue.ListStorageSpaces(ctx, &provider.ListStorageSpacesRequest{}, projects_catalogue.ProjectStatusActive)
			if err != nil {
				t.Fatalf("not expected error while listing projects: %+v", err)
			}

			if !reflect.DeepEqual(got.StorageSpaces, tt.expected) {
				t.Fatalf("projects' list do not match. got=%+v expected=%+v", got.StorageSpaces, tt.expected)
			}
		})
	}
}

// TestCacheTTLUsesSeconds guards against a regression where the configured
// CacheTTL (documented in seconds) was passed to ttlcache without multiplying
// by time.Second. That made the cache TTL 60ns instead of 60s, which made the
// ttlcache expiration goroutine busy-loop and burn a full CPU core.
func TestCacheTTLUsesSeconds(t *testing.T) {
	ctx := context.Background()
	dbName := "ttl_test_db.sqlite"
	defer os.Remove(dbName)
	mgr, err := New(ctx, map[string]any{
		"db_engine": "sqlite",
		"db_name":   dbName,
		"cache_ttl": 60,
	})
	if err != nil {
		t.Fatalf("unexpected error creating manager: %v", err)
	}
	catmgr := mgr.(*ProjectsManager)

	// A fresh item must report a remaining TTL on the order of seconds, not
	// nanoseconds, otherwise the expiration loop never sleeps.
	if err := catmgr.cache.Set("k", "v"); err != nil {
		t.Fatalf("unexpected error setting cache item: %v", err)
	}
	_, remaining, err := catmgr.cache.GetWithTTL("k")
	if err != nil {
		t.Fatalf("unexpected error getting cache item: %v", err)
	}
	if remaining < 30*time.Second {
		t.Fatalf("remaining TTL = %v, want ~60s (a sub-second TTL makes the cache spin)", remaining)
	}
}
