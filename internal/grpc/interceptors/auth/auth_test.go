// Copyright 2018-2026 CERN
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

package auth

import "testing"

func TestParseConfigCacheSizeDefault(t *testing.T) {
	conf, err := parseConfig(map[string]any{})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if conf.CacheSize != defaultCacheSize {
		t.Fatalf("CacheSize = %d, want %d", conf.CacheSize, defaultCacheSize)
	}
}

func TestParseConfigCacheSizeOverride(t *testing.T) {
	conf, err := parseConfig(map[string]any{"cache_size": 500})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if conf.CacheSize != 500 {
		t.Fatalf("CacheSize = %d, want 500", conf.CacheSize)
	}
}
