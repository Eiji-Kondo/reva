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

package memory

import (
	"runtime"
	"testing"
)

func TestApplyDefaultsCacheSize(t *testing.T) {
	c := &config{}
	c.ApplyDefaults()
	if c.CacheSize != defaultCacheSize {
		t.Fatalf("expected default cache size %d, got %d", defaultCacheSize, c.CacheSize)
	}
}

func TestApplyDefaultsCacheSizeOverride(t *testing.T) {
	c := &config{CacheSize: 42}
	c.ApplyDefaults()
	if c.CacheSize != 42 {
		t.Fatalf("expected override to be preserved, got %d", c.CacheSize)
	}
}

// TestDefaultCacheSizeIsSmall guards against a regression to the old 1e6
// default, which made gcache preallocate tens of MiB per cache up front.
func TestDefaultCacheSizeIsSmall(t *testing.T) {
	if defaultCacheSize > 100000 {
		t.Fatalf("default cache size %d is too large; gcache preallocates for it", defaultCacheSize)
	}
}

// TestNewDoesNotPreallocate proves the fix at runtime: building the cache must
// not reserve the tens of MiB the old default did. The heap is measured right
// after construction, before anything is inserted.
func TestNewDoesNotPreallocate(t *testing.T) {
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	c, err := New[any](map[string]any{})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if c == nil {
		t.Fatal("New returned nil cache")
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	// The old default (1e6) allocated ~50 MiB up front. With the new default an
	// empty cache must cost far less; 8 MiB is a generous ceiling.
	grown := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if grown > 8<<20 {
		t.Fatalf("empty cache grew the heap by %d bytes; expected well under 8 MiB", grown)
	}
}

// TestNewWithNilConfig covers the OCS shares handler, which passes an unset
// driver map and therefore relies entirely on the default.
func TestNewWithNilConfig(t *testing.T) {
	c, err := New[any](nil)
	if err != nil {
		t.Fatalf("New(nil) returned error: %v", err)
	}
	if c == nil {
		t.Fatal("New(nil) returned nil")
	}
}
