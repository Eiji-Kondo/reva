Bugfix: Commit share grants as the share owner and drop the reference on unshare

Accepting or rejecting a received share changes the ACL of the sharer's
resource, but the gateway made those storage calls as the recipient. A storage
provider only lets the owner of a resource change its ACL, so accepting a share
again after rejecting it failed with `permission denied` and left the recipient
without access.

The gateway now acts as the share owner for the storage grant calls of
`UpdateReceivedShare`. Removing a share as the owner also deletes the
recipient's reference, which used to stay behind and fail with a permission
error in the share folder.

https://github.com/Eiji-Kondo/reva/pull/8
