#!/bin/sh
# Regenerates Resources/AppIcon.icns and Resources/avatar.png.
# Usage: make-icon.sh [TILE], TILE one hex colour or two for a gradient.
# Both apps use the brand gradient, the same as the website's tiles.
set -eu
APP_DIR=$(cd "$(dirname "$0")/.." && pwd)
COLOR="${1:-#2FA8F0,#2E5FE0}"
WORK=$(mktemp -d)
cat "$APP_DIR"/Sources/*/MenuBarMark.swift "$APP_DIR/scripts/make-icon.swift" | grep -v '^import AppKit$' | { echo "import AppKit"; cat; } > "$WORK/main.swift"
swiftc -O -o "$WORK/make-icon" "$WORK/main.swift"
"$WORK/make-icon" "$WORK/icon.png" "$COLOR"
SET="$WORK/AppIcon.iconset"
mkdir "$SET"
for s in 16 32 128 256 512; do
  sips -z $s $s "$WORK/icon.png" --out "$SET/icon_${s}x${s}.png" >/dev/null
  d=$((s * 2))
  sips -z $d $d "$WORK/icon.png" --out "$SET/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns "$SET" -o "$APP_DIR/Resources/AppIcon.icns"
"$WORK/make-icon" "$APP_DIR/Resources/avatar.png" "$COLOR" avatar
echo "$WORK/icon.png"
