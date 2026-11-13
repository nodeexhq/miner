#!/bin/sh
# Publishes a packaged app as an update: uploads the disk image to the
# GitHub release of a tag and points the update feed at it. Installed apps
# pick it up within six hours and install it when no lease is running.
#
#   app/scripts/publish.sh v0.2.0        (after app/scripts/package.sh at that tag)
#
# The feed is app/appcast.xml on the main branch: this script rewrites it
# and leaves the commit and push to you, so nothing goes out unreviewed.
set -eu
TAG=$1
APP_DIR=$(cd "$(dirname "$0")/.." && pwd)
REPO=$(cd "$APP_DIR/.." && pwd)
DMG="$APP_DIR/dist/NodeEx-Miner-$TAG.dmg"
[ -f "$DMG" ] || { echo "no $DMG: run package.sh with the tag checked out" >&2; exit 1; }
BUILD=$(git -C "$REPO" rev-list --count "$TAG")
# The update-signing key is in the keychain on a developer's Mac, and in a
# file named by SPARKLE_KEY_FILE in CI.
if [ -n "${SPARKLE_KEY_FILE:-}" ]; then
  SIGNATURE=$("$APP_DIR/.build/artifacts/sparkle/Sparkle/bin/sign_update" --ed-key-file "$SPARKLE_KEY_FILE" "$DMG")
else
  SIGNATURE=$("$APP_DIR/.build/artifacts/sparkle/Sparkle/bin/sign_update" --account nodeex-miner "$DMG")
fi
gh release upload "$TAG" -R nodeexhq/miner "$DMG" "$DMG.sha256" --clobber
cat > "$APP_DIR/appcast.xml" <<FEED
<?xml version="1.0" encoding="utf-8"?>
<!-- The update feed of NodeEx Miner for macOS. Installed apps read this file
     from the main branch; scripts/publish.sh rewrites it for each release.
     An item is trusted only if its signature matches the key built into
     the app, so this file alone cannot make an app install anything. -->
<rss version="2.0" xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle">
  <channel>
    <title>NodeEx Miner</title>
    <item>
      <title>${TAG#v}</title>
      <pubDate>$(LC_ALL=C date -u "+%a, %d %b %Y %H:%M:%S +0000")</pubDate>
      <sparkle:version>$BUILD</sparkle:version>
      <sparkle:shortVersionString>${TAG#v}</sparkle:shortVersionString>
      <sparkle:minimumSystemVersion>15.0</sparkle:minimumSystemVersion>
      <enclosure url="https://github.com/nodeexhq/miner/releases/download/$TAG/$(basename "$DMG")" type="application/octet-stream" $SIGNATURE />
    </item>
  </channel>
</rss>
FEED
echo "uploaded $(basename "$DMG") and wrote app/appcast.xml; commit and push it to release the update"
