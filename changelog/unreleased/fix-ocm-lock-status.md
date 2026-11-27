Bugfix: Stop answering 500 when a lock is checked on a share

The `ocmoutcoming` storage driver turned every status other than OK or
NOT_FOUND from the local storage provider into an internal error, so a
provider that does not support locks (or that refuses to lock a directory) made
the WebDAV lock guard answer 500. Uploading a file into an existing folder of a
received share failed with `MkdirAll /dir/: 500`, because the receiving client
creates the parent collection with a MKCOL before the PUT.

The driver now maps `CODE_UNIMPLEMENTED` to `errtypes.NotSupported`, which the
lock guard already tolerates, for the four lock operations. Other unexpected
statuses still become internal errors.

https://github.com/Eiji-Kondo/reva/pull/5
