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

package ocdav

import (
	"net/http"

	rpc "github.com/cs3org/go-cs3apis/cs3/rpc/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	"github.com/cs3org/reva/v3/pkg/appctx"
)

func (s *svc) handleUnlock(w http.ResponseWriter, r *http.Request, ns string) {
	ctx := r.Context()
	lockID, ok := singleLockToken(r.Header.Get("Lock-Token"))
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	user, ok := appctx.ContextGetUser(ctx)
	if !ok || user.GetId() == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	client, err := s.getClient()
	if err != nil {
		appctx.GetLogger(ctx).Error().Err(err).Msg("error getting gateway client for UNLOCK")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	ref := &provider.Reference{Path: requestReferencePath(ns, r.URL.Path)}
	res, err := client.GetLock(ctx, &provider.GetLockRequest{Ref: ref})
	if err != nil {
		appctx.GetLogger(ctx).Error().Err(err).Msg("error retrieving WebDAV lock for UNLOCK")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if res.GetStatus().GetCode() != rpc.Code_CODE_OK {
		writeLockRPCError(w, res.GetStatus())
		return
	}
	locks, err := webDAVLocks(res.GetLock())
	if err != nil {
		appctx.GetLogger(ctx).Error().Err(err).Msg("error decoding WebDAV lock set")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	currentLock := findWebDAVLock(locks, lockID)
	if currentLock == nil {
		w.WriteHeader(http.StatusConflict)
		return
	}
	unlockRes, err := client.Unlock(ctx, &provider.UnlockRequest{
		Ref: ref,
		Lock: &provider.Lock{
			LockId:  lockID,
			Type:    currentLock.GetType(),
			User:    user.GetId(),
			AppName: currentLock.GetAppName(),
		},
	})
	if err != nil {
		appctx.GetLogger(ctx).Error().Err(err).Msg("error unlocking WebDAV resource")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if unlockRes.GetStatus().GetCode() != rpc.Code_CODE_OK {
		writeLockRPCError(w, unlockRes.GetStatus())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
