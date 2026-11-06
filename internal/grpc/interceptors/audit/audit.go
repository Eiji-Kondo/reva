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

// Package audit emits one structured log line per mutating storage request.
//
// It answers "who did what, when" - a question the storage layer cannot, since
// localfs only keeps mtime and etag. Every write the gateway forwards is logged
// with the acting user, the method and the resource reference, so the stream can
// be shipped to a SIEM or read straight from the revad log. Unlike the
// last-modified-by metadata (the current value of a single field), this is an
// append-only history: it keeps every event, not just the latest one.
//
// Reads are ignored, so the volume stays proportional to writes.
package audit

import (
	"context"

	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	"github.com/cs3org/reva/v3/pkg/appctx"
	"github.com/cs3org/reva/v3/pkg/rgrpc"
	"google.golang.org/grpc"
)

const (
	defaultPriority = 250
)

func init() {
	rgrpc.RegisterUnaryInterceptor("audit", NewUnary)
}

// NewUnary returns a new unary interceptor that logs mutating storage calls.
func NewUnary(_ map[string]any) (grpc.UnaryServerInterceptor, int, error) {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		res, err := handler(ctx, req)
		if action, ref := describe(req); action != "" {
			event := appctx.GetLogger(ctx).Info()
			event.Str("action", action).
				Str("uri", info.FullMethod).
				Bool("ok", err == nil)
			if ref != nil {
				if ref.GetPath() != "" {
					event.Str("path", ref.GetPath())
				}
				if id := ref.GetResourceId(); id != nil {
					event.Str("storage_id", id.GetStorageId())
					event.Str("opaque_id", id.GetOpaqueId())
					event.Str("space_id", id.GetSpaceId())
				}
			}
			if u, ok := appctx.ContextGetUser(ctx); ok && u != nil {
				event.Str("user", u.GetUsername())
				if id := u.GetId(); id != nil {
					event.Str("user_id", id.GetOpaqueId()).
						Str("idp", id.GetIdp())
				}
			}
			event.Msg("audit: storage write")
		}
		return res, err
	}, defaultPriority, nil
}

// describe maps a request to an action name and the resource it targets. An
// empty action means the request is a read (or not a storage write) and is not
// audited.
func describe(req any) (string, *provider.Reference) {
	switch r := req.(type) {
	case *provider.InitiateFileUploadRequest:
		return "upload", r.GetRef()
	case *provider.CreateContainerRequest:
		return "create-container", r.GetRef()
	case *provider.TouchFileRequest:
		return "touch-file", r.GetRef()
	case *provider.MoveRequest:
		return "move", r.GetSource()
	case *provider.DeleteRequest:
		return "delete", r.GetRef()
	case *provider.SetArbitraryMetadataRequest:
		return "set-metadata", r.GetRef()
	case *provider.UnsetArbitraryMetadataRequest:
		return "unset-metadata", r.GetRef()
	case *provider.RestoreFileVersionRequest:
		return "restore-version", r.GetRef()
	case *provider.RestoreRecycleItemRequest:
		return "restore-recycle", r.GetRef()
	case *provider.PurgeRecycleRequest:
		return "purge-recycle", r.GetRef()
	case *provider.CreateHomeRequest:
		return "create-home", nil
	}
	return "", nil
}
