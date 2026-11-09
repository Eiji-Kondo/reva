package ocgraph

import (
	"context"
	"testing"

	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
)

func TestLastModifiedByFromMetadata(t *testing.T) {
	tests := []struct {
		name       string
		md         *provider.ArbitraryMetadata
		wantNil    bool
		wantID     string
		wantDispay string
	}{
		{name: "nil metadata", md: nil, wantNil: true},
		{name: "empty metadata", md: &provider.ArbitraryMetadata{}, wantNil: true},
		{
			name:    "no author recorded",
			md:      &provider.ArbitraryMetadata{Metadata: map[string]string{"other": "x"}},
			wantNil: true,
		},
		{
			name:   "username only",
			md:     &provider.ArbitraryMetadata{Metadata: map[string]string{lastModifiedByMetadataKey: "marie"}},
			wantID: "marie",
		},
		{
			name: "username and display name",
			md: &provider.ArbitraryMetadata{Metadata: map[string]string{
				lastModifiedByMetadataKey:                   "einstein",
				lastModifiedByMetadataKey + "-display-name": "Albert Einstein",
			}},
			wantID:     "einstein",
			wantDispay: "Albert Einstein",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lastModifiedByFromMetadata(tt.md)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil || got.User == nil {
				t.Fatalf("expected a user identity, got %+v", got)
			}
			if got.User.Id == nil || *got.User.Id != tt.wantID {
				t.Fatalf("id: got %v, want %q", got.User.Id, tt.wantID)
			}
			if got.User.DisplayName != tt.wantDispay {
				t.Fatalf("displayName: got %q, want %q", got.User.DisplayName, tt.wantDispay)
			}
		})
	}
}

// An encoded resource id (<storage>$<space>!<item>) must be usable as a CS3
// reference without any gateway lookup.
func TestReferenceForItemIDEncoded(t *testing.T) {
	s := &svc{}
	ref, err := s.referenceForItemID(context.Background(), nil, "projects$F5YHE33KMVRXI4Y=", "projects$F5YHE33KMVRXI4Y=!fileid-%2Fnote.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.ResourceId == nil {
		t.Fatal("expected a resource id reference")
	}
	if ref.ResourceId.StorageId != "projects" {
		t.Fatalf("storage id: got %q", ref.ResourceId.StorageId)
	}
	if ref.ResourceId.OpaqueId != "fileid-%2Fnote.txt" {
		t.Fatalf("opaque id: got %q", ref.ResourceId.OpaqueId)
	}
}
