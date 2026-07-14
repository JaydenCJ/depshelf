#!/usr/bin/env bash
# End-to-end smoke test for depshelf: builds the binary, seeds a store
# offline via `import`, serves it, chains a second read-through shelf to
# the first, kills the "network" and proves the stale fallback, then checks
# the allowlist gate and `verify`. Loopback only — no real network.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="$(mktemp -d)"
A_PID=""
B_PID=""
cleanup() {
  [ -n "$A_PID" ] && kill "$A_PID" 2>/dev/null || true
  [ -n "$B_PID" ] && kill "$B_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

fail() {
  echo "SMOKE FAIL: $*" >&2
  exit 1
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# wait_addr <logfile>: block until the server logs its bound address.
wait_addr() {
  local addr=""
  for _ in $(seq 1 100); do
    addr="$(sed -n 's|.*listening on http://\([0-9.]*:[0-9]*\).*|\1|p' "$1" | head -1)"
    [ -n "$addr" ] && { echo "$addr"; return 0; }
    sleep 0.1
  done
  return 1
}

BIN="$WORKDIR/depshelf"
STORE_A="$WORKDIR/store-a"
STORE_B="$WORKDIR/store-b"

echo "1. build"
(cd "$ROOT" && go build -o "$BIN" ./cmd/depshelf) || fail "go build failed"

echo "2. version matches manifest"
"$BIN" version | grep -qx "depshelf 0.1.0" || fail "version mismatch"

echo "3. seed a store offline with import (npm tarball + pypi wheel)"
mkdir -p "$WORKDIR/pkg/package"
printf '{"name":"demo-lib","version":"1.0.0"}\n' > "$WORKDIR/pkg/package/package.json"
tar -czf "$WORKDIR/demo-lib-1.0.0.tgz" -C "$WORKDIR/pkg" package
printf 'not-a-real-wheel-but-bytes-are-bytes' > "$WORKDIR/demo_lib-1.0.0-py3-none-any.whl"
"$BIN" import npm  --store "$STORE_A" "$WORKDIR/demo-lib-1.0.0.tgz" \
  | grep -q "imported npm demo-lib@1.0.0" || fail "npm import"
"$BIN" import pypi --store "$STORE_A" "$WORKDIR/demo_lib-1.0.0-py3-none-any.whl" \
  | grep -q "imported pypi demo-lib" || fail "pypi import"

echo "4. list sees both ecosystems"
LIST="$("$BIN" list --store "$STORE_A")"
echo "$LIST" | grep -q "npm *demo-lib" || fail "npm package missing from list"
echo "$LIST" | grep -q "pypi *demo-lib" || fail "pypi project missing from list"
echo "$LIST" | grep -q "2 packages, 2 artifacts" || fail "list totals wrong"

echo "5. serve shelf A fully offline"
"$BIN" serve --store "$STORE_A" --offline --listen 127.0.0.1:0 2> "$WORKDIR/a.log" &
A_PID=$!
ADDR_A="$(wait_addr "$WORKDIR/a.log")" || fail "shelf A never came up"
curl -fsS "http://$ADDR_A/healthz" | grep -qx "ok" || fail "healthz"
curl -fsS "http://$ADDR_A/" | grep -q '"mode":"offline"' || fail "status mode"

echo "6. npm protocol: packument rewritten to the mirror, tarball intact"
curl -fsS "http://$ADDR_A/npm/demo-lib" > "$WORKDIR/packument.json"
grep -q "\"http://$ADDR_A/npm/demo-lib/-/demo-lib-1.0.0.tgz\"" "$WORKDIR/packument.json" \
  || fail "tarball URL not rewritten"
grep -q '"latest":"1.0.0"' "$WORKDIR/packument.json" || fail "dist-tags missing"
curl -fsS "http://$ADDR_A/npm/demo-lib/-/demo-lib-1.0.0.tgz" -o "$WORKDIR/fetched.tgz"
[ "$(sha256 "$WORKDIR/fetched.tgz")" = "$(sha256 "$WORKDIR/demo-lib-1.0.0.tgz")" ] \
  || fail "tarball bytes differ"

echo "7. PyPI protocol: PEP 503 page with sha256 fragment, file intact"
WHEEL_SHA="$(sha256 "$WORKDIR/demo_lib-1.0.0-py3-none-any.whl")"
curl -fsS "http://$ADDR_A/pypi/simple/demo-lib/" > "$WORKDIR/simple.html"
grep -q "demo_lib-1.0.0-py3-none-any.whl#sha256=$WHEEL_SHA" "$WORKDIR/simple.html" \
  || fail "simple index missing hash fragment"
curl -fsS -H 'Accept: application/vnd.pypi.simple.v1+json' \
  "http://$ADDR_A/pypi/simple/demo-lib/" | grep -q '"api-version"' || fail "PEP 691 negotiation"
curl -fsSL "http://$ADDR_A/pypi/simple/Demo_Lib/" -o /dev/null \
  -w '%{url_effective}' | grep -q "/pypi/simple/demo-lib/$" || fail "PEP 503 redirect"
curl -fsS "http://$ADDR_A/pypi/files/demo-lib/demo_lib-1.0.0-py3-none-any.whl" -o "$WORKDIR/fetched.whl"
[ "$(sha256 "$WORKDIR/fetched.whl")" = "$WHEEL_SHA" ] || fail "wheel bytes differ"

echo "8. offline shelf 404s what it does not hold"
CODE="$(curl -s -o /dev/null -w '%{http_code}' "http://$ADDR_A/npm/never-cached")"
[ "$CODE" = "404" ] || fail "offline miss returned $CODE"

echo "9. chain shelf B (read-through) to shelf A as its upstream"
"$BIN" serve --store "$STORE_B" --listen 127.0.0.1:0 \
  --npm-upstream "http://$ADDR_A/npm" --pypi-upstream "http://$ADDR_A/pypi/simple" \
  2> "$WORKDIR/b.log" &
B_PID=$!
ADDR_B="$(wait_addr "$WORKDIR/b.log")" || fail "shelf B never came up"
curl -fsS -D "$WORKDIR/b-headers" "http://$ADDR_B/npm/demo-lib" -o /dev/null
grep -qi "x-depshelf-source: upstream" "$WORKDIR/b-headers" || fail "B should read through"
curl -fsS "http://$ADDR_B/npm/demo-lib/-/demo-lib-1.0.0.tgz" -o "$WORKDIR/via-b.tgz"
[ "$(sha256 "$WORKDIR/via-b.tgz")" = "$(sha256 "$WORKDIR/demo-lib-1.0.0.tgz")" ] \
  || fail "tarball via B differs"
curl -fsS "http://$ADDR_B/pypi/files/demo-lib/demo_lib-1.0.0-py3-none-any.whl" -o /dev/null
[ -f "$STORE_B/npm/demo-lib/tarballs/demo-lib-1.0.0.tgz" ] || fail "B did not persist the tarball"
[ -f "$STORE_B/pypi/demo-lib/files/demo_lib-1.0.0-py3-none-any.whl.sha256" ] \
  || fail "B did not write a sidecar"

echo "10. kill the upstream: B serves stale metadata instead of failing"
kill "$A_PID" && wait "$A_PID" 2>/dev/null || true
A_PID=""
touch -t 202001010000 "$STORE_B/npm/demo-lib/packument.json" # expire the TTL without sleeping
curl -fsS -D "$WORKDIR/stale-headers" "http://$ADDR_B/npm/demo-lib" -o /dev/null \
  || fail "B failed with upstream down"
grep -qi "x-depshelf-source: stale" "$WORKDIR/stale-headers" || fail "stale fallback not used"

echo "11. allowlist: deny-by-default lockdown"
kill "$B_PID" && wait "$B_PID" 2>/dev/null || true
B_PID=""
printf 'npm:@myorg/*\npypi:requests\n' > "$WORKDIR/allowlist.txt"
"$BIN" serve --store "$STORE_B" --offline --listen 127.0.0.1:0 \
  --allowlist "$WORKDIR/allowlist.txt" 2> "$WORKDIR/c.log" &
B_PID=$!
ADDR_C="$(wait_addr "$WORKDIR/c.log")" || fail "locked-down shelf never came up"
CODE="$(curl -s -o /dev/null -w '%{http_code}' "http://$ADDR_C/npm/demo-lib")"
[ "$CODE" = "403" ] || fail "allowlist let demo-lib through ($CODE)"
CODE="$(curl -s -o /dev/null -w '%{http_code}' "http://$ADDR_C/pypi/simple/demo-lib/")"
[ "$CODE" = "403" ] || fail "allowlist let pypi demo-lib through ($CODE)"

echo "12. verify: clean store passes, one flipped byte fails"
"$BIN" verify --store "$STORE_B" | grep -q "2 ok, 0 corrupt" || fail "verify clean"
printf 'X' >> "$STORE_B/npm/demo-lib/tarballs/demo-lib-1.0.0.tgz"
if "$BIN" verify --store "$STORE_B" > "$WORKDIR/verify.out"; then
  fail "verify missed the corruption"
fi
grep -q "corrupt" "$WORKDIR/verify.out" || fail "verify output silent about corruption"

echo "SMOKE OK"
