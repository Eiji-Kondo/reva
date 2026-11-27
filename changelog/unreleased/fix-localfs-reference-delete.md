Bugfix: Stop a removed share reference from resolving

`localfs` stores a received share in two places: a marker directory under
`.shadow/references/<user>/MyShares/<name>`, and the target in the
`share_references` table, keyed by that marker path. `Delete` of the reference
recycled the marker but left the row behind, and `resolveShareReference` only
looked at the row. A recipient who rejected a share therefore kept the file: the
entry vanished from the `MyShares` listing while `GET /MyShares/<name>` still
answered 200.

Resolution now requires the marker, the same thing the share folder listing
shows, so a removed reference is not-found. A missing row is reported as
not-found as well, instead of a generic error that surfaced as 500. Deleting and
restoring the entry from the trash keeps working, because the marker comes back.

https://github.com/Eiji-Kondo/reva/pull/6
