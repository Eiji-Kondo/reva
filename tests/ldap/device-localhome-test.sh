#!/usr/bin/env bash
set -Eeuo pipefail

REVA_DIR="${REVA_DIR:-$HOME/reva}"
REVAD="${REVAD:-$REVA_DIR/revad}"
REVA="${REVA:-$REVA_DIR/reva}"
CONFIG="${CONFIG:-$REVA_DIR/revad-test.toml}"
PASSWORD_FILE="${PASSWORD_FILE:-$REVA_DIR/test-passwords}"
DAV_URL="${DAV_URL:-http://127.0.0.1:38001/remote.php/webdav}"
GRPC_HOST="${GRPC_HOST:-127.0.0.1:9142}"
LDAP_IDP="${LDAP_IDP:-http://localhost:20080}"

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

for dependency in curl cmp awk ss stat python3; do
  command -v "$dependency" >/dev/null 2>&1 || die "Required command not found: $dependency"
done

[[ -x "$REVAD" ]] || die "revad binary is missing or not executable: $REVAD"
[[ -x "$REVA" ]] || die "reva CLI binary is missing or not executable: $REVA"
[[ -r "$CONFIG" ]] || die "Configuration file is not readable: $CONFIG"

"$REVAD" -t -c "$CONFIG"

if ss -ltnH | awk '$4 ~ /:9142$/ || $4 ~ /:38001$/ { found = 1 } END { exit !found }'; then
  die "Ports 9142 or 38001 are already in use; refusing to interrupt another service"
fi

install -d -m 0700 "$REVA_DIR/test-storage"
if [[ ! -e "$PASSWORD_FILE" ]]; then
  printf 'First run: securely save the two LDAP test-user passwords for later runs.\n'
  read -r -s -p "Einstein test password: " einstein_password
  printf '\n'
  read -r -s -p "Marie test password: " marie_password
  printf '\n'
  [[ -n "$einstein_password" && -n "$marie_password" ]] ||
    die "Test passwords must not be empty"
  passwords_tmp="$(mktemp "$REVA_DIR/.test-passwords.XXXXXXXX")"
  chmod 0600 "$passwords_tmp"
  printf '%s\n%s\n' "$einstein_password" "$marie_password" >"$passwords_tmp"
  mv -- "$passwords_tmp" "$PASSWORD_FILE"
  unset einstein_password marie_password
fi

[[ -f "$PASSWORD_FILE" && ! -L "$PASSWORD_FILE" ]] ||
  die "Password file must be a regular, non-symlink file: $PASSWORD_FILE"
[[ "$(stat -c %u "$PASSWORD_FILE")" == "$(id -u)" ]] ||
  die "Password file must be owned by the current user: $PASSWORD_FILE"
chmod 0600 "$PASSWORD_FILE"
mapfile -t saved_passwords <"$PASSWORD_FILE"
[[ "${#saved_passwords[@]}" == 2 &&
  -n "${saved_passwords[0]}" && -n "${saved_passwords[1]}" ]] ||
  die "Password file must contain exactly two non-empty lines"

tmpdir="$(mktemp -d "$REVA_DIR/.device-test.XXXXXXXX")"
chmod 0700 "$tmpdir"
pty_login="$tmpdir/pty-login.py"
cat >"$pty_login" <<'PY'
import errno
import os
import pty
import select
import signal
import sys
import termios
import time


def fail(message):
    print(message, file=sys.stderr)
    raise SystemExit(1)


credentials = sys.stdin.buffer.read().splitlines()
if len(credentials) != 2 or not all(credentials):
    fail("CLI login expected a username and a non-empty password on private stdin")

command = sys.argv[1:]
if not command:
    fail("CLI command was not provided")

pid, fd = pty.fork()
if pid == 0:
    try:
        os.execvp(command[0], command)
    except OSError as exc:
        print(f"Could not start reva CLI: {exc}", file=sys.stderr)
        os._exit(127)

output = bytearray()
username_sent = False
password_sent = False
deadline = time.monotonic() + 90
child_status = None

while time.monotonic() < deadline:
    try:
        ready, _, _ = select.select([fd], [], [], 0.1)
        if ready:
            chunk = os.read(fd, 4096)
            if not chunk:
                _, child_status = os.waitpid(pid, 0)
                break
            output.extend(chunk)
    except OSError as exc:
        if exc.errno != errno.EIO:
            raise
        _, child_status = os.waitpid(pid, 0)
        break

    lowered = output.lower()
    if not username_sent and b"username:" in lowered:
        os.write(fd, credentials[0] + b"\n")
        username_sent = True

    if username_sent and not password_sent and b"password:" in lowered:
        try:
            echo_enabled = bool(termios.tcgetattr(fd)[3] & termios.ECHO)
        except termios.error:
            echo_enabled = True
        if not echo_enabled:
            os.write(fd, credentials[1] + b"\n")
            password_sent = True

    waited, status = os.waitpid(pid, os.WNOHANG)
    if waited == pid:
        child_status = status
        break

if child_status is None:
    try:
        os.kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    _, child_status = os.waitpid(pid, 0)
    safe_output = output.replace(credentials[1], b"<redacted>")
    if safe_output:
        sys.stderr.buffer.write(safe_output)
        sys.stderr.buffer.flush()
    fail("CLI login timed out before completing")

try:
    while True:
        chunk = os.read(fd, 4096)
        if not chunk:
            break
        output.extend(chunk)
except OSError as exc:
    if exc.errno != errno.EIO:
        raise

sys.stdout.buffer.write(output)
sys.stdout.buffer.flush()
if not username_sent or not password_sent:
    fail("CLI login did not reach both credential prompts")
if not os.WIFEXITED(child_status) or os.WEXITSTATUS(child_status) != 0:
    fail("reva CLI login failed")
PY
log_file="$REVA_DIR/revad-test.log"
revad_pid=""
owner_token="$tmpdir/einstein.token"
recipient_token="$tmpdir/marie.token"
viewer_share_id=""
editor_share_id=""
test_id="device-test-$(date +%s)-$$"
test_dir="/localfs/$test_id"
viewer_file="viewer-$test_id.txt"
editor_file="editor-$test_id.txt"
operations_dir="$DAV_URL/$test_id/operations"
created_test_dir=0
einstein_password="${saved_passwords[0]}"
marie_password="${saved_passwords[1]}"
unset saved_passwords

cleanup() {
  local status=$?
  trap - EXIT

  if [[ -n "$owner_token" && -x "$REVA" ]]; then
    if [[ -n "$viewer_share_id" ]]; then
      "$REVA" -insecure -host="$GRPC_HOST" -token-file="$owner_token" \
        share-remove "$viewer_share_id" >/dev/null 2>&1 || true
    fi
    if [[ -n "$editor_share_id" ]]; then
      "$REVA" -insecure -host="$GRPC_HOST" -token-file="$owner_token" \
        share-remove "$editor_share_id" >/dev/null 2>&1 || true
    fi
    if (( created_test_dir )); then
      "$REVA" -insecure -host="$GRPC_HOST" -token-file="$owner_token" \
        rm "$test_dir/$viewer_file" >/dev/null 2>&1 || true
      "$REVA" -insecure -host="$GRPC_HOST" -token-file="$owner_token" \
        rm "$test_dir/$editor_file" >/dev/null 2>&1 || true
      for file in large.bin source.txt copy.txt moved.txt parallel-{1..4}.txt; do
        "$REVA" -insecure -host="$GRPC_HOST" -token-file="$owner_token" \
          rm "$test_dir/operations/$file" >/dev/null 2>&1 || true
      done
      "$REVA" -insecure -host="$GRPC_HOST" -token-file="$owner_token" \
        rm "$test_dir/operations" >/dev/null 2>&1 || true
      "$REVA" -insecure -host="$GRPC_HOST" -token-file="$owner_token" \
        rm "$test_dir" >/dev/null 2>&1 || true
    fi
  fi

  if [[ -n "$revad_pid" ]] && kill -0 "$revad_pid" 2>/dev/null; then
    kill "$revad_pid"
    wait "$revad_pid" || true
  fi
  rm -rf -- "$tmpdir"

  if (( status != 0 )); then
    printf 'Test failed. revad log: %s\n' "$log_file" >&2
  fi
  exit "$status"
}
trap cleanup EXIT

printf 'Starting test revad with %s\n' "$CONFIG"
"$REVAD" -c "$CONFIG" >>"$log_file" 2>&1 &
revad_pid=$!

ready=0
for _ in $(seq 1 60); do
  if ! kill -0 "$revad_pid" 2>/dev/null; then
    cat "$log_file" >&2
    die "revad exited before the WebDAV endpoint became ready"
  fi
  status_code="$(curl -sS -o "$tmpdir/ready" -w '%{http_code}' \
    --max-time 2 -X OPTIONS "$DAV_URL/" 2>/dev/null || true)"
  if [[ "$status_code" != "000" ]]; then
    ready=1
    break
  fi
  sleep 1
done
(( ready )) || die "Timed out waiting for WebDAV; see $log_file"

curl_config() {
  local path="$1"
  local username="$2"
  local password="$3"
  local escaped
  [[ "$password" != *$'\n'* && "$password" != *$'\r'* ]] ||
    die "Passwords cannot contain line breaks"
  escaped="${password//\\/\\\\}"
  escaped="${escaped//\"/\\\"}"
  (umask 077; printf 'user = "%s:%s"\n' "$username" "$escaped" >"$path")
}

curl_config "$tmpdir/einstein.curl" einstein "$einstein_password"
curl_config "$tmpdir/marie.curl" marie "$marie_password"

expect_status() {
  local expected="$1"
  shift
  local actual
  actual="$(curl -sS -o "$tmpdir/response" -w '%{http_code}' \
    --max-time 30 "$@")" || die "curl request failed: $*"
  if [[ "$actual" != "$expected" ]]; then
    printf 'Expected HTTP %s, got %s\n' "$expected" "$actual" >&2
    cat "$tmpdir/response" >&2
    return 1
  fi
}

printf 'Checking LDAP authentication and WebDAV access...\n'
expect_status 401 -X PROPFIND -H 'Depth: 0' "$DAV_URL/"
expect_status 207 --config "$tmpdir/einstein.curl" \
  -X PROPFIND -H 'Depth: 0' "$DAV_URL/"
expect_status 207 --config "$tmpdir/marie.curl" \
  -X PROPFIND -H 'Depth: 0' "$DAV_URL/"

printf 'Logging Einstein in to the reva CLI...\n'
login_cli() {
  local username="$1"
  local login_secret="$2"
  local token="$3"
  local -a command=(
    "$REVA" -insecure "-host=$GRPC_HOST" "-token-file=$token" login basic
  )
  printf '%s\n%s\n' "$username" "$login_secret" |
    python3 "$pty_login" "${command[@]}"
}
login_cli einstein "$einstein_password" "$owner_token"
printf 'Logging Marie in to the reva CLI...\n'
login_cli marie "$marie_password" "$recipient_token"
unset einstein_password marie_password

printf 'Checking viewer and editor shares...\n'
"$REVA" -insecure -host="$GRPC_HOST" -token-file="$owner_token" mkdir "$test_dir"
created_test_dir=1
printf 'viewer share test\n' >"$tmpdir/$viewer_file"
printf 'editor share test\n' >"$tmpdir/$editor_file"
expect_status 201 --config "$tmpdir/einstein.curl" \
  -T "$tmpdir/$viewer_file" "$DAV_URL/$test_id/$viewer_file"
expect_status 201 --config "$tmpdir/einstein.curl" \
  -T "$tmpdir/$editor_file" "$DAV_URL/$test_id/$editor_file"

cat >"$tmpdir/proppatch-set.xml" <<'XML'
<?xml version="1.0" encoding="utf-8"?>
<d:propertyupdate xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
  <d:set><d:prop><oc:favorite>1</oc:favorite></d:prop></d:set>
</d:propertyupdate>
XML
cat >"$tmpdir/proppatch-get.xml" <<'XML'
<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
  <d:prop><oc:favorite/></d:prop>
</d:propfind>
XML
expect_status 200 --config "$tmpdir/einstein.curl" \
  -X PROPPATCH -H 'Content-Type: application/xml; charset=utf-8' \
  --data-binary "@$tmpdir/proppatch-set.xml" \
  "$DAV_URL/$test_id/$viewer_file"
expect_status 207 --config "$tmpdir/einstein.curl" \
  -X PROPFIND -H 'Depth: 0' \
  -H 'Content-Type: application/xml; charset=utf-8' \
  --data-binary "@$tmpdir/proppatch-get.xml" \
  "$DAV_URL/$test_id/$viewer_file"
grep -q '<oc:favorite>1</oc:favorite>' "$tmpdir/response" ||
  die "PROPPATCH favorite property was not returned by PROPFIND"

viewer_result="$("$REVA" -insecure -host="$GRPC_HOST" \
  -token-file="$owner_token" share-create \
  -grantee=marie -idp="$LDAP_IDP" -role=viewer \
  "$test_dir/$viewer_file")"
printf '%s\n' "$viewer_result" | grep -q 'initiate_file_download:true' ||
  die "Viewer share is missing download permission"
if printf '%s\n' "$viewer_result" | grep -q 'initiate_file_upload:true'; then
  die "Viewer share unexpectedly includes upload permission"
fi
received="$("$REVA" -insecure -host="$GRPC_HOST" \
  -token-file="$recipient_token" share-list-received)"
viewer_share_id="$(printf '%s\n' "$received" | awk -F '|' -v file="$viewer_file" \
  'index($0, file) { id=$2; gsub(/^[[:space:]]+|[[:space:]]+$/, "", id); print id; exit }')"
[[ -n "$viewer_share_id" ]] || die "Marie did not receive the viewer share"
"$REVA" -insecure -host="$GRPC_HOST" -token-file="$recipient_token" \
  share-update-received -state=accepted "$viewer_share_id"

expect_status 200 --config "$tmpdir/marie.curl" "$DAV_URL/MyShares/$viewer_file"
cmp "$tmpdir/$viewer_file" "$tmpdir/response"
expect_status 403 --config "$tmpdir/marie.curl" \
  -T "$tmpdir/$viewer_file" "$DAV_URL/MyShares/$viewer_file"

editor_result="$("$REVA" -insecure -host="$GRPC_HOST" \
  -token-file="$owner_token" share-create \
  -grantee=marie -idp="$LDAP_IDP" -role=editor \
  "$test_dir/$editor_file")"
printf '%s\n' "$editor_result" | grep -q 'initiate_file_upload:true' ||
  die "Editor share is missing upload permission"
received="$("$REVA" -insecure -host="$GRPC_HOST" \
  -token-file="$recipient_token" share-list-received)"
editor_share_id="$(printf '%s\n' "$received" | awk -F '|' -v file="$editor_file" \
  'index($0, file) { id=$2; gsub(/^[[:space:]]+|[[:space:]]+$/, "", id); print id; exit }')"
[[ -n "$editor_share_id" ]] || die "Marie did not receive the editor share"
"$REVA" -insecure -host="$GRPC_HOST" -token-file="$recipient_token" \
  share-update-received -state=accepted "$editor_share_id"

printf 'edited by Marie\n' >"$tmpdir/edited.txt"
expect_status 204 --config "$tmpdir/marie.curl" -T "$tmpdir/edited.txt" \
  "$DAV_URL/MyShares/$editor_file"
expect_status 200 --config "$tmpdir/einstein.curl" \
  "$DAV_URL/$test_id/$editor_file"
cmp "$tmpdir/edited.txt" "$tmpdir/response"

printf 'Checking device COPY/MOVE/DELETE and large/concurrent operations...\n'
expect_status 201 --config "$tmpdir/einstein.curl" -X MKCOL "$operations_dir"
dd if=/dev/zero of="$tmpdir/large.bin" bs=1M count=8 status=none
expect_status 201 --config "$tmpdir/einstein.curl" -T "$tmpdir/large.bin" \
  "$operations_dir/large.bin"
expect_status 200 --config "$tmpdir/einstein.curl" "$operations_dir/large.bin"
cmp "$tmpdir/large.bin" "$tmpdir/response"
expect_status 204 --config "$tmpdir/einstein.curl" -X DELETE \
  "$operations_dir/large.bin"

printf 'device COPY/MOVE source\n' >"$tmpdir/source.txt"
expect_status 201 --config "$tmpdir/einstein.curl" -T "$tmpdir/source.txt" \
  "$operations_dir/source.txt"
expect_status 201 --config "$tmpdir/einstein.curl" -X COPY \
  -H "Destination: $operations_dir/copy.txt" \
  -H 'Overwrite: F' "$operations_dir/source.txt"
expect_status 201 --config "$tmpdir/einstein.curl" -X MOVE \
  -H "Destination: $operations_dir/moved.txt" \
  -H 'Overwrite: F' "$operations_dir/copy.txt"
expect_status 200 --config "$tmpdir/einstein.curl" "$operations_dir/moved.txt"
cmp "$tmpdir/source.txt" "$tmpdir/response"
expect_status 204 --config "$tmpdir/einstein.curl" -X DELETE \
  "$operations_dir/source.txt"
expect_status 204 --config "$tmpdir/einstein.curl" -X DELETE \
  "$operations_dir/moved.txt"

parallel_pids=()
for number in 1 2 3 4; do
  printf 'parallel device upload %s\n' "$number" >"$tmpdir/parallel-$number.txt"
  (
    curl -sS -o "$tmpdir/parallel-response-$number" \
      -w '%{http_code}' --config "$tmpdir/einstein.curl" \
      -T "$tmpdir/parallel-$number.txt" \
      "$operations_dir/parallel-$number.txt" \
      >"$tmpdir/parallel-status-$number"
  ) &
  parallel_pids+=("$!")
done
for pid in "${parallel_pids[@]}"; do
  wait "$pid"
done
for number in 1 2 3 4; do
  [[ "$(cat "$tmpdir/parallel-status-$number")" == 201 ]] ||
    die "parallel device upload $number did not return HTTP 201"
  expect_status 204 --config "$tmpdir/einstein.curl" -X DELETE \
    "$operations_dir/parallel-$number.txt"
done
expect_status 204 --config "$tmpdir/einstein.curl" -X DELETE "$operations_dir/"

printf 'Checking shared and exclusive WebDAV locks on the device...\n'
lock_info='<?xml version="1.0" encoding="utf-8"?>
<d:lockinfo xmlns:d="DAV:"><d:lockscope><d:shared/></d:lockscope>
<d:locktype><d:write/></d:locktype></d:lockinfo>'
lock_status="$(curl -sS -o "$tmpdir/lock-response" -D "$tmpdir/lock-headers" \
  -w '%{http_code}' --config "$tmpdir/einstein.curl" \
  -X LOCK -H 'Depth: 0' -H 'Content-Type: application/xml' \
  --data "$lock_info" "$DAV_URL/$test_id/$editor_file")"
[[ "$lock_status" == 200 ]] || die "Einstein shared LOCK returned HTTP $lock_status"
shared_token_one="$(awk 'tolower($1) == "lock-token:" { gsub(/[<>]/, "", $2); sub(/\r$/, "", $2); print $2; exit }' "$tmpdir/lock-headers")"
[[ -n "$shared_token_one" ]] || die "Einstein shared LOCK did not return a token"
lock_status="$(curl -sS -o "$tmpdir/lock-response" -D "$tmpdir/lock-headers" \
  -w '%{http_code}' --config "$tmpdir/marie.curl" \
  -X LOCK -H 'Depth: 0' -H 'Content-Type: application/xml' \
  --data "$lock_info" "$DAV_URL/MyShares/$editor_file")"
[[ "$lock_status" == 200 ]] || die "Marie shared LOCK returned HTTP $lock_status"
shared_token_two="$(awk 'tolower($1) == "lock-token:" { gsub(/[<>]/, "", $2); sub(/\r$/, "", $2); print $2; exit }' "$tmpdir/lock-headers")"
[[ -n "$shared_token_two" && "$shared_token_one" != "$shared_token_two" ]] ||
  die "shared LOCKs did not receive distinct tokens"
expect_status 423 --config "$tmpdir/einstein.curl" -T "$tmpdir/edited.txt" \
  -H "If: (<$shared_token_one>)" "$DAV_URL/$test_id/$editor_file"
expect_status 204 --config "$tmpdir/einstein.curl" -T "$tmpdir/edited.txt" \
  -H "If: (<$shared_token_one> <$shared_token_two>)" \
  "$DAV_URL/$test_id/$editor_file"
expect_status 204 --config "$tmpdir/marie.curl" -X UNLOCK \
  -H "Lock-Token: <$shared_token_two>" "$DAV_URL/MyShares/$editor_file"
expect_status 204 --config "$tmpdir/einstein.curl" -T "$tmpdir/edited.txt" \
  -H "If: (<$shared_token_one>)" "$DAV_URL/$test_id/$editor_file"
expect_status 204 --config "$tmpdir/einstein.curl" -X UNLOCK \
  -H "Lock-Token: <$shared_token_one>" "$DAV_URL/$test_id/$editor_file"

lock_info='<?xml version="1.0" encoding="utf-8"?>
<d:lockinfo xmlns:d="DAV:"><d:lockscope><d:exclusive/></d:lockscope>
<d:locktype><d:write/></d:locktype></d:lockinfo>'
lock_status="$(curl -sS -o "$tmpdir/lock-response" -D "$tmpdir/lock-headers" \
  -w '%{http_code}' --config "$tmpdir/einstein.curl" \
  -X LOCK -H 'Depth: 0' -H 'Content-Type: application/xml' \
  --data "$lock_info" "$DAV_URL/$test_id/$editor_file")"
[[ "$lock_status" == 200 ]] || die "exclusive LOCK returned HTTP $lock_status"
exclusive_token="$(awk 'tolower($1) == "lock-token:" { gsub(/[<>]/, "", $2); sub(/\r$/, "", $2); print $2; exit }' "$tmpdir/lock-headers")"
[[ -n "$exclusive_token" ]] || die "exclusive LOCK did not return a token"
expect_status 423 --config "$tmpdir/einstein.curl" -T "$tmpdir/edited.txt" \
  "$DAV_URL/$test_id/$editor_file"
expect_status 204 --config "$tmpdir/einstein.curl" -T "$tmpdir/edited.txt" \
  -H "If: (<$exclusive_token>)" "$DAV_URL/$test_id/$editor_file"
expect_status 204 --config "$tmpdir/einstein.curl" -X UNLOCK \
  -H "Lock-Token: <$exclusive_token>" "$DAV_URL/$test_id/$editor_file"
expect_status 204 --config "$tmpdir/einstein.curl" -X DELETE \
  "$DAV_URL/$test_id/$editor_file"

printf 'All LDAP, WebDAV, share, lock, COPY/MOVE/DELETE, and large/concurrent device tests passed.\n'
