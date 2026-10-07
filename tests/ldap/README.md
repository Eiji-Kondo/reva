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
container-creation permissions. LOCK/UNLOCK are not included: the current
handlers contain TODOs and do not implement persistent locking.
