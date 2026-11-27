package gateway

import (
	"context"
	"testing"

	userpb "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	"github.com/cs3org/reva/v3/pkg/appctx"
	"github.com/cs3org/reva/v3/pkg/token/manager/jwt"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
)

// TestImpersonateUserReplacesToken pins the mechanism the share flows use to act
// on behalf of another user: the impersonated user must travel in the context
// and in the token header, and the caller's token must be replaced instead of
// appended, because services authenticate with the first value of the header.
func TestImpersonateUserReplacesToken(t *testing.T) {
	mgr, err := jwt.New(map[string]any{"secret": "impersonation-test"})
	require.NoError(t, err)

	s := &svc{tokenmgr: mgr, c: &config{}}

	callerCtx := appctx.ContextSetToken(context.Background(), "caller-token")
	callerCtx = metadata.AppendToOutgoingContext(callerCtx, appctx.TokenHeader, "caller-token")

	ctx, err := s.impersonateUser(callerCtx, &userpb.UserId{OpaqueId: "user-a"})
	require.NoError(t, err)
	actingUser, ok := appctx.ContextGetUser(ctx)
	require.True(t, ok)
	require.Equal(t, "user-a", actingUser.GetId().GetOpaqueId())

	tkn, ok := appctx.ContextGetToken(ctx)
	require.True(t, ok)
	require.NotEqual(t, "caller-token", tkn)

	u, _, err := mgr.DismantleToken(ctx, tkn)
	require.NoError(t, err)
	require.Equal(t, "user-a", u.GetId().GetOpaqueId())

	md, ok := metadata.FromOutgoingContext(ctx)
	require.True(t, ok)
	require.Equal(t, []string{tkn}, md.Get(appctx.TokenHeader))
}

// TestImpersonateUserWithoutIdentityKeepsCaller makes sure a share that does not
// record its owner or grantee does not break the flow it is used in.
func TestImpersonateUserWithoutIdentityKeepsCaller(t *testing.T) {
	mgr, err := jwt.New(map[string]any{"secret": "impersonation-test"})
	require.NoError(t, err)

	s := &svc{tokenmgr: mgr, c: &config{}}
	callerCtx := appctx.ContextSetUser(context.Background(), &userpb.User{Id: &userpb.UserId{OpaqueId: "caller"}})

	ctx, err := s.impersonateUser(callerCtx, &userpb.UserId{})
	require.NoError(t, err)
	actingUser, ok := appctx.ContextGetUser(ctx)
	require.True(t, ok)
	require.Equal(t, "caller", actingUser.GetId().GetOpaqueId())
}
