#!/usr/bin/env bash
# Goアプリを linux/amd64 向けにローカルでクロスビルドする。
#
# c5.large (2 vCPU) の上でビルドするとベンチ直前に数十秒CPUを食うので、手元で作って配る。
# MySQLドライバは pure Go なので CGO なしでビルドできる。
#
# usage: ./tools/deploy/build.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$ROOT/build"
cd "$ROOT/webapp/go"
echo "==> build isuride (linux/amd64)"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$ROOT/build/isuride" .
ls -l "$ROOT/build/isuride" | awk '{print "    " $5 " bytes"}'
