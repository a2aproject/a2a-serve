#!/bin/sh
# Builds the C libraries of the sandbox and of a2aacp, their headers, and
# the launcher of the Docker sandbox into a directory, by default the one
# the Python package loads them from.
#
#   clib/build.sh [<out dir>]
set -eu
cd "$(dirname "$0")/.."
out=${1:-python/src/a2a_serve/_lib}
mkdir -p "$out"
go build -buildmode=c-shared -o "$out/liba2asandbox.so" ./clib/sandbox
go build -buildmode=c-shared -o "$out/liba2aacp.so" ./clib/acp
# The launcher runs as PID 1 of the containers, which may lack a dynamic
# loader, and drops capabilities from every thread, which cgo prevents.
CGO_ENABLED=0 GOOS=linux go build -trimpath -o "$out/a2a-sandbox-init" ./sandbox/cmd/a2a-sandbox-init
# Container root runs it without CAP_DAC_OVERRIDE, as another user than
# its owner.
chmod 755 "$out/a2a-sandbox-init"
echo "built into $out"
