Bugfix: Revoke the ACL entry when a storage grant is removed

`localfs` keys an ACL row as `u:<opaque-id>:<user-type>@<idp>` when `AddGrant`
stores it, but `RemoveGrant` looked the row up as `u:<opaque-id>@<idp>`. The
revoke is an `UPDATE ... WHERE resource=? AND grantee=?`, so it matched no row
and reported success. The recipient of an unshared or rejected share therefore
kept read access to the resource.

`RemoveGrant` now builds the same key as `AddGrant`, so removing the grant
clears the ACL entry and the resource is no longer reachable through the share.

https://github.com/Eiji-Kondo/reva/pull/7
