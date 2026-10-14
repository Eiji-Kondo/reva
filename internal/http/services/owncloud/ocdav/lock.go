// Copyright 2018-2024 CERN
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// In applying this license, CERN does not waive the privileges and immunities
// granted to it by virtue of its status as an Intergovernmental Organization
// or submit itself to any jurisdiction.

package ocdav

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	gateway "github.com/cs3org/go-cs3apis/cs3/gateway/v1beta1"
	rpc "github.com/cs3org/go-cs3apis/cs3/rpc/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	typespb "github.com/cs3org/go-cs3apis/cs3/types/v1beta1"
	"github.com/cs3org/reva/v3/pkg/appctx"
	"github.com/google/uuid"
)

const (
	defaultLockTimeout = time.Hour
	maxLockTimeout     = 7 * 24 * time.Hour
	webDAVLockSetKey   = "reva-localfs-lock-set"
)

type davLockInfo struct {
	XMLName   xml.Name `xml:"lockinfo"`
	LockScope struct {
		Exclusive *struct{} `xml:"exclusive"`
		Shared    *struct{} `xml:"shared"`
	} `xml:"lockscope"`
	LockType struct {
		Write *struct{} `xml:"write"`
	} `xml:"locktype"`
}

func (s *svc) handleLock(w http.ResponseWriter, r *http.Request, ns string) {
	ctx := r.Context()
	client, err := s.getClient()
	if err != nil {
		appctx.GetLogger(ctx).Error().Err(err).Msg("error getting gateway client for LOCK")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	ref := &provider.Reference{Path: requestReferencePath(ns, r.URL.Path)}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		s.refreshWebDAVLock(w, r, client, ref)
		return
	}

	var lockInfo davLockInfo
	if err := xml.Unmarshal(body, &lockInfo); err != nil ||
		lockInfo.XMLName.Local != "lockinfo" || lockInfo.XMLName.Space != "DAV:" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if (lockInfo.LockScope.Exclusive == nil && lockInfo.LockScope.Shared == nil) ||
		(lockInfo.LockScope.Exclusive != nil && lockInfo.LockScope.Shared != nil) ||
		lockInfo.LockType.Write == nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	depth := strings.TrimSpace(r.Header.Get(HeaderDepth))
	if depth == "" {
		depth = "0"
	}
	if depth == "infinity" {
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	if depth != "0" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	timeout, ok := parseLockTimeout(r.Header.Get("Timeout"))
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	user, ok := appctx.ContextGetUser(ctx)
	if !ok || user.GetId() == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	expiration := time.Now().Add(timeout)
	lock := &provider.Lock{
		LockId:     "opaquelocktoken:" + uuid.NewString(),
		Type:       provider.LockType_LOCK_TYPE_EXCL,
		User:       user.GetId(),
		Expiration: &typespb.Timestamp{Seconds: uint64(expiration.Unix()), Nanos: uint32(expiration.Nanosecond())},
	}
	if lockInfo.LockScope.Shared != nil {
		lock.Type = provider.LockType_LOCK_TYPE_SHARED
	}
	res, err := client.SetLock(ctx, &provider.SetLockRequest{Ref: ref, Lock: lock})
	if err != nil {
		appctx.GetLogger(ctx).Error().Err(err).Msg("error setting WebDAV lock")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if res.GetStatus().GetCode() != rpc.Code_CODE_OK {
		writeLockRPCError(w, res.GetStatus())
		return
	}
	writeLockResponse(w, r, lock, timeout)
}

func (s *svc) refreshWebDAVLock(w http.ResponseWriter, r *http.Request, client gateway.GatewayAPIClient, ref *provider.Reference) {
	lockID, ok := singleLockToken(r.Header.Get("If"))
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	res, err := client.GetLock(ctx, &provider.GetLockRequest{Ref: ref})
	if err != nil {
		appctx.GetLogger(ctx).Error().Err(err).Msg("error retrieving WebDAV lock for refresh")
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
	timeout, ok := parseLockTimeout(r.Header.Get("Timeout"))
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	user, ok := appctx.ContextGetUser(ctx)
	if !ok || user.GetId() == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	expiration := time.Now().Add(timeout)
	lock := &provider.Lock{
		LockId:     lockID,
		Type:       currentLock.GetType(),
		User:       user.GetId(),
		AppName:    currentLock.GetAppName(),
		Expiration: &typespb.Timestamp{Seconds: uint64(expiration.Unix()), Nanos: uint32(expiration.Nanosecond())},
	}
	refreshRes, err := client.RefreshLock(ctx, &provider.RefreshLockRequest{
		Ref: ref, Lock: lock, ExistingLockId: lockID,
	})
	if err != nil {
		appctx.GetLogger(ctx).Error().Err(err).Msg("error refreshing WebDAV lock")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if refreshRes.GetStatus().GetCode() != rpc.Code_CODE_OK {
		writeLockRPCError(w, refreshRes.GetStatus())
		return
	}
	writeLockResponse(w, r, lock, timeout)
}

func parseLockTimeout(value string) (time.Duration, bool) {
	if value == "" {
		return defaultLockTimeout, true
	}
	for _, candidate := range strings.Split(value, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "Infinite" {
			continue
		}
		secondsText, ok := strings.CutPrefix(candidate, "Second-")
		if !ok {
			continue
		}
		seconds, err := strconv.ParseInt(secondsText, 10, 64)
		if err != nil || seconds <= 0 {
			return 0, false
		}
		timeout := time.Duration(seconds) * time.Second
		if timeout > maxLockTimeout {
			timeout = maxLockTimeout
		}
		return timeout, true
	}
	return 0, false
}

func writeLockResponse(w http.ResponseWriter, r *http.Request, lock *provider.Lock, timeout time.Duration) {
	token := xmlEscape(lock.GetLockId())
	rootPath := r.URL.EscapedPath()
	if incomingPath, ok := r.Context().Value(ctxKeyIncomingURL).(string); ok {
		rootPath = (&url.URL{Path: incomingPath}).EscapedPath()
	}
	root := xmlEscape(rootPath)
	lockScope := "exclusive"
	if lock.GetType() == provider.LockType_LOCK_TYPE_SHARED {
		lockScope = "shared"
	}
	body := `<?xml version="1.0" encoding="utf-8"?>
<d:prop xmlns:d="DAV:"><d:lockdiscovery><d:activelock>
<d:locktype><d:write/></d:locktype><d:lockscope><d:` + lockScope + `/></d:lockscope>
<d:depth>0</d:depth><d:timeout>Second-` + strconv.FormatInt(int64(timeout/time.Second), 10) + `</d:timeout>
<d:locktoken><d:href>` + token + `</d:href></d:locktoken>
<d:lockroot><d:href>` + root + `</d:href></d:lockroot>
</d:activelock></d:lockdiscovery></d:prop>`
	w.Header().Set(HeaderContentType, "application/xml; charset=utf-8")
	w.Header().Set("Lock-Token", "<"+lock.GetLockId()+">")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, body); err != nil {
		appctx.GetLogger(r.Context()).Error().Err(err).Msg("error writing WebDAV LOCK response")
	}
}

func webDAVLocks(lock *provider.Lock) ([]*provider.Lock, error) {
	if lock == nil {
		return nil, nil
	}
	if lock.GetOpaque() == nil {
		return []*provider.Lock{lock}, nil
	}
	entry := lock.GetOpaque().GetMap()[webDAVLockSetKey]
	if entry == nil || len(entry.GetValue()) == 0 {
		return []*provider.Lock{lock}, nil
	}
	var locks []*provider.Lock
	if err := json.Unmarshal(entry.GetValue(), &locks); err != nil {
		return nil, err
	}
	return locks, nil
}

func findWebDAVLock(locks []*provider.Lock, lockID string) *provider.Lock {
	for _, lock := range locks {
		if lock.GetLockId() == lockID {
			return lock
		}
	}
	return nil
}

func xmlEscape(value string) string {
	var escaped bytes.Buffer
	if err := xml.EscapeText(&escaped, []byte(value)); err != nil {
		return ""
	}
	return escaped.String()
}

func writeLockRPCError(w http.ResponseWriter, status *rpc.Status) {
	switch status.GetCode() {
	case rpc.Code_CODE_NOT_FOUND:
		w.WriteHeader(http.StatusNotFound)
	case rpc.Code_CODE_PERMISSION_DENIED:
		w.WriteHeader(http.StatusForbidden)
	case rpc.Code_CODE_FAILED_PRECONDITION:
		w.WriteHeader(http.StatusLocked)
	case rpc.Code_CODE_UNIMPLEMENTED:
		w.WriteHeader(http.StatusNotImplemented)
	default:
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func requestReferencePath(namespace, requestPath string) string {
	return path.Join(namespace, requestPath)
}

func singleLockToken(value string) (string, bool) {
	tokens := lockTokens(value)
	if len(tokens) != 1 {
		return "", false
	}
	return tokens[0], true
}

func lockTokens(value string) []string {
	var tokens []string
	for {
		start := strings.IndexByte(value, '<')
		if start < 0 {
			break
		}
		value = value[start+1:]
		end := strings.IndexByte(value, '>')
		if end < 0 {
			break
		}
		token := value[:end]
		value = value[end+1:]
		if strings.HasPrefix(token, "opaquelocktoken:") {
			tokens = append(tokens, token)
		}
	}
	return tokens
}
