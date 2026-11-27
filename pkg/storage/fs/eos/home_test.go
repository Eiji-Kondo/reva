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

package eos

import (
	"context"
	"testing"

	userpb "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	"github.com/cs3org/reva/v3/pkg/appctx"
)

func testFS(enableHome bool) *Eosfs {
	return &Eosfs{conf: &Config{
		Namespace:  "/eos/user/",
		UserLayout: "{{substr 0 1 .Username}}/{{.Username}}",
		EnableHome: enableHome,
	}}
}

func userCtx(username string) context.Context {
	return appctx.ContextSetUser(context.Background(), &userpb.User{
		Id:       &userpb.UserId{OpaqueId: username},
		Username: username,
	})
}

func TestWrapWithoutHome(t *testing.T) {
	fs := testFS(false)

	got, err := fs.wrap(context.Background(), "/docs")
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if want := "/eos/user/docs"; got != want {
		t.Errorf("wrap = %q, want %q", got, want)
	}

	// already wrapped paths are returned as they are
	got, err = fs.wrap(context.Background(), "/eos/user/docs")
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if want := "/eos/user/docs"; got != want {
		t.Errorf("wrap = %q, want %q", got, want)
	}
}

func TestWrapWithHome(t *testing.T) {
	fs := testFS(true)
	ctx := userCtx("einstein")

	got, err := fs.wrap(ctx, "/docs")
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if want := "/eos/user/e/einstein/docs"; got != want {
		t.Errorf("wrap = %q, want %q", got, want)
	}

	got, err = fs.wrap(ctx, "/")
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if want := "/eos/user/e/einstein"; got != want {
		t.Errorf("wrap = %q, want %q", got, want)
	}
}

func TestWrapWithHomeNeedsUser(t *testing.T) {
	if _, err := testFS(true).wrap(context.Background(), "/docs"); err == nil {
		t.Fatal("wrap without a user succeeded, want an error")
	}
}

func TestUnwrapWithHome(t *testing.T) {
	fs := testFS(true)
	ctx := userCtx("einstein")

	got, err := fs.unwrap(ctx, "/eos/user/e/einstein/docs/file.txt")
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if want := "/docs/file.txt"; got != want {
		t.Errorf("unwrap = %q, want %q", got, want)
	}

	got, err = fs.unwrap(ctx, "/eos/user/e/einstein")
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if want := "/"; got != want {
		t.Errorf("unwrap = %q, want %q", got, want)
	}
}

func TestUnwrapWithHomeRejectsOtherHomes(t *testing.T) {
	fs := testFS(true)

	if _, err := fs.unwrap(userCtx("einstein"), "/eos/user/m/marie/docs"); err == nil {
		t.Fatal("unwrap of another user's home succeeded, want an error")
	}
}

func TestUnwrapWithoutHome(t *testing.T) {
	fs := testFS(false)

	got, err := fs.unwrap(context.Background(), "/eos/user/docs/file.txt")
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if want := "/docs/file.txt"; got != want {
		t.Errorf("unwrap = %q, want %q", got, want)
	}
}
