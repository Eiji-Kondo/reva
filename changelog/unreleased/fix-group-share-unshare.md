Bugfix: Revoke group shares and their references on unshare

Removing a group share left every member with the access it carried, and the
entry in their share folder answered with a permission error instead of
disappearing. Two defects:

The storage provider stored a group grant as `egroup::scientists@idp` but
rendered the grantee as `egroup:scientists@idp` when revoking, so the delete
matched no ACL row and the group kept its permissions. Adding and removing now
share one formatter.

A group share is accepted per member, so every member keeps their own reference
in `MyShares`. Removing the share now resolves the group and drops the reference
of each member. Where a group provider cannot list members, the reference is
still invalidated: a reference whose grant no longer exists is reported as not
found, so the entry stops resolving and drops out of the share folder listing
instead of failing with a permission error.

Covered by unit tests on the grant lifecycle and on reference resolution, and by
the ARMv7 CI run, which now shares a file with the `scientists` group and
asserts that the member's entry and access are gone after the owner unshares it.

https://github.com/Eiji-Kondo/reva/pull/9
