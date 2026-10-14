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
	"testing"
	"time"
)

func TestParseLockTimeout(t *testing.T) {
	tests := []struct {
		header string
		want   time.Duration
		ok     bool
	}{
		{header: "", want: time.Hour, ok: true},
		{header: "Second-30", want: 30 * time.Second, ok: true},
		{header: "Infinite, Second-60", want: time.Minute, ok: true},
		{header: "Second-9999999", want: maxLockTimeout, ok: true},
		{header: "Second-0", ok: false},
		{header: "invalid", ok: false},
	}
	for _, test := range tests {
		got, ok := parseLockTimeout(test.header)
		if got != test.want || ok != test.ok {
			t.Errorf("parseLockTimeout(%q) = (%s, %t), want (%s, %t)",
				test.header, got, ok, test.want, test.ok)
		}
	}
}

func TestLockTokens(t *testing.T) {
	tokens := lockTokens(`<https://example.test/file> (<opaquelocktoken:first>) (Not [valid])`)
	if len(tokens) != 1 || tokens[0] != "opaquelocktoken:first" {
		t.Fatalf("lockTokens returned %#v", tokens)
	}
	if _, ok := singleLockToken(`<opaquelocktoken:first> <opaquelocktoken:second>`); ok {
		t.Fatal("singleLockToken accepted multiple lock tokens")
	}
}

func TestLockTokensAreScopedToDAVResource(t *testing.T) {
	header := `(<opaquelocktoken:untagged>) <https://dav.example.test/other.txt> (<opaquelocktoken:tagged>)`

	tokens, ok := lockTokensForDAVResource(header, "/files/user/file.txt", "/files/user/file.txt")
	if !ok || len(tokens) != 1 || len(tokens[0]) != 1 || tokens[0][0] != "opaquelocktoken:untagged" {
		t.Fatalf("request-resource tokens = %#v, valid=%t", tokens, ok)
	}
	tokens, ok = lockTokensForDAVResource(header, "/files/user/file.txt", "/other.txt")
	if !ok || len(tokens) != 1 || len(tokens[0]) != 1 || tokens[0][0] != "opaquelocktoken:tagged" {
		t.Fatalf("tagged-resource tokens = %#v, valid=%t", tokens, ok)
	}
	tokens, ok = lockTokensForDAVResource(`</wrong.txt> (<opaquelocktoken:foreign>)`,
		"/files/user/file.txt", "/files/user/file.txt")
	if !ok || len(tokens) != 0 {
		t.Fatalf("foreign tagged token authorized target: %#v, valid=%t", tokens, ok)
	}
	if _, ok := lockTokensForDAVResource(`(<opaquelocktoken:broken>`, "/file.txt", "/file.txt"); ok {
		t.Fatal("malformed If header accepted")
	}
}

func TestLockTokensMustBeInOneDAVIfList(t *testing.T) {
	tests := []struct {
		header    string
		wantLists int
		wantFirst []string
	}{
		{header: `(<opaquelocktoken:first> <opaquelocktoken:second>)`,
			wantLists: 1, wantFirst: []string{"opaquelocktoken:first", "opaquelocktoken:second"}},
		{header: `(<opaquelocktoken:first>) (<opaquelocktoken:second>)`,
			wantLists: 2, wantFirst: []string{"opaquelocktoken:first"}},
		{header: `(Not <opaquelocktoken:first>)`, wantLists: 1},
	}
	for _, test := range tests {
		got, ok := lockTokensForDAVResource(test.header, "/file.txt", "/file.txt")
		if !ok || len(got) != test.wantLists {
			t.Fatalf("lockTokensForDAVResource(%q) = %#v, %t", test.header, got, ok)
		}
		if test.wantLists > 0 && len(got[0]) != len(test.wantFirst) {
			t.Fatalf("first condition-list tokens = %#v, want %#v", got[0], test.wantFirst)
		}
		for i, want := range test.wantFirst {
			if got[0][i] != want {
				t.Fatalf("condition-list token %d = %q, want %q", i, got[0][i], want)
			}
		}
	}
}
