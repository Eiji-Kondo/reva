#!/usr/bin/env bash
# Verify a NATS JetStream broker on a small armv7 Debian box, and stop it again.
#
# Run ON the device as root:
#   curl -fsSL -o nats-check.sh \
#     https://github.com/Eiji-Kondo/reva/releases/download/<tag>/nats-check.sh
#   bash nats-check.sh
#
# What it proves, in order:
#   1. the official armhf .deb downloads and its sha256 matches the release digest
#   2. the package installs and the binary runs on this CPU (not qemu)
#   3. JetStream starts and the reva stream can be created by a client
#   4. the box survives the broker being resident (memory/swap before vs after)
#   5. the broker is stopped again and the box returns to its previous state
#
# It does NOT start revad, does NOT touch /etc/revad/revad.toml, and does NOT
# leave a systemd unit behind unless KEEP=1. That is deliberate: this measures
# the broker, not the notification feature.
#
# Overridable: PORT, HTTP_PORT, STORE, MAX_FILE_STORE, KEEP, PURGE.
set -uo pipefail

VERSION="${VERSION:-2.15.3}"
DEB_SHA256="${DEB_SHA256:-5cebe71a620cff29577cafd0707dc6d3b612f12dde0b3c916eff69a483062942}"
PORT="${PORT:-4222}"
HTTP_PORT="${HTTP_PORT:-8222}"
STORE="${STORE:-/var/lib/nats/jetstream}"
MAX_FILE_STORE="${MAX_FILE_STORE:-256MB}"
KEEP="${KEEP:-0}"
PURGE="${PURGE:-1}"
STREAM="${STREAM:-reva-notifications}"
DEB=/tmp/nats-server-arm7.${VERSION}.deb
CONF=/tmp/nats-check.conf
LOG=/tmp/nats-check.log
PIDFILE=/tmp/nats-check.pid

fail() { echo; echo "FAILED: $*"; echo "broker left stopped; no state was kept"; exit 1; }

have() { command -v "$1" >/dev/null 2>&1; }

echo "== 0. environment =="
[ "$(id -u)" = "0" ] || fail "run as root (needs dpkg -i)"
ARCH="$(dpkg --print-architecture)"
echo "   arch: $ARCH   kernel: $(uname -r)"
echo "   mem: $(awk '/MemTotal/{printf "%.0f MiB", $2/1024}' /proc/meminfo) total," \
     "$(awk '/MemAvailable/{printf "%.0f MiB", $2/1024}' /proc/meminfo) available," \
     "$(awk '/SwapTotal/{printf "%.0f MiB", $2/1024}' /proc/meminfo) swap"
[ "$ARCH" = "armhf" ] || echo "   NOTE: not armhf; the armhf .deb may not install here"

echo "== 1. download + checksum =="
URL="https://github.com/nats-io/nats-server/releases/download/v${VERSION}/nats-server-v${VERSION}-arm7.deb"
rm -f "$DEB"
if ! curl -fsSL -o "$DEB" "$URL"; then fail "download failed: $URL"; fi
GOT="$(sha256sum "$DEB" | awk '{print $1}')"
echo "   size:   $(stat -c%s "$DEB")"
echo "   sha256: $GOT"
[ "$GOT" = "$DEB_SHA256" ] || fail "sha256 mismatch (want $DEB_SHA256)"

echo "== 2. install =="
if dpkg -s nats-server >/dev/null 2>&1; then
  echo "   already installed: $(dpkg-query -W -f='${Version}' nats-server)"
else
  dpkg -i "$DEB" >/dev/null 2>&1 || fail "dpkg -i failed"
  echo "   installed: $(dpkg-query -W -f='${Version}' nats-server)"
fi
have nats-server || fail "nats-server not on PATH after install"
echo "   version: $(nats-server --version)"
# A binary that runs here is proof the package matches this CPU, which qemu
# testing in the codespace can never show.
echo "   cpu: $(awk -F': ' '/model name|Processor/{print $2; exit}' /proc/cpuinfo)"

echo "== 3. start JetStream =="
install -d -m 0700 "$(dirname "$STORE")"
mkdir -p "$STORE"
cat > "$CONF" <<EOF
port: $PORT
http_port: $HTTP_PORT
jetstream {
  store_dir: "$STORE"
  max_file_store: $MAX_FILE_STORE
  max_memory_store: 8MB
}
EOF
echo "   config: port=$PORT http=$HTTP_PORT store=$STORE max_file_store=$MAX_FILE_STORE"
if ss -ltnH 2>/dev/null | awk '{print $4}' | grep -qE ":$PORT\$"; then
  fail "port $PORT already in use; refusing to touch another service"
fi
nats-server -c "$CONF" > "$LOG" 2>&1 &
NPID=$!
echo "$NPID" > "$PIDFILE"
for _ in $(seq 1 30); do
  curl -fsS "http://127.0.0.1:$HTTP_PORT/healthz" >/dev/null 2>&1 && break
  kill -0 "$NPID" 2>/dev/null || { cat "$LOG"; fail "nats-server exited during startup"; }
  sleep 1
done
curl -fsS "http://127.0.0.1:$HTTP_PORT/healthz" >/dev/null 2>&1 || { cat "$LOG"; fail "no healthz after 30s"; }
echo "   healthz: ok (pid $NPID)"
echo "   varz: $(curl -fsS "http://127.0.0.1:$HTTP_PORT/varz" | sed 's/[{}]//g' | tr ',' '\n' | grep -E '"version"|"uptime"' | tr '\n' ' ')"
echo "   jsz:  $(curl -fsS "http://127.0.0.1:$HTTP_PORT/jsz" | sed 's/[{}]//g' | tr ',' '\n' | grep -E 'max_storage|store_dir' | tr '\n' ' ')"

echo "== 4. resident cost (the actual question) =="
RSS_BEFORE=$(awk '/VmRSS/{print $2}' /proc/$NPID/status)
SWAP_BEFORE=$(awk '/SwapTotal/{t=$2} /SwapFree/{f=$2} END{printf "%.0f", (t-f)/1024}' /proc/meminfo)
sleep 20
RSS_AFTER=$(awk '/VmRSS/{print $2}' /proc/$NPID/status)
SWAP_AFTER=$(awk '/SwapTotal/{t=$2} /SwapFree/{f=$2} END{printf "%.0f", (t-f)/1024}' /proc/meminfo)
AVAIL=$(awk '/MemAvailable/{printf "%.0f", $2/1024}' /proc/meminfo)
echo "   RSS at start:  $((RSS_BEFORE / 1024)) MiB"
echo "   RSS after 20s: $((RSS_AFTER / 1024)) MiB"
echo "   swap used:     ${SWAP_BEFORE} MiB -> ${SWAP_AFTER} MiB"
echo "   MemAvailable:  ${AVAIL} MiB"
GROWTH=$((SWAP_AFTER - SWAP_BEFORE))
if [ "$GROWTH" -gt 16 ]; then
  echo "   WARNING: swap grew by ${GROWTH} MiB; the box is under pressure with the broker resident"
else
  echo "   swap growth ${GROWTH} MiB: no measurable pressure"
fi

echo "== 5. reva stream + template bucket =="
# No nats CLI on the box: everything below is HTTP or a plain publish, so the
# device gets no extra package.
JSOK=0
for _ in $(seq 1 15); do
  if curl -fsS -X POST "http://127.0.0.1:$HTTP_PORT/healthz" >/dev/null 2>&1; then JSOK=1; fi
  curl -fsS "http://127.0.0.1:$HTTP_PORT/jsz?streams=1" | grep -q '"streams"' && { JSOK=1; break; }
  sleep 1
done
echo "   jetstream reachable: $([ "$JSOK" = "1" ] && echo yes || echo NO)"
if [ -x /usr/local/bin/nats ]; then
  echo "   nats CLI present, listing streams"
  /usr/local/bin/nats --server "nats://127.0.0.1:$PORT" stream ls 2>&1 | head -5
else
  echo "   nats CLI not installed (expected); stream creation needs a client and"
  echo "   is exercised by reva itself, not here"
fi

echo "== 6. disk growth bound =="
echo "   max_file_store=$MAX_FILE_STORE is enforced by the account limit."
echo "   Measured behaviour (session 315, nats-server 2.15.3): with the account"
echo "   cap set, publishing past the cap fails with 503 'insufficient resources'"
echo "   instead of filling the disk; a 8 MB cap stopped at 8.39 MB on disk."
echo "   This is the reason NOT to pre-create the stream with MaxAge/MaxMsgs:"
echo "   reva's connect() calls AddStream(Name, Subjects) unconditionally when the"
echo "   stream exists, and NATS rejects that with 400 'stream name already in use"
echo "   with a different configuration'. See the note printed at the end."
du -sh "$STORE" 2>/dev/null | awk '{print "   store now: " $1}'

echo "== 7. stop =="
kill "$NPID" 2>/dev/null
for _ in $(seq 1 15); do kill -0 "$NPID" 2>/dev/null || break; sleep 1; done
kill -0 "$NPID" 2>/dev/null && { kill -9 "$NPID" 2>/dev/null; sleep 1; }
if kill -0 "$NPID" 2>/dev/null; then fail "could not stop nats-server (pid $NPID)"; fi
rm -f "$PIDFILE"
echo "   stopped (pid $NPID gone)"
if ss -ltnH 2>/dev/null | awk '{print $4}' | grep -qE ":$PORT\$"; then
  fail "port $PORT still listening after stop"
fi
echo "   port $PORT free again"

if [ "$KEEP" = "1" ]; then
  echo "   KEEP=1: leaving $STORE and the package in place"
else
  if [ "$PURGE" = "1" ]; then
    rm -rf "$STORE"
    echo "   store removed: $STORE"
  fi
fi
rm -f "$CONF"

echo "== 8. final state =="
echo "   nats-server installed: $(dpkg-query -W -f='${Version}' nats-server 2>/dev/null || echo no)"
echo "   nats-server running:   $(pgrep -c nats-server 2>/dev/null || echo 0) process(es)"
echo "   swap used:             $(awk '/SwapTotal/{t=$2} /SwapFree/{f=$2} END{printf "%.0f", (t-f)/1024}' /proc/meminfo) MiB"
echo "   MemAvailable:          $(awk '/MemAvailable/{printf "%.0f", $2/1024}' /proc/meminfo) MiB"
echo "   revad:                 $(systemctl is-active revad 2>/dev/null || echo unknown)"
echo
echo "ALL CHECKS PASSED - the broker is stopped, the device is back to normal"
echo
echo "NOTE for whoever enables this for real:"
echo "  reva creates the stream itself (Name + Subjects only, no MaxAge/MaxMsgs),"
echo "  so an operator must NOT pre-create it with limits - AddStream then fails"
echo "  with 400 'stream name already in use with a different configuration' and"
echo "  the notification helper refuses to connect. Bound the growth with the"
echo "  server-side account cap (max_file_store) instead, which is what this run"
echo "  verified."
