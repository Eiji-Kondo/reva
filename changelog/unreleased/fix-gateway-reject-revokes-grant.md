Bugfix: Remove the storage grant when a received share is rejected

`UpdateReceivedShare` removed the recipient's reference when a share was
rejected but left the storage grant in place, so the ACL still allowed the
recipient to stat and download the sharer's resource. Accepting the share again
did not re-apply the grant either, since grants are only committed when the
share is created.

The gateway now revokes the grant on `ACCEPTED -> REJECTED` (re-applying it if
the share provider update fails) and re-applies it when a share is accepted, so
declining a share really removes the access it carried and accepting a declined
share still works.

https://github.com/Eiji-Kondo/reva/pull/7
