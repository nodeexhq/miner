#!/bin/sh
# Builds the disk image owners download: "NodeEx Miner.app" signed with a
# Developer ID, notarized by Apple and stapled, so it opens without a
# warning and without a network connection.
#
#   SIGN_IDENTITY=<SHA-1 of the Developer ID Application certificate> app/scripts/package.sh
#
# The notary credentials are a keychain profile (NOTARY_PROFILE, default
# nodeex-notary) made with `xcrun notarytool store-credentials`.
set -eu
APP_DIR=$(cd "$(dirname "$0")/.." && pwd)
REPO=$(cd "$APP_DIR/.." && pwd)
: "${SIGN_IDENTITY:?set SIGN_IDENTITY to the SHA-1 of the signing certificate}"
PROFILE=${NOTARY_PROFILE:-nodeex-notary}
VERSION=${VERSION:-$(git -C "$REPO" describe --tags --always --dirty 2>/dev/null || echo dev)}
export VERSION SIGN_IDENTITY

"$APP_DIR/scripts/bundle.sh"
DIST="$APP_DIR/dist"
STAGE=$(mktemp -d)
cp -R "$DIST/NodeEx Miner.app" "$STAGE/"
ln -s /Applications "$STAGE/Applications"
DMG="$DIST/NodeEx-Miner-$VERSION.dmg"
[ ! -f "$DMG" ] || mv "$DMG" "$DIST/.old-$(date +%s).dmg"
hdiutil create -quiet -volname "NodeEx Miner" -srcfolder "$STAGE" -fs HFS+ -format UDZO "$DMG"
codesign --force --timestamp -s "$SIGN_IDENTITY" "$DMG"

echo "notarizing $(basename "$DMG")"
xcrun notarytool submit "$DMG" --keychain-profile "$PROFILE" --wait | tee "$STAGE/notary.log" | grep -E "status:" | tail -1
grep -q "status: Accepted" "$STAGE/notary.log"
xcrun stapler staple -q "$DMG"
xcrun stapler validate -q "$DMG"
(cd "$DIST" && shasum -a 256 "$(basename "$DMG")" > "$(basename "$DMG").sha256")
echo "packaged $DMG"
