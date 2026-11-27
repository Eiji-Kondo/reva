package gateway

import (
	"context"
	"testing"

	rpc "github.com/cs3org/go-cs3apis/cs3/rpc/v1beta1"
	collaboration "github.com/cs3org/go-cs3apis/cs3/sharing/collaboration/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	"github.com/cs3org/reva/v3/pkg/permissions"
	"github.com/stretchr/testify/require"
)

// TestUpdateReceivedShareManagesStorageGrant pins the grant lifecycle of a
// received share: rejecting it must revoke the grant, accepting it again must
// restore it. Without the revoke, a recipient who declined a share kept the
// access the grant carried.
func TestUpdateReceivedShareManagesStorageGrant(t *testing.T) {
	h := newShareHierarchyGatewayHarness(t)
	share := h.createShare(t, parentPath, userAGrantee(), permissions.NewViewerRole().CS3ResourcePermissions(), false)
	h.assertCanRead(t, userAGrantee(), parentPath)

	updateState := func(state collaboration.ShareState) {
		t.Helper()
		res, err := h.svc.UpdateReceivedShare(context.Background(), &collaboration.UpdateReceivedShareRequest{
			Share: &collaboration.ReceivedShare{
				Share: &collaboration.Share{Id: &collaboration.ShareId{OpaqueId: share.Id.OpaqueId}},
				State: state,
			},
		})
		require.NoError(t, err)
		require.NotNil(t, res)
		require.Equal(t, rpc.Code_CODE_OK, res.Status.Code, res.Status.Message)
	}

	updateState(collaboration.ShareState_SHARE_STATE_REJECTED)
	h.assertCannotRead(t, userAGrantee(), parentPath)

	updateState(collaboration.ShareState_SHARE_STATE_ACCEPTED)
	h.assertCanRead(t, userAGrantee(), parentPath)
}

var _ = provider.ResourcePermissions{}
