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
if [[ "${MUSL_BUILD:-0}" == "1" ]]; then
  apt-get install -y --no-install-recommends musl-tools
fi

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
test_id="$(date +%s%N)"
runtime_root="./tmp/revalocalstorage-$test_id"
runtime_db="./tmp/revalocalstorage-$test_id.db"
runtime_config="/tmp/revad-localhome-$test_id.toml"
cleanup() {
  if [[ "${temporary_user_created:-0}" == 1 ]]; then
    ldapdelete -x -H ldaps://localhost:636 \
      -D 'cn=admin,dc=owncloud,dc=com' -w admin \
      'uid=revad-temporary,ou=users,dc=owncloud,dc=com' >/dev/null 2>&1 || true
  fi
  if kill -0 "$slapd_pid" 2>/dev/null; then
    kill "$slapd_pid"
    wait "$slapd_pid" || true
  fi
  if [[ -n "${revad_pid:-}" ]] && kill -0 "$revad_pid" 2>/dev/null; then
    kill "$revad_pid"
    wait "$revad_pid" || true
  fi
  rm -f -- "$runtime_config" "$runtime_db" "$runtime_db-shm" "$runtime_db-wal"
  rm -rf -- "$runtime_root"
}
trap cleanup EXIT
temporary_user_created=0

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

build_binary() {
  local output="$1"
  local package="$2"
  if [[ "${MUSL_BUILD:-0}" == "1" ]]; then
    CGO_ENABLED=1 CC=musl-gcc go build -tags sqlite_omit_load_extension \
      -buildvcs=false -ldflags '-extldflags -static' -o "$output" "$package"
  else
    go build -buildvcs=false -o "$output" "$package"
  fi
}

if [[ "${SKIP_BUILD:-0}" == "1" ]]; then
  [[ -x ./cmd/revad/revad && -x ./cmd/reva/reva ]] || {
    echo "SKIP_BUILD=1 requires prebuilt revad and reva binaries" >&2
    exit 1
  }
else
  build_binary ./cmd/revad/revad ./cmd/revad/main
  build_binary ./cmd/reva/reva ./cmd/reva
fi
sed \
  -e "s|db_name = \"test\"|db_name = \"$runtime_db\"|" \
  -e "s|root = \"./tmp/revalocalstorage\"|root = \"$runtime_root\"|g" \
  tests/revad/revad-localhome.toml >"$runtime_config"
./cmd/revad/revad -t -c "$runtime_config"
mkdir -p "$runtime_root/shares"
./cmd/revad/revad -c "$runtime_config" >/tmp/revad.log 2>&1 &
revad_pid=$!

DAV_URL=http://127.0.0.1:38001/remote.php/webdav
OCS_URL=http://127.0.0.1:38001/ocs/v2.php
METRICS_URL=http://127.0.0.1:38001/metrics
SOAK_SECONDS="${SOAK_SECONDS:-0}"
[[ "$SOAK_SECONDS" =~ ^[0-9]+$ ]] || {
  echo "SOAK_SECONDS must be a non-negative integer" >&2
  exit 1
}
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

restart_revad() {
  if [[ -n "$revad_pid" ]] && kill -0 "$revad_pid" 2>/dev/null; then
    kill -TERM "$revad_pid"
    wait "$revad_pid" || true
  fi
  revad_pid=""
  ./cmd/revad/revad -c "$runtime_config" >>/tmp/revad.log 2>&1 &
  revad_pid=$!
  for attempt in $(seq 1 60); do
    if ! kill -0 "$revad_pid" 2>/dev/null; then
      cat /tmp/revad.log >&2
      echo "revad exited before becoming ready after restart" >&2
      exit 1
    fi
    status=$(curl -sS -o /dev/null -w '%{http_code}' \
      --user einstein:test-password -X OPTIONS "$DAV_URL/" || true)
    if [[ "$status" != 000 ]]; then
      return
    fi
    sleep 1
  done
  cat /tmp/revad.log >&2
  echo "revad did not become ready after restart" >&2
  exit 1
}

restart_revad() {
  if [[ -n "$revad_pid" ]] && kill -0 "$revad_pid" 2>/dev/null; then
    kill -TERM "$revad_pid"
    wait "$revad_pid" || true
  fi
  revad_pid=""
  ./cmd/revad/revad -c "$runtime_config" >>/tmp/revad.log 2>&1 &
  revad_pid=$!
  for attempt in $(seq 1 60); do
    if ! kill -0 "$revad_pid" 2>/dev/null; then
      cat /tmp/revad.log >&2
      echo "revad exited before becoming ready after restart" >&2
      exit 1
    fi
    status=$(curl -sS -o /dev/null -w '%{http_code}' \
      --user einstein:test-password -X OPTIONS "$DAV_URL/" || true)
    if [[ "$status" != 000 ]]; then
      return
    fi
    sleep 1
  done
  cat /tmp/revad.log >&2
  echo "revad did not become ready after restart" >&2
  exit 1
}

expect_status 401 -X PROPFIND -H 'Depth: 0' "$DAV_URL/"
expect_status 401 --user einstein:wrong-password \
  -X PROPFIND -H 'Depth: 0' "$DAV_URL/"
expect_status 401 --user not-registered:test-password \
  -X PROPFIND -H 'Depth: 0' "$DAV_URL/"
expect_status 207 --user einstein:test-password \
  -X PROPFIND -H 'Depth: 0' "$DAV_URL/"
expect_status 207 --user marie:test-password \
  -X PROPFIND -H 'Depth: 0' "$DAV_URL/"

temporary_user_hash=$(slappasswd -s test-password)
cat >/tmp/reva-temporary-user.ldif <<EOF
dn: uid=revad-temporary,ou=users,dc=owncloud,dc=com
objectClass: inetOrgPerson
objectClass: posixAccount
objectClass: shadowAccount
uid: revad-temporary
cn: Temporary Reva Test
sn: Test
uidNumber: 10999
gidNumber: 10001
homeDirectory: /home/revad-temporary
loginShell: /bin/bash
userPassword: ${temporary_user_hash}
EOF
ldapadd -x -H ldaps://localhost:636 \
  -D 'cn=admin,dc=owncloud,dc=com' -w admin -f /tmp/reva-temporary-user.ldif
temporary_user_created=1
expect_status 207 --user revad-temporary:test-password \
  -X PROPFIND -H 'Depth: 0' "$DAV_URL/"
ldapdelete -x -H ldaps://localhost:636 \
  -D 'cn=admin,dc=owncloud,dc=com' -w admin \
  'uid=revad-temporary,ou=users,dc=owncloud,dc=com'
temporary_user_created=0
expect_status 401 --user revad-temporary:test-password \
  -X PROPFIND -H 'Depth: 0' "$DAV_URL/"
rm -f /tmp/reva-temporary-user.ldif

cat >/tmp/reva-duplicate-users.ldif <<EOF
dn: uid=revad-search-one,ou=users,dc=owncloud,dc=com
objectClass: inetOrgPerson
objectClass: posixAccount
objectClass: shadowAccount
uid: revad-search-hit
cn: revad-search-hit
sn: Search One
uidNumber: 10997
gidNumber: 10001
homeDirectory: /home/revad-search-one
loginShell: /bin/bash
userPassword: ${temporary_user_hash}

dn: uid=revad-search-two,ou=users,dc=owncloud,dc=com
objectClass: inetOrgPerson
objectClass: posixAccount
objectClass: shadowAccount
uid: revad-search-hit
cn: revad-search-hit
sn: Search Two
uidNumber: 10998
gidNumber: 10001
homeDirectory: /home/revad-search-two
loginShell: /bin/bash
userPassword: ${temporary_user_hash}
EOF
ldapadd -x -H ldaps://localhost:636 \
  -D 'cn=admin,dc=owncloud,dc=com' -w admin \
  -f /tmp/reva-duplicate-users.ldif
duplicate_search_status=$(curl -sS -o /tmp/sharees-response -w '%{http_code}' \
  --user einstein:test-password -H 'OCS-APIRequest: true' \
  "$OCS_URL/apps/files_sharing/api/v1/sharees?search=revad-search-hit&format=json")
[[ "$duplicate_search_status" == 200 ]] || {
  echo "LDAP multi-hit sharee search returned HTTP $duplicate_search_status" >&2
  cat /tmp/sharees-response >&2
  exit 1
}
duplicate_count=$(grep -o '"shareType":0' /tmp/sharees-response | wc -l)
[[ "$duplicate_count" == 2 ]] || {
  echo "LDAP multi-hit search returned $duplicate_count users, expected 2" >&2
  cat /tmp/sharees-response >&2
  exit 1
}
cat >/tmp/reva-update-user.ldif <<EOF
dn: uid=einstein,ou=users,dc=owncloud,dc=com
changetype: modify
replace: mail
mail: einstein-updated@example.org
EOF
ldapmodify -x -H ldaps://localhost:636 \
  -D 'cn=admin,dc=owncloud,dc=com' -w admin -f /tmp/reva-update-user.ldif
updated_search_status=$(curl -sS -o /tmp/sharees-response -w '%{http_code}' \
  --user einstein:test-password -H 'OCS-APIRequest: true' \
  "$OCS_URL/apps/files_sharing/api/v1/sharees?search=einstein-updated%40example.org&format=json")
[[ "$updated_search_status" == 200 ]] &&
  grep -q '"shareWith"' /tmp/sharees-response || {
  echo "LDAP search did not reflect changed user attributes" >&2
  cat /tmp/sharees-response >&2
  exit 1
}
cat >/tmp/reva-restore-user.ldif <<EOF
dn: uid=einstein,ou=users,dc=owncloud,dc=com
changetype: modify
replace: mail
mail: einstein@example.org
EOF
ldapmodify -x -H ldaps://localhost:636 \
  -D 'cn=admin,dc=owncloud,dc=com' -w admin -f /tmp/reva-restore-user.ldif
ldapdelete -x -H ldaps://localhost:636 \
  -D 'cn=admin,dc=owncloud,dc=com' -w admin \
  'uid=revad-search-one,ou=users,dc=owncloud,dc=com' \
  'uid=revad-search-two,ou=users,dc=owncloud,dc=com'
duplicate_search_status=$(curl -sS -o /tmp/sharees-response -w '%{http_code}' \
  --user einstein:test-password -H 'OCS-APIRequest: true' \
  "$OCS_URL/apps/files_sharing/api/v1/sharees?search=revad-search-hit&format=json")
[[ "$duplicate_search_status" == 200 ]] &&
  ! grep -q '"shareWith"' /tmp/sharees-response || {
  echo "LDAP sharee search did not reflect deletion of matching users" >&2
  cat /tmp/sharees-response >&2
  exit 1
}
rm -f /tmp/reva-duplicate-users.ldif
rm -f /tmp/reva-update-user.ldif /tmp/reva-restore-user.ldif

expect_status 204 -D /tmp/options-headers --user einstein:test-password \
  -X OPTIONS "$DAV_URL/"
grep -qi '^Allow:.*PROPFIND' /tmp/options-headers
grep -qi '^Allow:.*PROPPATCH' /tmp/options-headers
grep -qi '^Allow:.*PUT' /tmp/options-headers

expect_status 404 --user einstein:test-password \
  "$DAV_URL/not-found-$test_id"
traversal_status=$(curl --path-as-is -sS -o /tmp/dav-response -w '%{http_code}' \
  --user einstein:test-password "$DAV_URL/../../etc/passwd")
case "$traversal_status" in
  2*) echo "path traversal unexpectedly returned HTTP $traversal_status" >&2; exit 1 ;;
esac

test_dir="$DAV_URL/ci-check-$test_id"
expect_status 201 --user einstein:test-password -X MKCOL "$test_dir"
expect_status 405 --user einstein:test-password -X MKCOL "$test_dir"
expect_status 409 --user einstein:test-password \
  -X MKCOL "$test_dir/missing-parent/child"
expect_status 201 --user einstein:test-password \
  -X MKCOL "$test_dir/empty"
expect_status 207 --user einstein:test-password \
  -X PROPFIND -H 'Depth: 1' "$test_dir/empty/"
expect_status 201 --user einstein:test-password \
  -X MKCOL "$test_dir/deep"
expect_status 201 --user einstein:test-password \
  -X MKCOL "$test_dir/deep/level-two"
expect_status 201 --user einstein:test-password \
  -X MKCOL "$test_dir/deep/level-two/level-three"
printf 'localhomefs test\n' >/tmp/localhome-test.txt
expect_status 201 --user einstein:test-password \
  -T /tmp/localhome-test.txt "$test_dir/test.txt"
expect_status 204 --user einstein:test-password \
  -T /tmp/localhome-test.txt "$test_dir/test.txt"
expect_status 201 --user einstein:test-password \
  -T /tmp/localhome-test.txt \
  "$test_dir/deep/level-two/level-three/test.txt"
expect_status 207 --user einstein:test-password \
  -X PROPFIND -H 'Depth: 1' "$test_dir/"
expect_status 200 --user einstein:test-password "$test_dir/test.txt"
cmp /tmp/localhome-test.txt /tmp/dav-response
expect_status 200 --user einstein:test-password -I "$test_dir/test.txt"
grep -qi '^ETag:' /tmp/dav-response
grep -qi '^Last-Modified:' /tmp/dav-response

expect_status 207 --user einstein:test-password \
  -X PROPFIND -H 'Depth: 0' -H 'Content-Type: application/xml' \
  --data-binary @- "$test_dir/test.txt" <<'XML'
<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:"><d:prop><d:getetag/><d:getlastmodified/></d:prop></d:propfind>
XML
grep -q 'xmlns:d="DAV:"' /tmp/dav-response
grep -q '<d:multistatus' /tmp/dav-response

malformed_status=$(curl -sS -o /tmp/dav-response -w '%{http_code}' \
  --user einstein:test-password -X PROPFIND \
  -H 'Content-Type: application/xml' --data '<d:propfind' "$test_dir/test.txt")
[[ "$malformed_status" == 400 ]] || {
  echo "malformed PROPFIND XML: expected HTTP 400, got $malformed_status" >&2
  exit 1
}
malformed_status=$(curl -sS -o /tmp/dav-response -w '%{http_code}' \
  --user einstein:test-password -X PROPPATCH \
  -H 'Content-Type: application/xml' --data '<d:propertyupdate' "$test_dir/test.txt")
[[ "$malformed_status" == 400 ]] || {
  echo "malformed PROPPATCH XML: expected HTTP 400, got $malformed_status" >&2
  exit 1
}
printf 'xml-secret-marker\n' >/tmp/xml-secret-marker
xxe_status=$(curl -sS -o /tmp/dav-response -w '%{http_code}' \
  --user einstein:test-password -X PROPFIND \
  -H 'Content-Type: application/xml' \
  --data '<?xml version="1.0"?><!DOCTYPE x [<!ENTITY e SYSTEM "file:///tmp/xml-secret-marker">]><d:propfind xmlns:d="DAV:"><d:prop><d:displayname>&e;</d:displayname></d:prop></d:propfind>' \
  "$test_dir/test.txt")
case "$xxe_status" in
  2*) ;;
  4*) ;;
  *) echo "unsafe XXE response status: HTTP $xxe_status" >&2; exit 1 ;;
esac
if grep -q 'xml-secret-marker' /tmp/dav-response; then
  echo "PROPFIND exposed an external XML entity" >&2
  exit 1
fi

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

printf 'space and 日本語 filename\n' >/tmp/special-name.txt
special_name="$DAV_URL/ci-check-$test_id/space%20and%20%E6%97%A5%E6%9C%AC%E8%AA%9E.txt"
expect_status 201 --user einstein:test-password -T /tmp/special-name.txt "$special_name"
expect_status 200 --user einstein:test-password "$special_name"
cmp /tmp/special-name.txt /tmp/dav-response

dd if=/dev/zero of=/tmp/large-upload.bin bs=1M count=8 status=none
expect_status 201 --user einstein:test-password \
  -T /tmp/large-upload.bin "$test_dir/large-upload.bin"
expect_status 200 --user einstein:test-password "$test_dir/large-upload.bin"
cmp /tmp/large-upload.bin /tmp/dav-response

for n in $(seq 1 50); do
  printf 'bulk file %s\n' "$n" >/tmp/bulk-file.txt
  expect_status 201 --user einstein:test-password \
    -T /tmp/bulk-file.txt "$test_dir/bulk-$n.txt"
done
expect_status 207 --user einstein:test-password \
  -X PROPFIND -H 'Depth: 1' "$test_dir/"
bulk_count=$(grep -o 'd:response' /tmp/dav-response | wc -l)
if [[ "$bulk_count" -lt 50 ]]; then
  echo "bulk PROPFIND returned only $bulk_count response elements" >&2
  tail -30 /tmp/dav-response >&2
  exit 1
fi

pids=()
for n in $(seq 1 12); do
  (
    printf 'parallel file %s\n' "$n" >/tmp/parallel-file-"$n".txt
    curl -sS -o /tmp/parallel-response-"$n" -w '%{http_code}' \
      --user einstein:test-password -T /tmp/parallel-file-"$n".txt \
      "$test_dir/parallel-$n.txt" > /tmp/parallel-status-"$n"
  ) &
  pids+=("$!")
done
for pid in "${pids[@]}"; do
  wait "$pid"
done
for n in $(seq 1 12); do
  [[ "$(cat /tmp/parallel-status-"$n")" == 201 ]] || {
    echo "parallel upload $n did not return HTTP 201" >&2
    exit 1
  }
done

etag_before=$(curl -sSI --user einstein:test-password "$test_dir/test.txt" |
  awk 'tolower($1) == "etag:" { print $2; exit }')
expect_status 204 --user einstein:test-password -T /tmp/localhome-test.txt \
  "$test_dir/test.txt"
etag_after=$(curl -sSI --user einstein:test-password "$test_dir/test.txt" |
  awk 'tolower($1) == "etag:" { print $2; exit }')
if [[ -n "$etag_before" && -n "$etag_after" && "$etag_before" == "$etag_after" ]]; then
  echo "ETag did not change after an overwrite" >&2
  exit 1
fi

expect_status 412 --user einstein:test-password \
  -H 'If-Match: "incorrect-etag"' -T /tmp/localhome-test.txt "$test_dir/test.txt"

expect_status 201 --user einstein:test-password -X COPY \
  -H "Destination: $DAV_URL/ci-copy-$test_id.txt" \
  -H 'Overwrite: F' "$test_dir/deep/level-two/level-three/test.txt"
expect_status 201 --user einstein:test-password -X MOVE \
  -H "Destination: $DAV_URL/ci-moved-$test_id.txt" \
  -H 'Overwrite: F' "$DAV_URL/ci-copy-$test_id.txt"
expect_status 412 --user einstein:test-password -X COPY \
  -H "Destination: $DAV_URL/ci-moved-$test_id.txt" \
  -H 'Overwrite: F' "$test_dir/test.txt"
expect_status 204 --user einstein:test-password -X COPY \
  -H "Destination: $DAV_URL/ci-moved-$test_id.txt" \
  -H 'Overwrite: T' "$test_dir/test.txt"
expect_status 200 --user einstein:test-password -I "$DAV_URL/ci-moved-$test_id.txt"

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
  share-create -grantee=marie -idp=http://localhost:20080 -role=viewer \
  "/localfs/$share_dir/shared.txt" \
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
recipient_share_path="$DAV_URL/MyShares/shared.txt"
expect_status 200 --user marie:test-password "$recipient_share_path"
cmp /tmp/shared-test.txt /tmp/dav-response
expect_status 403 --user marie:test-password -T /tmp/localhome-test.txt \
  "$recipient_share_path"
expect_status 200 --user einstein:test-password \
  "$DAV_URL/$share_dir/shared.txt"
cmp /tmp/shared-test.txt /tmp/dav-response
owner_shares=$(./cmd/reva/reva -insecure -host=localhost:9142 \
  -token-file="$owner_token" share-list)
grep -q 'marie' <<<"$owner_shares"
./cmd/reva/reva -insecure -host=localhost:9142 -token-file="$recipient_token" \
  share-update-received -state=rejected "$received_share_id"
expect_status 404 --user marie:test-password "$recipient_share_path"

printf 'editor permission test\n' >/tmp/editor-test.txt
expect_status 201 --user einstein:test-password \
  -T /tmp/editor-test.txt "$DAV_URL/$share_dir/editor.txt"
./cmd/reva/reva -insecure -host=localhost:9142 -token-file="$owner_token" \
  share-create -grantee=marie -idp=http://localhost:20080 -role=editor \
  "/localfs/$share_dir/editor.txt" \
  >/tmp/editor-share.txt
grep -q 'initiate_file_upload:true' /tmp/editor-share.txt
grep -q 'create_container:true' /tmp/editor-share.txt
received_shares=$(./cmd/reva/reva -insecure -host=localhost:9142 \
  -token-file="$recipient_token" share-list-received)
editor_share_id=$(printf '%s\n' "$received_shares" |
  awk -F '|' -v previous="$received_share_id" \
    '/GRANTEE_TYPE_USER/ { id=$2; gsub(/^[[:space:]]+|[[:space:]]+$/, "", id); if (id != previous) { print id; exit } }')
if [[ -z "$editor_share_id" ]]; then
  echo "Marie did not receive Einstein's editor share" >&2
  exit 1
fi
./cmd/reva/reva -insecure -host=localhost:9142 -token-file="$recipient_token" \
  share-update-received -state=accepted "$editor_share_id"
editor_share_path="$DAV_URL/MyShares/editor.txt"
editor_owner_path="$DAV_URL/$share_dir/editor.txt"
expect_status 200 --user marie:test-password "$editor_share_path"
cmp /tmp/editor-test.txt /tmp/dav-response
printf 'edited by Marie\n' >/tmp/marie-edited.txt
expect_status 204 --user marie:test-password -T /tmp/marie-edited.txt \
  "$editor_share_path"
expect_status 200 --user einstein:test-password \
  "$DAV_URL/$share_dir/editor.txt"
cmp /tmp/marie-edited.txt /tmp/dav-response
expect_status 204 --user einstein:test-password -X DELETE "$test_dir/"

lock_info='<?xml version="1.0" encoding="utf-8"?>
<d:lockinfo xmlns:d="DAV:"><d:lockscope><d:exclusive/></d:lockscope>
<d:locktype><d:write/></d:locktype></d:lockinfo>'
shared_lock_info='<?xml version="1.0" encoding="utf-8"?>
<d:lockinfo xmlns:d="DAV:"><d:lockscope><d:shared/></d:lockscope>
<d:locktype><d:write/></d:locktype></d:lockinfo>'
shared_lock_status=$(curl -sS -o /tmp/shared-lock-response -D /tmp/shared-lock-headers \
  -w '%{http_code}' --user einstein:test-password \
  -X LOCK -H 'Depth: 0' -H 'Content-Type: application/xml' \
  --data "$shared_lock_info" "$editor_owner_path")
[[ "$shared_lock_status" == 200 ]] || {
  echo "first shared LOCK returned HTTP $shared_lock_status, expected 200" >&2
  cat /tmp/shared-lock-response >&2
  exit 1
}
shared_token_one=$(awk 'tolower($1) == "lock-token:" { gsub(/[<>]/, "", $2); sub(/\r$/, "", $2); print $2; exit }' /tmp/shared-lock-headers)
[[ -n "$shared_token_one" ]] || { echo "first shared LOCK did not return a token" >&2; exit 1; }
shared_lock_status=$(curl -sS -o /tmp/shared-lock-response -D /tmp/shared-lock-headers \
  -w '%{http_code}' --user marie:test-password \
  -X LOCK -H 'Depth: 0' -H 'Content-Type: application/xml' \
  --data "$shared_lock_info" "$editor_share_path")
[[ "$shared_lock_status" == 200 ]] || {
  echo "second shared LOCK returned HTTP $shared_lock_status, expected 200" >&2
  cat /tmp/shared-lock-response >&2
  exit 1
}
shared_token_two=$(awk 'tolower($1) == "lock-token:" { gsub(/[<>]/, "", $2); sub(/\r$/, "", $2); print $2; exit }' /tmp/shared-lock-headers)
[[ -n "$shared_token_two" && "$shared_token_one" != "$shared_token_two" ]] || {
  echo "shared LOCKs did not receive distinct tokens" >&2
  exit 1
}
exclusive_while_shared=$(curl -sS -o /tmp/lock-response -w '%{http_code}' \
  --user einstein:test-password -X LOCK -H 'Depth: 0' \
  -H 'Content-Type: application/xml' --data "$lock_info" \
  "$editor_owner_path")
[[ "$exclusive_while_shared" == 423 ]] || {
  echo "exclusive LOCK with active shared locks returned HTTP $exclusive_while_shared, expected 423" >&2
  exit 1
}
expect_status 423 --user einstein:test-password -T /tmp/localhome-test.txt \
  -H "If: (<$shared_token_one>)" "$editor_owner_path"
expect_status 204 --user einstein:test-password -T /tmp/localhome-test.txt \
  -H "If: (<$shared_token_one> <$shared_token_two>)" \
  "$editor_owner_path"
expect_status 204 --user marie:test-password -X UNLOCK \
  -H "Lock-Token: <$shared_token_two>" "$editor_share_path"
expect_status 204 --user einstein:test-password -T /tmp/localhome-test.txt \
  -H "If: (<$shared_token_one>)" "$editor_owner_path"
expect_status 204 --user einstein:test-password -X UNLOCK \
  -H "Lock-Token: <$shared_token_one>" "$editor_owner_path"

lock_status=$(curl -sS -o /tmp/lock-response -D /tmp/lock-headers \
  -w '%{http_code}' --user einstein:test-password \
  -X LOCK -H 'Depth: 0' -H 'Timeout: Second-30' \
  -H 'Content-Type: application/xml' --data "$lock_info" \
  "$editor_owner_path")
[[ "$lock_status" == 200 ]] || {
  echo "LOCK returned HTTP $lock_status" >&2
  cat /tmp/lock-response >&2
  exit 1
}
lock_token=$(awk 'tolower($1) == "lock-token:" { gsub(/[<>]/, "", $2); sub(/\r$/, "", $2); print $2; exit }' /tmp/lock-headers)
[[ -n "$lock_token" ]] || { echo "LOCK did not return Lock-Token" >&2; exit 1; }
refresh_status=$(curl -sS -o /tmp/lock-response -D /tmp/lock-headers \
  -w '%{http_code}' --user einstein:test-password \
  -X LOCK -H "If: (<$lock_token>)" -H 'Timeout: Second-30' \
  "$editor_owner_path")
[[ "$refresh_status" == 200 ]] || {
  echo "LOCK refresh returned HTTP $refresh_status, expected 200" >&2
  exit 1
}
second_lock_token=$(curl -sS -o /tmp/lock-response -D /tmp/lock-headers \
  -w '%{http_code}' --user einstein:test-password \
  -X LOCK -H 'Depth: 0' -H 'Timeout: Second-30' \
  -H 'Content-Type: application/xml' --data "$lock_info" \
  "$editor_owner_path")
[[ "$second_lock_token" == 423 ]] || {
  echo "second LOCK returned HTTP $second_lock_token, expected 423" >&2
  exit 1
}
expect_status 423 --user einstein:test-password -T /tmp/localhome-test.txt \
  "$editor_owner_path"
expect_status 204 --user einstein:test-password \
  -H "If: (<$lock_token>)" -T /tmp/localhome-test.txt \
  "$editor_owner_path"
wrong_unlock_status=$(curl -sS -o /tmp/unlock-response -w '%{http_code}' \
  --user einstein:test-password -X UNLOCK \
  -H 'Lock-Token: <opaquelocktoken:wrong-token>' \
  "$editor_owner_path")
[[ "$wrong_unlock_status" == 409 ]] || {
  echo "UNLOCK with wrong token returned HTTP $wrong_unlock_status, expected 409" >&2
  exit 1
}
unlock_status=$(curl -sS -o /tmp/unlock-response -w '%{http_code}' \
  --user einstein:test-password -X UNLOCK -H "Lock-Token: <$lock_token>" \
  "$editor_owner_path")
[[ "$unlock_status" == 204 ]] || {
  echo "UNLOCK returned HTTP $unlock_status, expected 204" >&2
  cat /tmp/unlock-response >&2
  exit 1
}
expect_status 204 --user einstein:test-password \
  -T /tmp/localhome-test.txt "$editor_owner_path"

lock_status=$(curl -sS -o /tmp/lock-response -D /tmp/lock-headers \
  -w '%{http_code}' --user einstein:test-password \
  -X LOCK -H 'Depth: 0' -H 'Timeout: Second-300' \
  -H 'Content-Type: application/xml' --data "$lock_info" \
  "$editor_owner_path")
[[ "$lock_status" == 200 ]] || {
  echo "LOCK before restart returned HTTP $lock_status, expected 200" >&2
  exit 1
}
persistent_lock_token=$(awk 'tolower($1) == "lock-token:" { gsub(/[<>]/, "", $2); sub(/\r$/, "", $2); print $2; exit }' /tmp/lock-headers)
[[ -n "$persistent_lock_token" ]] || { echo "LOCK before restart did not return a token" >&2; exit 1; }
restart_revad
expect_status 200 --user einstein:test-password "$editor_owner_path"
expect_status 200 --user einstein:test-password \
  -X LOCK -H "If: (<$persistent_lock_token>)" -H 'Timeout: Second-300' \
  "$editor_owner_path"
expect_status 204 --user einstein:test-password -X UNLOCK \
  -H "Lock-Token: <$persistent_lock_token>" "$editor_owner_path"

sed 's|loginfilter = "(uid={{login}})"|loginfilter = "(uid=disabled-{{login}})"|' \
  "$runtime_config" >/tmp/revad-config-changed.toml
grep -q 'loginfilter = "(uid=disabled-{{login}})"' /tmp/revad-config-changed.toml || {
  echo "failed to prepare changed LDAP login configuration" >&2
  exit 1
}
mv /tmp/revad-config-changed.toml "$runtime_config"
restart_revad
expect_status 401 --user einstein:test-password \
  -X PROPFIND -H 'Depth: 0' "$DAV_URL/"
sed 's|loginfilter = "(uid=disabled-{{login}})"|loginfilter = "(uid={{login}})"|' \
  "$runtime_config" >/tmp/revad-config-restored.toml
mv /tmp/revad-config-restored.toml "$runtime_config"
restart_revad
expect_status 207 --user einstein:test-password \
  -X PROPFIND -H 'Depth: 0' "$DAV_URL/"

expect_status 200 "$METRICS_URL"
grep -q '^go_goroutines ' /tmp/dav-response || {
  echo "Prometheus endpoint did not expose Go runtime metrics" >&2
  exit 1
}
expect_status 404 --user einstein:test-password \
  "http://127.0.0.1:38001/unconfigured-$test_id"
# 404リクエストがアクセスログに記録されるまで待つ（最大30秒）
# ログ行にはANSIカラーエスケープが混入するため、sedで除去してからgrepする
found=0
for ((i = 0; i < 60; i++)); do
  clean=$(sed "s/\x1b\[[0-9;]*m//g" /tmp/revad.log 2>/dev/null || true)
  hit=$(printf '%s\n' "$clean" | grep "unconfigured-$test_id" | grep "status=404" || true)
  if [[ -n "$hit" ]]; then
    found=1
    break
  fi
  sleep 0.5
done
if [[ "$found" -ne 1 ]]; then
  echo "404 request was not recorded with its status in revad logs" >&2
  tail -80 /tmp/revad.log >&2
  exit 1
fi
echo "404 access-log check OK"
if (( SOAK_SECONDS > 0 )); then
  echo "Starting $SOAK_SECONDS-second WebDAV soak test"
  for ((elapsed = 0; elapsed < SOAK_SECONDS; elapsed++)); do
    soak_status=$(curl -sS -o /tmp/soak-response -w '%{http_code}' \
      --max-time 10 --user einstein:test-password "$editor_owner_path")
    [[ "$soak_status" == 200 ]] || {
      echo "soak request at ${elapsed}s returned HTTP $soak_status, expected 200" >&2
      cat /tmp/soak-response >&2
      exit 1
    }
    if (( elapsed % 60 == 0 )); then
      kill -0 "$revad_pid" || { echo "revad exited during soak test" >&2; exit 1; }
      echo "soak test progress: ${elapsed}/${SOAK_SECONDS}s"
    fi
    sleep 1
  done
fi

expect_status 204 --user einstein:test-password -X DELETE "$editor_owner_path"

kill "$slapd_pid"
wait "$slapd_pid" || true
slapd_pid=""
ldap_outage_status=$(curl -sS -o /tmp/dav-response -w '%{http_code}' \
  --user not-registered:test-password -X PROPFIND -H 'Depth: 0' "$DAV_URL/" || true)
case "$ldap_outage_status" in
  2*) echo "WebDAV allowed an authenticated request while LDAP was stopped" >&2; exit 1 ;;
esac
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
    cat /tmp/slapd.log
    echo "slapd did not recover after restart" >&2
    exit 1
  fi
  sleep 1
done
expect_status 207 --user einstein:test-password \
  -X PROPFIND -H 'Depth: 0' "$DAV_URL/"

echo "ARMv7 Bookworm LDAP/LDAPS, WebDAV edge cases, PROPPATCH, concurrency, and share tests passed"
