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
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"

	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	"github.com/cs3org/reva/v3/pkg/appctx"
	"github.com/cs3org/reva/v3/pkg/errtypes"
	"github.com/cs3org/reva/v3/pkg/storage/utils/grants"
	"github.com/cs3org/reva/v3/pkg/utils"
	"github.com/pkg/errors"
)

const foreignResourcePathPrefix = "/.reva-shared"

func (fs *localfs) sharedResourcePath(ctx context.Context, id *provider.ResourceId) (string, error) {
	if id == nil || !strings.HasPrefix(id.GetOpaqueId(), "fileid-") {
		return "", errtypes.NotFound("localfs: invalid resource id")
	}

	decoded, err := url.QueryUnescape(strings.TrimPrefix(id.GetOpaqueId(), "fileid-"))
	if err != nil {
		return "", errors.Wrap(err, "localfs: error decoding resource id")
	}
	if decoded == "" || path.Clean(decoded) != decoded || strings.HasPrefix(decoded, "../") {
		return "", errtypes.PermissionDenied("localfs: invalid resource path in resource id")
	}

	var currentLayout string
	if !fs.conf.DisableHome {
		currentLayout, err = fs.GetHome(ctx)
		if err != nil {
			return "", err
		}
	}
	if currentLayout == "" || decoded == currentLayout || strings.HasPrefix(decoded, currentLayout+"/") {
		return path.Join("/", strings.TrimPrefix(strings.TrimPrefix(decoded, currentLayout), "/")), nil
	}

	physicalPath := path.Join(fs.conf.DataDirectory, decoded)
	dataRoot := path.Clean(fs.conf.DataDirectory)
	if physicalPath == dataRoot || !strings.HasPrefix(physicalPath, dataRoot+"/") {
		return "", errtypes.PermissionDenied("localfs: resource id is outside the data directory")
	}
	if _, statErr := os.Stat(physicalPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return "", errtypes.NotFound("localfs: shared resource not found")
		}
		return "", errors.Wrap(statErr, "localfs: error stating shared resource")
	}

	permissions, permissionErr := fs.inheritedGrantedPermissions(ctx, physicalPath)
	if permissionErr != nil {
		return "", permissionErr
	}
	if permissions.GetStat() || permissions.GetInitiateFileDownload() {
		return path.Join(foreignResourcePathPrefix, decoded), nil
	}
	return "", errtypes.PermissionDenied("localfs: share grant does not allow reading the resource")
}

func (fs *localfs) resolveShareReference(ctx context.Context, sharePath string, write bool) (string, error) {
	referencePath := fs.wrapReferences(ctx, sharePath)
	target, err := fs.getReferenceEntry(ctx, referencePath)
	if err != nil {
		return "", errors.Wrap(err, "localfs: error getting share reference target")
	}

	targetURI, err := url.Parse(target)
	if err != nil {
		return "", errors.Wrap(err, "localfs: error parsing share reference target")
	}
	if targetURI.Scheme != "cs3" {
		return "", errtypes.NotSupported("localfs: only CS3 share references can be accessed by this storage driver")
	}
	storageID, opaqueID, ok := strings.Cut(targetURI.Opaque, "/")
	if !ok || storageID != "localfs" || opaqueID == "" {
		return "", errtypes.NotSupported("localfs: share reference targets a different storage provider")
	}

	foreignPath, err := fs.sharedResourcePath(ctx, &provider.ResourceId{OpaqueId: opaqueID})
	if err != nil {
		return "", err
	}
	physicalPath := path.Join(
		fs.conf.DataDirectory,
		strings.TrimPrefix(foreignPath, foreignResourcePathPrefix+"/"),
	)
	permissions, err := fs.inheritedGrantedPermissions(ctx, physicalPath)
	if err != nil {
		return "", err
	}
	if write && !permissions.GetInitiateFileUpload() {
		return "", errtypes.PermissionDenied("localfs: share grant does not allow writing to the resource")
	}
	if !write && !permissions.GetInitiateFileDownload() {
		return "", errtypes.PermissionDenied("localfs: share grant does not allow reading the resource")
	}
	return foreignPath, nil
}

func (fs *localfs) isForeignDataPath(ctx context.Context, resourcePath string) (bool, error) {
	if fs.conf.DisableHome || resourcePath == "" {
		return false, nil
	}
	resourcePath = path.Clean(resourcePath)
	dataRoot := path.Clean(fs.conf.DataDirectory)
	if resourcePath == dataRoot || !strings.HasPrefix(resourcePath, dataRoot+"/") {
		return false, nil
	}
	layout, err := fs.GetHome(ctx)
	if err != nil {
		return false, err
	}
	userRoot := path.Join(dataRoot, layout)
	return resourcePath != userRoot && !strings.HasPrefix(resourcePath, userRoot+"/"), nil
}

func (fs *localfs) grantedPermissions(ctx context.Context, resourcePath string) (*provider.ResourcePermissions, error) {
	return fs.inheritedGrantedPermissions(ctx, resourcePath)
}

func (fs *localfs) inheritedGrantedPermissions(ctx context.Context, resourcePath string) (*provider.ResourcePermissions, error) {
	dataRoot := path.Clean(fs.conf.DataDirectory)
	resourcePath = path.Clean(resourcePath)
	if !strings.HasPrefix(resourcePath, dataRoot+"/") {
		return nil, errtypes.PermissionDenied("localfs: resource is outside the data directory")
	}
	for candidate := resourcePath; candidate != dataRoot; candidate = path.Dir(candidate) {
		permissions, found, err := fs.permissionsForGrantee(ctx, candidate)
		if err != nil {
			return nil, err
		}
		if found {
			return permissions, nil
		}
	}
	return nil, errtypes.PermissionDenied("localfs: no share grant for resource")
}

func (fs *localfs) permissionsForGrantee(ctx context.Context, resourcePath string) (*provider.ResourcePermissions, bool, error) {
	user, ok := appctx.ContextGetUser(ctx)
	if !ok || user.GetId() == nil {
		return &provider.ResourcePermissions{}, false, nil
	}

	rows, err := fs.getACLs(ctx, resourcePath)
	if err != nil {
		return nil, false, errors.Wrap(err, "localfs: error reading share grants")
	}
	defer rows.Close()

	userACLType, err := grants.GetACLType(provider.GranteeType_GRANTEE_TYPE_USER)
	if err != nil {
		return nil, false, errors.Wrap(err, "localfs: error resolving user ACL type")
	}
	grantees := map[string]struct{}{
		fmt.Sprintf("%s:%s:%s@%s", userACLType, user.Id.GetOpaqueId(), utils.UserTypeToString(user.Id.GetType()), user.Id.GetIdp()): {},
	}
	groupACLType, err := grants.GetACLType(provider.GranteeType_GRANTEE_TYPE_GROUP)
	if err != nil {
		return nil, false, errors.Wrap(err, "localfs: error resolving group ACL type")
	}
	for _, group := range user.GetGroups() {
		grantees[fmt.Sprintf("%s::%s@%s", groupACLType, group, user.Id.GetIdp())] = struct{}{}
	}
	result := &provider.ResourcePermissions{}
	var grantee, role string
	found := false
	for rows.Next() {
		if err := rows.Scan(&grantee, &role); err != nil {
			return nil, false, errors.Wrap(err, "localfs: error scanning share grants")
		}
		if _, ok := grantees[grantee]; !ok {
			continue
		}
		if role == "" {
			continue
		}
		mergeResourcePermissions(result, grants.GetGrantPermissionSet(role))
		found = true
	}
	if err := rows.Err(); err != nil {
		return nil, false, errors.Wrap(err, "localfs: error iterating share grants")
	}
	return result, found, nil
}

func mergeResourcePermissions(dst, src *provider.ResourcePermissions) {
	dst.AddGrant = dst.AddGrant || src.GetAddGrant()
	dst.CreateContainer = dst.CreateContainer || src.GetCreateContainer()
	dst.Delete = dst.Delete || src.GetDelete()
	dst.GetPath = dst.GetPath || src.GetGetPath()
	dst.GetQuota = dst.GetQuota || src.GetGetQuota()
	dst.InitiateFileDownload = dst.InitiateFileDownload || src.GetInitiateFileDownload()
	dst.InitiateFileUpload = dst.InitiateFileUpload || src.GetInitiateFileUpload()
	dst.ListContainer = dst.ListContainer || src.GetListContainer()
	dst.ListFileVersions = dst.ListFileVersions || src.GetListFileVersions()
	dst.ListGrants = dst.ListGrants || src.GetListGrants()
	dst.ListRecycle = dst.ListRecycle || src.GetListRecycle()
	dst.Move = dst.Move || src.GetMove()
	dst.PurgeRecycle = dst.PurgeRecycle || src.GetPurgeRecycle()
	dst.RemoveGrant = dst.RemoveGrant || src.GetRemoveGrant()
	dst.RestoreFileVersion = dst.RestoreFileVersion || src.GetRestoreFileVersion()
	dst.RestoreRecycleItem = dst.RestoreRecycleItem || src.GetRestoreRecycleItem()
	dst.Stat = dst.Stat || src.GetStat()
	dst.UpdateGrant = dst.UpdateGrant || src.GetUpdateGrant()
}
