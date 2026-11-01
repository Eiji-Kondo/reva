package ocgraph

import (
	"context"
	"testing"

	userpb "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	types "github.com/cs3org/go-cs3apis/cs3/types/v1beta1"
)

func TestCs3StorageSpaceToDriveRootIdentityAndURLs(t *testing.T) {
	const spaceID = "localfs$F5WG6Y3BNRTHG==="

	s := &svc{c: &config{
		WebDavBase: "https://cloud.home.lan/remote.php/dav/spaces",
		WebBase:    "https://cloud.home.lan/files/spaces",
	}}

	space := &provider.StorageSpace{
		Id:        &provider.StorageSpaceId{OpaqueId: spaceID},
		SpaceType: "personal",
		Name:      "einstein",
		RootInfo: &provider.ResourceInfo{
			Path: "/localfs",
			Etag: "abc123",
			Mtime: &types.Timestamp{
				Seconds: 1700000000,
			},
		},
	}

	drive := s.cs3StorageSpaceToDrive(context.Background(), &userpb.User{}, space)
	if drive == nil {
		t.Fatal("expected a drive, got nil")
	}
	if drive.Root == nil {
		t.Fatal("expected drive.Root, got nil")
	}

	if got := drive.Root.GetId(); got != spaceID {
		t.Errorf("drive.Root.Id = %q, want %q", got, spaceID)
	}
	if got := drive.Root.GetETag(); got != "abc123" {
		t.Errorf("drive.Root.ETag = %q, want %q", got, "abc123")
	}
	if got := drive.Root.GetWebDavUrl(); got != "https://cloud.home.lan/remote.php/dav/spaces/"+spaceID {
		t.Errorf("drive.Root.WebDavUrl = %q", got)
	}
	if got := drive.GetWebUrl(); got != "https://cloud.home.lan/files/spaces/"+spaceID {
		t.Errorf("drive.WebUrl = %q", got)
	}
}
