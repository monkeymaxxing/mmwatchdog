#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
root=${MMWATCHDOG_TEST_ROOT:-$HOME/.cache/mmwatchdog-testing}
exec python3 tests/integration/testenv.py --root "$root" -- sh -eu -c '
root=$1
mkdir -p "$root/bin"
# At the lowest priority: test guests of other test environments run on the
# same machine, and their suites measure in seconds.
CGO_ENABLED=0 nice -n 19 go build -trimpath -ldflags="-s -w" -o "$root/bin/mmwatchdog" ./cmd/mmwatchdog
for arch in arm64 amd64; do
 CGO_ENABLED=0 GOOS=linux GOARCH=$arch nice -n 19 go build -trimpath -ldflags="-s -w" -o "$root/bin/mmwatchdog-linux-$arch" ./cmd/mmwatchdog
done
' sh "$root"
