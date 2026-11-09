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

// This package implements the APIs defined in https://owncloud.dev/apis/http/graph/

package ocgraph

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"

	gateway "github.com/cs3org/go-cs3apis/cs3/gateway/v1beta1"
	rpcv1beta1 "github.com/cs3org/go-cs3apis/cs3/rpc/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	"github.com/cs3org/reva/v3/pkg/appctx"
	"github.com/cs3org/reva/v3/pkg/spaces"
	"github.com/go-chi/chi/v5"
	"github.com/pkg/errors"
)

// storageSpaceByID looks a single storage space up by its (encoded) id.
func (s *svc) storageSpaceByID(ctx context.Context, gw gateway.GatewayAPIClient, spaceID string) (*provider.StorageSpace, error) {
	listRes, err := gw.ListStorageSpaces(ctx, &provider.ListStorageSpacesRequest{
		Filters: []*provider.ListStorageSpacesRequest_Filter{
			{
				Type: provider.ListStorageSpacesRequest_Filter_TYPE_ID,
				Term: &provider.ListStorageSpacesRequest_Filter_Id{
					Id: &provider.StorageSpaceId{OpaqueId: spaceID},
				},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	if listRes.Status == nil || listRes.Status.Code != rpcv1beta1.Code_CODE_OK {
		return nil, errors.New("failed to fetch storage space " + spaceID)
	}
	if len(listRes.StorageSpaces) != 1 {
		return nil, errors.New("space not found: " + spaceID)
	}
	return listRes.StorageSpaces[0], nil
}

// referenceForItemID turns a Graph item id into a CS3 reference. Item ids
// returned by this service are encoded resource ids (<storage>$<space>!<item>),
// so those are used directly. Any other id is treated as a path relative to the
// space root, which keeps the endpoint usable with plain paths too.
func (s *svc) referenceForItemID(ctx context.Context, gw gateway.GatewayAPIClient, spaceID, itemID string) (*provider.Reference, error) {
	if rid, ok := spaces.ParseResourceID(itemID); ok {
		return &provider.Reference{ResourceId: rid}, nil
	}
	space, err := s.storageSpaceByID(ctx, gw, spaceID)
	if err != nil {
		return nil, err
	}
	if space.RootInfo == nil || space.RootInfo.Path == "" {
		return nil, errors.New("space has no root path")
	}
	return &provider.Reference{Path: path.Join(space.RootInfo.Path, itemID)}, nil
}

// getDriveItem answers GET /v1beta1/drives/{space-id}/items/{resource-id}. It is
// the read counterpart of the existing PATCH/invite routes and lets a client
// fetch one item's metadata, including the last editor
// (lastModifiedBy / oc:last-modified-by), without a full PROPFIND.
func (s *svc) getDriveItem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := appctx.GetLogger(ctx)

	gw, err := s.getClient()
	if err != nil {
		log.Error().Err(err).Msg("error getting grpc client")
		handleError(ctx, err, w)
		return
	}

	spaceID := chi.URLParam(r, "space-id")
	resourceID := chi.URLParam(r, "resource-id")
	if resourceID == "" {
		handleBadRequest(ctx, errors.New("missing resource id"), w)
		return
	}

	ref, err := s.referenceForItemID(ctx, gw, spaceID, resourceID)
	if err != nil {
		handleBadRequest(ctx, err, w)
		return
	}

	stat, err := gw.Stat(ctx, &provider.StatRequest{
		Ref:                   ref,
		ArbitraryMetadataKeys: lastModifiedByMetadataKeys,
	})
	if err != nil {
		log.Error().Err(err).Msg("error statting drive item")
		handleError(ctx, err, w)
		return
	}
	if stat.Status == nil || stat.Status.Code != rpcv1beta1.Code_CODE_OK {
		switch stat.Status.GetCode() {
		case rpcv1beta1.Code_CODE_NOT_FOUND:
			handleCustomError(ctx, fmt.Errorf("item not found: %s", resourceID), http.StatusNotFound, w)
		case rpcv1beta1.Code_CODE_PERMISSION_DENIED:
			handleCustomError(ctx, fmt.Errorf("permission denied: %s", resourceID), http.StatusForbidden, w)
		default:
			handleRpcStatus(ctx, stat.Status, fmt.Sprintf("ocgraph: failed to stat item %s", resourceID), w)
		}
		return
	}

	item := s.ResourceInfoToDriveItem(stat.Info, "")
	item.LastModifiedBy = lastModifiedByFromMetadata(stat.Info.ArbitraryMetadata)

	if err := json.NewEncoder(w).Encode(item); err != nil {
		log.Error().Err(err).Msg("error marshalling drive item as json")
	}
}
