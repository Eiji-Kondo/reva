Bugfix: Report an unresolvable reference as not found

`localfs.GetMD` wrapped the error from `resolve` with `errors.Wrap`, which erased
the `errtypes.NotFound` type. The storage provider service matches on the
concrete error type, so statting a resource id that the driver cannot resolve
answered `CODE_INTERNAL` instead of `CODE_NOT_FOUND`. Creating an OCM share for
a non-existent resource id therefore returned 500 rather than 404.

`GetMD` now returns the resolve error unchanged, as `localdb` already does.

https://github.com/Eiji-Kondo/reva/pull/5
