Enhancement: Jail users into their home in the eos driver

The `eoshome` driver was removed when the storage drivers were refactored, but
the `eos` driver kept `user_layout` and `enable_home_creation`, so a home mount
built on it resolved every path below the bare namespace. A request for
`/litmus` became `/eos/user/litmus` instead of `/eos/user/e/einstein/litmus`,
which does not exist, and every write failed.

The new `enable_home` option resolves `user_layout` against the request user and
joins it to the namespace in both directions, so such a mount jails users into
their home again. It defaults to false: mounts that address a namespace
directly, like the project spaces in the same test stack, keep working as
before.

https://github.com/Eiji-Kondo/reva/pull/11
