#!/bin/sh
# Builds "NodeEx Miner.app": the Swift app with the Go core inside it, and
# the nodeex-vm tool beside it, in app/dist/.
#
#   app/scripts/bundle.sh                 ad-hoc signature, for this Mac only
#   SIGN_IDENTITY=<SHA-1> app/scripts/bundle.sh   Developer ID signature
#
# Virtual machines need the com.apple.security.virtualization entitlement,
# which is why even a local build is signed.
set -eu
APP_DIR=$(cd "$(dirname "$0")/.." && pwd)
REPO=$(cd "$APP_DIR/.." && pwd)
VERSION=${VERSION:-$(git -C "$REPO" describe --tags --always --dirty 2>/dev/null || echo dev)}
IDENTITY=${SIGN_IDENTITY:--}
DIST="$APP_DIR/dist"
BUNDLE="$DIST/NodeEx Miner.app"

(cd "$APP_DIR" && swift build -c release)
BIN=$(cd "$APP_DIR" && swift build -c release --show-bin-path)

mkdir -p "$DIST"
[ ! -d "$BUNDLE" ] || mv "$BUNDLE" "$DIST/.old-$(date +%s)"
mkdir -p "$BUNDLE/Contents/MacOS" "$BUNDLE/Contents/Helpers"
cp "$BIN/NodeExMiner" "$BUNDLE/Contents/MacOS/NodeExMiner"
(cd "$REPO" && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
  -o "$BUNDLE/Contents/Helpers/nodeex-miner" ./cmd/nodeex-miner)
cp "$BIN/nodeex-vm" "$DIST/nodeex-vm"

cat > "$BUNDLE/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>NodeEx Miner</string>
	<key>CFBundleDisplayName</key><string>NodeEx Miner</string>
	<key>CFBundleIdentifier</key><string>com.nodeex.miner</string>
	<key>CFBundleExecutable</key><string>NodeExMiner</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleShortVersionString</key><string>${VERSION#v}</string>
	<key>CFBundleVersion</key><string>${VERSION#v}</string>
	<key>LSMinimumSystemVersion</key><string>15.0</string>
	<key>LSUIElement</key><true/>
	<key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST

sign() {
  if [ "$IDENTITY" = "-" ]; then codesign --force -s - "$@"; else codesign --force --options runtime --timestamp -s "$IDENTITY" "$@"; fi
}
sign "$BUNDLE/Contents/Helpers/nodeex-miner"
sign --entitlements "$APP_DIR/NodeExMiner.entitlements" "$BUNDLE"
sign --entitlements "$APP_DIR/NodeExMiner.entitlements" "$DIST/nodeex-vm"
codesign --verify --strict "$BUNDLE"
echo "built $BUNDLE ($VERSION)"
