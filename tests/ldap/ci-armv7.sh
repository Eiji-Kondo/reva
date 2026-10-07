#!/usr/bin/env bash
set -euo pipefail

cd /workspace

export DEBIAN_FRONTEND=noninteractive
apt-get update
printf '%s\n' \
  'slapd slapd/no_configuration boolean false' \
  'slapd slapd/domain string owncloud.com' \
  'slapd slapd/organization string Reva Test' \
  'slapd slapd/password1 password admin' \
  'slapd slapd/password2 password admin' \
  'slapd slapd/purge_database boolean false' \
  'slapd slapd/move_old_database boolean true' |
  debconf-set-selections
apt-get install -y --no-install-recommends slapd ldap-utils openssl curl ca-certificates

install -d -o openldap -g openldap -m 0750 /etc/ldap/certs
openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout /etc/ldap/certs/ldap.key \
  -out /etc/ldap/certs/ldap.crt \
  -days 2 \
  -subj '/CN=localhost' \
  -addext 'basicConstraints=critical,CA:TRUE' \
  -addext 'keyUsage=critical,digitalSignature,keyEncipherment,keyCertSign,cRLSign' \
  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1,DNS:${HOSTNAME}"
chown openldap:openldap /etc/ldap/certs/ldap.crt /etc/ldap/certs/ldap.key
chmod 0640 /etc/ldap/certs/ldap.key
export LDAPTLS_CACERT=/etc/ldap/certs/ldap.crt

service slapd stop >/dev/null 2>&1 || true
slapd -d 1 -h ldapi:/// -F /etc/ldap/slapd.d -u openldap -g openldap \
  >/tmp/slapd-bootstrap.log 2>&1 &
slapd_pid=$!
cleanup() {
  if kill -0 "$slapd_pid" 2>/dev/null; then
    kill "$slapd_pid"
    wait "$slapd_pid" || true
  fi
  if [[ -n "${revad_pid:-}" ]] && kill -0 "$revad_pid" 2>/dev/null; then
    kill "$revad_pid"
    wait "$revad_pid" || true
  fi
}
trap cleanup EXIT

for attempt in $(seq 1 30); do
  if ldapwhoami -Y EXTERNAL -H ldapi:/// >/dev/null 2>&1; then
    break
  fi
  if [[ "$attempt" == 30 ]]; then
    cat /tmp/slapd-bootstrap.log
    echo "slapd did not become ready on ldapi" >&2
    exit 1
  fi
  sleep 1
done

ldapmodify -Y EXTERNAL -H ldapi:/// -f tests/ldap/enable-tls.ldif
kill "$slapd_pid"
wait "$slapd_pid" || true
slapd -d 1 -h 'ldap:/// ldaps:/// ldapi:///' -F /etc/ldap/slapd.d \
  -u openldap -g openldap >/tmp/slapd.log 2>&1 &
slapd_pid=$!

for attempt in $(seq 1 30); do
  if ldapsearch -x -H ldaps://localhost:636 \
    -D 'cn=admin,dc=owncloud,dc=com' -w admin \
    -b 'dc=owncloud,dc=com' -s base dn >/dev/null 2>&1; then
    break
  fi
  if [[ "$attempt" == 30 ]]; then
    openssl s_client -connect localhost:636 -servername localhost \
      -CAfile /etc/ldap/certs/ldap.crt -verify_return_error </dev/null || true
    ldapsearch -x -H ldaps://localhost:636 \
      -D 'cn=admin,dc=owncloud,dc=com' -w admin \
      -b 'dc=owncloud,dc=com' -s base -d 1 dn || true
    cat /tmp/slapd.log
    echo "slapd did not become ready on LDAPS port 636" >&2
    exit 1
  fi
  sleep 1
done

user_password_hash=$(slappasswd -s test-password)
sed "s|@USER_PASSWORD_HASH@|${user_password_hash}|" \
  tests/ldap/test-directory.ldif >/tmp/test-directory.ldif
ldapadd -x -H ldaps://localhost:636 \
  -D 'cn=admin,dc=owncloud,dc=com' -w admin \
  -f /tmp/test-directory.ldif

ldapwhoami -x -H ldaps://localhost:636 \
  -D 'uid=einstein,ou=users,dc=owncloud,dc=com' -w test-password
ldapsearch -x -H ldaps://localhost:636 \
  -D 'cn=admin,dc=owncloud,dc=com' -w admin \
  -b 'ou=users,dc=owncloud,dc=com' \
  '(&(objectClass=posixGroup)(memberUid=einstein))' cn gidNumber memberUid |
  grep -q 'memberUid: marie'
ldapsearch -x -H ldaps://localhost:636 \
  -D 'cn=admin,dc=owncloud,dc=com' -w admin \
  -b 'ou=users,dc=owncloud,dc=com' \
  '(&(objectClass=posixGroup)(memberUid=marie))' cn gidNumber memberUid |
  grep -q 'cn: scientists'
ldapwhoami -x -H ldaps://localhost:636 \
  -D 'uid=marie,ou=users,dc=owncloud,dc=com' -w test-password

go build -buildvcs=false -o ./cmd/revad/revad ./cmd/revad/main
go build -buildvcs=false -o ./cmd/reva/reva ./cmd/reva
./cmd/revad/revad -t -c tests/revad/revad-localhome.toml
mkdir -p tmp/revalocalstorage/shares
./cmd/revad/revad -c tests/revad/revad-localhome.toml >/tmp/revad.log 2>&1 &
revad_pid=$!

DAV_URL=http://127.0.0.1:38001/remote.php/webdav
for attempt in $(seq 1 60); do
  if ! kill -0 "$revad_pid" 2>/dev/null; then
    cat /tmp/revad.log
    echo "revad exited before becoming ready" >&2
    exit 1
  fi
  status=$(curl -sS -o /dev/null -w '%{http_code}' \
    --user einstein:test-password -X OPTIONS "$DAV_URL/" || true)
  if [[ "$status" != 000 ]]; then
    break
  fi
  sleep 1
done
if [[ "$status" == 000 ]]; then
  cat /tmp/revad.log
  echo "revad did not become ready" >&2
  exit 1
fi

expect_status() {
  local expected="$1"
  shift
  local actual
  actual=$(curl -sS -o /tmp/dav-response -w '%{http_code}' "$@")
  if [[ "$actual" != "$expected" ]]; then
    cat /tmp/dav-response
    printf 'expected HTTP %s, got %s\n' "$expected" "$actual" >&2
    exit 1
  fi
}

expect_status 401 -X PROPFIND -H 'Depth: 0' "$DAV_URL/"
expect_status 401 --user einstein:wrong-password \
  -X PROPFIND -H 'Depth: 0' "$DAV_URL/"
expect_status 207 --user einstein:test-password \
  -X PROPFIND -H 'Depth: 0' "$DAV_URL/"

test_id="$(date +%s%N)"
test_dir="$DAV_URL/ci-check-$test_id"
expect_status 201 --user einstein:test-password -X MKCOL "$test_dir"
printf 'localhomefs test\n' >/tmp/localhome-test.txt
expect_status 201 --user einstein:test-password \
  -T /tmp/localhome-test.txt "$test_dir/test.txt"
expect_status 207 --user einstein:test-password \
  -X PROPFIND -H 'Depth: 1' "$test_dir/"
expect_status 200 --user einstein:test-password "$test_dir/test.txt"
cmp /tmp/localhome-test.txt /tmp/dav-response

expect_status 200 --user einstein:test-password \
  -X PROPPATCH -H 'Content-Type: application/xml; charset=utf-8' \
  --data-binary @- "$test_dir/test.txt" <<'XML'
<?xml version="1.0" encoding="utf-8"?>
<d:propertyupdate xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
  <d:set><d:prop><oc:favorite>1</oc:favorite></d:prop></d:set>
</d:propertyupdate>
XML
expect_status 207 --user einstein:test-password \
  -X PROPFIND -H 'Depth: 0' \
  -H 'Content-Type: application/xml; charset=utf-8' \
  --data-binary @- "$test_dir/test.txt" <<'XML'
<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
  <d:prop><oc:favorite/></d:prop>
</d:propfind>
XML
grep -q '<oc:favorite>1</oc:favorite>' /tmp/dav-response
expect_status 200 --user einstein:test-password \
  -X PROPPATCH -H 'Content-Type: application/xml; charset=utf-8' \
  --data-binary @- "$test_dir/test.txt" <<'XML'
<?xml version="1.0" encoding="utf-8"?>
<d:propertyupdate xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
  <d:remove><d:prop><oc:favorite/></d:prop></d:remove>
</d:propertyupdate>
XML
expect_status 207 --user einstein:test-password \
  -X PROPFIND -H 'Depth: 0' \
  -H 'Content-Type: application/xml; charset=utf-8' \
  --data-binary @- "$test_dir/test.txt" <<'XML'
<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
  <d:prop><oc:favorite/></d:prop>
</d:propfind>
XML
if grep -q '<oc:favorite>1</oc:favorite>' /tmp/dav-response; then
  echo "PROPPATCH remove did not clear the favorite property" >&2
  exit 1
fi

expect_status 201 --user einstein:test-password -X COPY \
  -H "Destination: $DAV_URL/ci-copy-$test_id.txt" \
  -H 'Overwrite: F' "$test_dir/test.txt"
expect_status 201 --user einstein:test-password -X MOVE \
  -H "Destination: $DAV_URL/ci-moved-$test_id.txt" \
  -H 'Overwrite: F' "$DAV_URL/ci-copy-$test_id.txt"
expect_status 200 --user einstein:test-password -I "$DAV_URL/ci-moved-$test_id.txt"
expect_status 204 --user einstein:test-password -X DELETE "$DAV_URL/ci-moved-$test_id.txt"

owner_token=/tmp/reva-token-einstein
recipient_token=/tmp/reva-token-marie
share_dir="share-ci-$test_id"
./cmd/reva/reva -insecure -host=localhost:9142 -token-file="$owner_token" \
  login --username=einstein --password=test-password basic
./cmd/reva/reva -insecure -host=localhost:9142 -token-file="$recipient_token" \
  login --username=marie --password=test-password basic
./cmd/reva/reva -insecure -host=localhost:9142 -token-file="$owner_token" \
  mkdir "/localfs/$share_dir"
printf 'shared localhomefs content\n' >/tmp/shared-test.txt
expect_status 201 --user einstein:test-password \
  -T /tmp/shared-test.txt "$DAV_URL/$share_dir/shared.txt"

./cmd/reva/reva -insecure -host=localhost:9142 -token-file="$owner_token" \
  share-create -grantee=marie -role=viewer "/localfs/$share_dir/shared.txt" \
  >/tmp/viewer-share.txt
grep -q 'initiate_file_download:true' /tmp/viewer-share.txt
if grep -q 'initiate_file_upload:true' /tmp/viewer-share.txt; then
  echo "viewer share unexpectedly includes upload permission" >&2
  exit 1
fi
received_shares=$(./cmd/reva/reva -insecure -host=localhost:9142 \
  -token-file="$recipient_token" share-list-received)
printf '%s\n' "$received_shares"
received_share_id=$(printf '%s\n' "$received_shares" |
  awk -F '|' '/GRANTEE_TYPE_USER/ { gsub(/^[[:space:]]+|[[:space:]]+$/, "", $2); print $2; exit }')
if [[ -z "$received_share_id" ]]; then
  echo "Marie did not receive Einstein's viewer share" >&2
  exit 1
fi
./cmd/reva/reva -insecure -host=localhost:9142 -token-file="$recipient_token" \
  share-update-received -state=accepted "$received_share_id"
./cmd/reva/reva -insecure -host=localhost:9142 -token-file="$owner_token" \
  share-list | grep -q 'marie'
printf 'editor permission test\n' >/tmp/editor-test.txt
expect_status 201 --user einstein:test-password \
  -T /tmp/editor-test.txt "$DAV_URL/$share_dir/editor.txt"
./cmd/reva/reva -insecure -host=localhost:9142 -token-file="$owner_token" \
  share-create -grantee=marie -role=editor "/localfs/$share_dir/editor.txt" \
  >/tmp/editor-share.txt
grep -q 'initiate_file_upload:true' /tmp/editor-share.txt
grep -q 'create_container:true' /tmp/editor-share.txt
./cmd/reva/reva -insecure -host=localhost:9142 -token-file="$recipient_token" \
  share-list-received | grep -q 'marie'
expect_status 204 --user einstein:test-password -X DELETE "$test_dir/"

echo "ARMv7 Bookworm LDAP/LDAPS, WebDAV, PROPPATCH, and share permission tests passed"
