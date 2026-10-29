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

package oidc

import (
	"context"
	"net/http"
	"testing"

	"golang.org/x/oauth2"
)

// go-oidc only honours a *http.Client stored under oauth2.HTTPClient (see
// getClient in coreos/go-oidc). Storing the wrapper instead silently drops the
// custom transport, so "insecure = true" stops working and the discovery/JWKS
// requests fall back to the system trust store. Guard that regression.
func TestGetOAuthCtxProvidesNativeHTTPClient(t *testing.T) {
	for _, insecure := range []bool{false, true} {
		m := &mgr{c: &config{Insecure: insecure}}
		ctx := m.getOAuthCtx(context.Background())

		client, ok := ctx.Value(oauth2.HTTPClient).(*http.Client)
		if !ok {
			t.Fatalf("insecure=%v: oauth2.HTTPClient holds %T, not *http.Client; go-oidc will ignore it",
				insecure, ctx.Value(oauth2.HTTPClient))
		}
		if client == nil {
			t.Fatalf("insecure=%v: oauth2.HTTPClient is a nil *http.Client", insecure)
		}
	}
}
