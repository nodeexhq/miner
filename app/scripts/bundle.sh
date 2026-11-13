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
# Sparkle compares build numbers, so the build is a plain count of commits.
BUILD=${BUILD:-$(git -C "$REPO" rev-list --count HEAD 2>/dev/null || echo 1)}
# Where the app looks for updates. Empty builds an app that never updates.
FEED_URL=${FEED_URL-https://raw.githubusercontent.com/nodeexhq/miner/main/app/appcast.xml}
DIST="$APP_DIR/dist"
BUNDLE="$DIST/NodeEx Miner.app"

(cd "$APP_DIR" && swift build -c release)
BIN=$(cd "$APP_DIR" && swift build -c release --show-bin-path)

mkdir -p "$DIST"
[ ! -d "$BUNDLE" ] || mv "$BUNDLE" "$DIST/.old-$(date +%s)"
mkdir -p "$BUNDLE/Contents/MacOS" "$BUNDLE/Contents/Helpers" "$BUNDLE/Contents/Resources"
cp "$APP_DIR/Resources/AppIcon.icns" "$BUNDLE/Contents/Resources/AppIcon.icns"
mkdir -p "$BUNDLE/Contents/Frameworks"
cp -R "$APP_DIR/.build/artifacts/sparkle/Sparkle/Sparkle.xcframework/macos-arm64_x86_64/Sparkle.framework" "$BUNDLE/Contents/Frameworks/"
cp "$BIN/NodeExMiner" "$BUNDLE/Contents/MacOS/NodeExMiner"
(cd "$REPO" && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
  -o "$BUNDLE/Contents/Helpers/nodeex-miner" ./cmd/nodeex-miner)
cp "$BIN/nodeex-vm" "$DIST/nodeex-vm"
(cd "$REPO/vmnet" && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o "$BUNDLE/Contents/Helpers/nodeex-vmnet" .)
cp "$BUNDLE/Contents/Helpers/nodeex-vmnet" "$DIST/nodeex-vmnet"

# The update settings, when the build has a feed. A feed on this Mac is for
# testing, and needs the system's leave to be fetched without TLS.
UPDATE_KEYS=""
if [ -n "$FEED_URL" ]; then
  UPDATE_KEYS="	<key>SUFeedURL</key><string>$FEED_URL</string>
	<key>SUPublicEDKey</key><string>$(cat "$APP_DIR/sparkle_public_key")</string>
	<key>SUEnableAutomaticChecks</key><true/>
	<key>SUAutomaticallyUpdate</key><true/>
	<key>SUScheduledCheckInterval</key><integer>21600</integer>"
  case "$FEED_URL" in
    http://127.0.0.1*|http://localhost*)
      UPDATE_KEYS="$UPDATE_KEYS
	<key>NSAppTransportSecurity</key><dict><key>NSAllowsLocalNetworking</key><true/></dict>" ;;
  esac
fi

cat > "$BUNDLE/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>NodeEx Miner</string>
	<key>CFBundleDisplayName</key><string>NodeEx Miner</string>
	<key>CFBundleIdentifier</key><string>com.nodeex.miner</string>
	<key>CFBundleExecutable</key><string>NodeExMiner</string>
	<key>CFBundleIconFile</key><string>AppIcon</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleShortVersionString</key><string>${VERSION#v}</string>
	<key>CFBundleVersion</key><string>${BUILD}</string>
${UPDATE_KEYS}
	<key>LSMinimumSystemVersion</key><string>15.0</string>
	<key>LSUIElement</key><true/>
	<key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST

sign() {
  if [ "$IDENTITY" = "-" ]; then codesign --force -s - "$@"; else codesign --force --options runtime --timestamp -s "$IDENTITY" "$@"; fi
}
# Sparkle's own programs are signed from the inside out, as its
# documentation asks, before the framework and then the app around it.
SPARKLE="$BUNDLE/Contents/Frameworks/Sparkle.framework"
sign "$SPARKLE/Versions/B/XPCServices/Installer.xpc"
sign --preserve-metadata=entitlements "$SPARKLE/Versions/B/XPCServices/Downloader.xpc"
sign "$SPARKLE/Versions/B/Autoupdate"
sign "$SPARKLE/Versions/B/Updater.app"
sign "$SPARKLE"
sign "$BUNDLE/Contents/Helpers/nodeex-miner"
sign "$BUNDLE/Contents/Helpers/nodeex-vmnet"
sign "$DIST/nodeex-vmnet"
sign --entitlements "$APP_DIR/NodeExMiner.entitlements" "$BUNDLE"
sign --entitlements "$APP_DIR/NodeExMiner.entitlements" "$DIST/nodeex-vm"
codesign --verify --strict "$BUNDLE"
echo "built $BUNDLE ($VERSION, build $BUILD)"
