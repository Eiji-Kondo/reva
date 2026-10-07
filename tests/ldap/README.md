# LDAP-backed localhomefs integration test

The `test-localhome-armv7.yml` workflow runs this test in a Debian Bookworm
ARMv7 container. It installs OpenLDAP with `apt`, creates a short-lived
self-signed certificate for LDAPS, provisions the test directory entries,
then starts revad with `tests/revad/revad-localhome.toml`.

The fixture uses:

- LDAP suffix: `dc=owncloud,dc=com`
- User and group search base: `ou=users,dc=owncloud,dc=com`
- Test user: `einstein` / `test-password`
- Test group: `scientists`, containing `einstein`
- LDAPS: `localhost:636`

These credentials and certificates are only for disposable CI tests. Do not
reuse them in a deployed environment.

The workflow checks LDAPS user and group lookups, revad configuration loading,
successful and rejected WebDAV authentication, and basic localhomefs
create/list/read/delete operations.
