Bugfix: Report a missing resource when a lock is requested

The eos driver wrapped every error of the read access check it performs before
answering a lock request. The storageprovider service picks the rpc status from
the concrete error type, so a wrapped `NotFound` fell through to the internal
error branch, and the WebDAV lock check of a path that does not exist yet -
every `MKCOL` and `PUT` of a new resource - answered `500`.

The driver now returns the `NotFound` error as it comes from the storage client,
so the check reports `404` and the request continues.

https://github.com/Eiji-Kondo/reva/pull/11
