Bugfix: Revoke group shares and their references on unshare

Removing a group share left every member with the access it carried. The
storage provider stored the grant as `egroup::scientists@idp` but rendered the
grantee as `egroup:scientists@idp` when revoking, so the delete matched no ACL
row and the group kept its permissions. Removing a share now also drops the
reference in each member's share folder: a group share is accepted per member,
so every member holds their own entry in `MyShares` and all of them have to be
cleaned up, not just a user grantee's.

Covered by a unit test on the grant lifecycle and by the ARMv7 CI run, which
now shares a file with the `scientists` group and asserts that the member's
entry and access are gone after the owner unshares it.

https://github.com/Eiji-Kondo/reva/pull/9
