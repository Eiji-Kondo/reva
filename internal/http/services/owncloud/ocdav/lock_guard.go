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
	"net/http"
	"net/url"
	"path"
	"strings"

	rpc "github.com/cs3org/go-cs3apis/cs3/rpc/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	"github.com/cs3org/reva/v3/pkg/appctx"
)

func (s *svc) enforceDAVLock(w http.ResponseWriter, r *http.Request, namespace string) bool {
	type lockCheck struct {
		reference string
		davPath   string
	}
	var checks []lockCheck
	requestPath := requestReferencePath(namespace, r.URL.Path)
	requestDAVPath := r.URL.Path
	if incomingPath, ok := r.Context().Value(ctxKeyIncomingURL).(string); ok {
		if parsed, err := url.ParseRequestURI(incomingPath); err == nil {
			requestDAVPath = parsed.Path
		}
	}
	switch r.Method {
	case http.MethodPut, http.MethodDelete, MethodMkcol, MethodProppatch:
		checks = append(checks, lockCheck{reference: requestPath, davPath: requestDAVPath})
	case MethodMove:
		checks = append(checks, lockCheck{reference: requestPath, davPath: requestDAVPath})
		fallthrough
	case MethodCopy:
		if destination, ok := s.webDAVDestinationPath(r, namespace); ok {
			destinationURI, err := url.ParseRequestURI(r.Header.Get(HeaderDestination))
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return false
			}
			checks = append(checks, lockCheck{reference: destination, davPath: destinationURI.Path})
		}
	default:
		return true
	}

	client, err := s.getClient()
	if err != nil {
		appctx.GetLogger(r.Context()).Error().Err(err).Msg("error getting gateway client while checking WebDAV lock")
		w.WriteHeader(http.StatusInternalServerError)
		return false
	}
	for _, check := range checks {
		res, err := client.GetLock(r.Context(), &provider.GetLockRequest{
			Ref: &provider.Reference{Path: check.reference},
		})
		if err != nil {
			appctx.GetLogger(r.Context()).Error().Err(err).Str("path", check.reference).Msg("error checking WebDAV lock")
			w.WriteHeader(http.StatusInternalServerError)
			return false
		}
		switch res.GetStatus().GetCode() {
		case rpc.Code_CODE_NOT_FOUND:
			continue
		case rpc.Code_CODE_OK:
			conditionLists, valid := lockTokensForDAVResource(r.Header.Get("If"), requestDAVPath, check.davPath)
			if !valid {
				w.WriteHeader(http.StatusBadRequest)
				return false
			}
			locks, err := webDAVLocks(res.GetLock())
			if err != nil {
				appctx.GetLogger(r.Context()).Error().Err(err).Msg("error decoding WebDAV lock set")
				w.WriteHeader(http.StatusInternalServerError)
				return false
			}
			authorized := false
			for _, tokens := range conditionLists {
				listAuthorized := true
				for _, lock := range locks {
					if !containsLockToken(tokens, lock.GetLockId()) {
						listAuthorized = false
						break
					}
				}
				if listAuthorized {
					authorized = true
					break
				}
			}
			if !authorized {
				w.WriteHeader(http.StatusLocked)
				return false
			}
		case rpc.Code_CODE_UNIMPLEMENTED:
			appctx.GetLogger(r.Context()).Warn().Str("path", check.reference).
				Msg("storage backend does not support locks; proceeding without lock enforcement")
		default:
			writeLockRPCError(w, res.GetStatus())
			return false
		}
	}
	return true
}

func lockTokensForDAVResource(ifHeader, requestPath, targetPath string) ([][]string, bool) {
	if ifHeader == "" {
		return nil, true
	}
	targetPath = path.Clean(targetPath)
	currentScope := path.Clean(requestPath)
	input := strings.TrimSpace(ifHeader)
	var conditionLists [][]string
	for len(input) > 0 {
		input = strings.TrimLeft(input, " \t")
		if input == "" {
			break
		}
		if input[0] == '<' {
			end := strings.IndexByte(input, '>')
			if end < 0 {
				return nil, false
			}
			tag, err := url.Parse(input[1:end])
			if err != nil || (tag.Scheme != "http" && tag.Scheme != "https" && !strings.HasPrefix(input[1:end], "/")) {
				return nil, false
			}
			currentScope = path.Clean(tag.Path)
			input = strings.TrimSpace(input[end+1:])
			continue
		}
		if input[0] != '(' {
			return nil, false
		}
		end := strings.IndexByte(input, ')')
		if end < 0 {
			return nil, false
		}
		if currentScope == targetPath {
			tokens, valid := davStateListLockTokens(input[1:end])
			if !valid {
				return nil, false
			}
			conditionLists = append(conditionLists, tokens)
		}
		input = strings.TrimSpace(input[end+1:])
	}
	return conditionLists, true
}

func davStateListLockTokens(stateList string) ([]string, bool) {
	var tokens []string
	negated := false
	for len(stateList) > 0 {
		stateList = strings.TrimLeft(stateList, " \t")
		if stateList == "" {
			break
		}
		if strings.HasPrefix(stateList, "Not") &&
			(len(stateList) == 3 || stateList[3] == ' ' || stateList[3] == '\t') {
			if negated {
				return nil, false
			}
			negated = true
			stateList = stateList[3:]
			continue
		}
		switch stateList[0] {
		case '<':
			end := strings.IndexByte(stateList, '>')
			if end < 0 {
				return nil, false
			}
			token := stateList[1:end]
			if !negated && strings.HasPrefix(token, "opaquelocktoken:") {
				tokens = append(tokens, token)
			}
			stateList = stateList[end+1:]
		case '[':
			end := strings.IndexByte(stateList, ']')
			if end < 0 {
				return nil, false
			}
			stateList = stateList[end+1:]
		default:
			return nil, false
		}
		negated = false
	}
	if negated {
		return nil, false
	}
	return tokens, true
}

func containsLockToken(tokens []string, lockID string) bool {
	for _, token := range tokens {
		if token == lockID {
			return true
		}
	}
	return false
}

func (s *svc) webDAVDestinationPath(r *http.Request, namespace string) (string, bool) {
	if _, ok := r.Context().Value(ctxKeyBaseURI).(string); ok {
		destination, err := extractDestination(r, namespace)
		if err == nil {
			return destination, true
		}
		return "", false
	}
	destination, err := url.ParseRequestURI(r.Header.Get(HeaderDestination))
	if err != nil {
		return "", false
	}
	destinationPath := destination.Path
	if destinationPath == "" {
		return "", false
	}
	return path.Join(namespace, strings.TrimPrefix(destinationPath, "/")), true
}
