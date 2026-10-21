#!/bin/sh
# Runs the SDK's end-to-end test against a real core: starts one with Go's
# TestSDKHarness (a self-serve app with a test key, on a test database),
# waits for it to write its URL and key, runs test/e2e.test.ts, then stops it.
# Needs Postgres (make db) and Go.
set -eu
sdk=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$sdk/../.." && pwd)
dir=$(mktemp -d)
out="$dir/harness.json"
log="$dir/harness.log"
: "${WAKESHIFT_TEST_DATABASE_URL:=postgres://postgres:postgres@localhost:5439/postgres?sslmode=disable}"
export WAKESHIFT_TEST_DATABASE_URL

(cd "$repo" && WAKESHIFT_SDK_HARNESS="$out" go test -count=1 -run '^TestSDKHarness$' -timeout 15m -v ./internal/coretest/) >"$log" 2>&1 &
pid=$!
trap 'rm -f "$out"; wait $pid 2>/dev/null || true; rm -rf "$dir"' EXIT

i=0
while [ ! -f "$out" ]; do
  if ! kill -0 "$pid" 2>/dev/null; then
    echo "the core didn't start:" >&2
    cat "$log" >&2
    exit 1
  fi
  i=$((i + 1))
  if [ "$i" -gt 1200 ]; then
    echo "the core took over two minutes to start" >&2
    exit 1
  fi
  sleep 0.1
done

cd "$sdk"
npx tsc -p tsconfig.test.json
WAKESHIFT_SDK_HARNESS="$out" node --test build-test/test/e2e.test.js || {
  status=$?
  echo "core log: $log" >&2
  tail -50 "$log" >&2
  exit $status
}
