#!/bin/sh
# Build the OpenWrt/ImmortalWrt APK (apk-tools 3.x).
#
#   VERSION=1.1.0-r12 sh package/build-apk.sh
#   ARCH=arm_cortex-a7 GOARCH=arm ZIG_TARGET=arm-linux-musleabihf \
#     APK=/path/to/apk.static sh package/build-apk.sh
#
# Requires: Go 1.24+, zig (or a prebuilt filter/urftopwg.real), apk/apk.static
# with mkpkg support.
set -eu

VERSION=${VERSION:-1.1.0-r12}
ARCH=${ARCH:-aarch64_cortex-a53}
GOARCH=${GOARCH:-arm64}
GOARM=${GOARM:-}
ZIG_TARGET=${ZIG_TARGET:-aarch64-linux-musl}
APK=${APK:-apk}
DESCRIPTION=${DESCRIPTION:-"AirPrint bridge for EPSON L3250"}

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/.." && pwd)
BUILD="$HERE/build"
STAGE="$BUILD/stage-$VERSION"

rm -rf "$STAGE"
mkdir -p "$STAGE"
cp -R "$HERE/root/." "$STAGE/"
mkdir -p "$STAGE/usr/sbin" "$STAGE/usr/lib/cups/filter"

echo "==> building Go proxy (linux/$GOARCH)"
(
	cd "$REPO/go"
	GOOS=linux GOARCH="$GOARCH" GOARM="$GOARM" CGO_ENABLED=0 \
		go build -trimpath -ldflags "-s -w" -o "$STAGE/usr/sbin/airprint-proxy" .
)

echo "==> filter"
if [ -f "$REPO/filter/urftopwg.real" ]; then
	cp "$REPO/filter/urftopwg.real" "$STAGE/usr/lib/cups/filter/urftopwg.real"
else
	if ! command -v "${ZIG:-zig}" >/dev/null 2>&1; then
		echo "zig not found; run filter/build.sh first or install zig" >&2
		exit 1
	fi
	"${ZIG:-zig}" cc -target "$ZIG_TARGET" -Os -s -static \
		-o "$STAGE/usr/lib/cups/filter/urftopwg.real" "$REPO/filter/urftopwg.c"
fi

chmod 0755 "$STAGE/usr/sbin/airprint-proxy" \
	"$STAGE/usr/lib/cups/filter/urftopwg.real" \
	"$STAGE/usr/lib/cups/filter/urftopwg" \
	"$STAGE/usr/lib/cups/backend/ipps" \
	"$STAGE/etc/init.d/airprint" \
	"$STAGE/etc/init.d/ontaccess" \
	"$STAGE/etc/uci-defaults/90-airprint" \
	"$STAGE/etc/uci-defaults/91-ontaccess"
chmod 0755 "$STAGE"/usr/sbin/airprint-avahi "$STAGE"/usr/sbin/airprint-setup \
	"$STAGE"/usr/sbin/airprint-status "$STAGE"/usr/sbin/ontaccess-setup \
	"$STAGE"/usr/lib/airprint/airprint-proxy.py
chmod 0644 "$STAGE/usr/share/airprint/EPSON_L3250.ppd" \
	"$STAGE/usr/share/airprint/README.md" \
	"$STAGE/usr/lib/airprint/common.py" \
	"$STAGE"/usr/share/luci/menu.d/*.json "$STAGE"/usr/share/rpcd/acl.d/*.json \
	"$STAGE"/www/luci-static/resources/view/airprint/*.js \
	"$STAGE"/www/luci-static/resources/view/ontaccess/*.js

mkdir -p "$BUILD"
cd "$BUILD"
"$APK" --root=/ mkpkg --no-xattrs \
	-I name:airprint-bridge \
	-I version:"$VERSION" \
	-I "description:$DESCRIPTION" \
	-I arch:"$ARCH" \
	-I license:MIT \
	-I origin:airprint-bridge \
	-s post-install:"$HERE/scripts/post-install" \
	-s post-upgrade:"$HERE/scripts/post-upgrade" \
	-s pre-upgrade:"$HERE/scripts/pre-upgrade" \
	-s pre-deinstall:"$HERE/scripts/pre-deinstall" \
	-F "$STAGE" -o "$BUILD/airprint-bridge-$VERSION.apk"

ls -l "$BUILD/airprint-bridge-$VERSION.apk"
