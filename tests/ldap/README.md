# LDAP-backed localhomefs integration test

The `test-localhome-armv7.yml` workflow runs this test in a Debian Bookworm
ARMv7 container. It installs OpenLDAP with `apt`, creates a short-lived
self-signed certificate for LDAPS, provisions the test directory entries,
then starts revad with `tests/revad/revad-localhome.toml`.

The fixture uses:

- LDAP suffix: `dc=owncloud,dc=com`
- User and group search base: `ou=users,dc=owncloud,dc=com`
- Test user: `einstein` / `test-password`
- Test user: `marie` / `test-password`
- Test group: `scientists`, containing `einstein` and `marie`
- LDAPS: `localhost:636`

These credentials and certificates are only for disposable CI tests. Do not
reuse them in a deployed environment.

The workflow checks LDAPS user authentication and group membership, revad
configuration loading, successful and rejected WebDAV authentication,
localhomefs create/list/read/delete operations, PROPPATCH of the
ownCloud `favorite` property, COPY and MOVE, and viewer/editor user shares.
It checks that the recipient receives and accepts a user share, viewer grants
do not include upload permission, and editor grants include upload and
container-creation permissions. It also verifies that accepted shares appear
in the recipient's `MyShares` WebDAV path, viewer shares permit reads but deny
writes, rejecting a share removes that path, and editor writes are visible to
the owner.

The QEMU suite also checks unknown-user and wrong-password rejection,
temporary-user removal, LDAP outage and recovery, OPTIONS/GET/HEAD responses,
missing paths, path traversal rejection, repeated and nested MKCOL, overwrite
statuses, XML namespace and malformed/XXE PROPFIND/PROPPATCH requests, spaces
and Japanese filenames, 8 MiB uploads, 50-file PROPFIND listings, 12 parallel
uploads, ETag updates, COPY overwrite behavior, and the LOCK lifecycle:
exclusive lock creation, refresh, repeated-lock rejection (423), rejection of
an update without the lock token, update with the token, rejection of a wrong
UNLOCK token, and successful unlock. It also checks two clients acquiring
shared locks on one file with distinct tokens, exclusive-lock rejection while
shared locks exist, update requiring every active shared token, and removal of
one shared lock without affecting the other. localhomefs persists individual
lock records in its SQLite database and expires them automatically. Locks are
supported on existing files with `Depth: 0`; lock-null resources and
infinite-depth locks are not supported. Backends that explicitly report
locking as unimplemented log a warning and continue without lock enforcement.
Each run uses a unique database and storage root and removes them at exit.

The ARMv7 musl workflow runs the same suite with statically linked musl
binaries when `MUSL_BUILD=1`; the default local test uses the container's
native libc. When rerunning the suite with already-built ARM binaries,
`SKIP_BUILD=1` skips compilation and requires `cmd/revad/revad` and
`cmd/reva/reva` to exist.

## Running the suite on an ARMv7 Debian device

`device-localhome-test.sh` runs the LDAP-backed WebDAV and viewer/editor share
checks against the device's existing LDAP server. It expects these files under
`~/reva`: the `revad` binary, the `reva` CLI, and a device-specific TOML
configuration. Build `revad` as static ARMv7 musl and build the `reva` CLI for
ARMv7 glibc, then transfer them to the device. The glibc CLI uses Debian's
standard runtime loader; the server binary is statically linked.

The script validates the configuration, refuses to start if its local service
ports are already occupied, starts its own `revad`, and prompts for Einstein's
and Marie's test passwords without echoing them on first run. It saves those
passwords in `~/reva/test-passwords` with mode `0600`; subsequent runs read
them locally and provide them to WebDAV and CLI authentication without
echoing them or putting them in command-line arguments. Protect this file and
do not copy it into source control or share it.

It tests authenticated WebDAV access, PROPPATCH, viewer read-only behavior,
and editor write-through. It removes the uniquely named test files, directory,
and shares on exit and stops only the `revad` process it started. Its SQLite
database and storage root remain isolated under the paths configured for the
device test. To replace saved test passwords, remove `~/reva/test-passwords`;
the next run prompts for new ones.

Run it from the device's SSH shell with:

```sh
bash ~/reva/device-localhome-test.sh
```

The configured LDAP certificate must validate for the configured hostname.
Do not use the test configuration or credentials in production.
