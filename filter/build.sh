#!/bin/sh
# Build urftopwg.real (URF -> CUPS Raster) for the target platform.
# Requires zig: https://ziglang.org/download/
set -eu

TARGET=${TARGET:-aarch64-linux-musl}
ZIG=${ZIG:-zig}
HERE=$(cd "$(dirname "$0")" && pwd)

if ! command -v "$ZIG" >/dev/null 2>&1; then
	echo "zig not found; install zig or set ZIG=/path/to/zig" >&2
	exit 1
fi

"$ZIG" cc -target "$TARGET" -Os -s -static \
	-o "$HERE/urftopwg.real" "$HERE/urftopwg.c"

ls -l "$HERE/urftopwg.real"
