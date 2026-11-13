#!/bin/sh
# Regenerates Resources/AppIcon.icns from scripts/make-icon.swift.
set -eu
APP_DIR=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
swift "$APP_DIR/scripts/make-icon.swift" "$WORK/icon.png"
SET="$WORK/AppIcon.iconset"
mkdir "$SET"
for s in 16 32 128 256 512; do
  sips -z $s $s "$WORK/icon.png" --out "$SET/icon_${s}x${s}.png" >/dev/null
  d=$((s * 2))
  sips -z $d $d "$WORK/icon.png" --out "$SET/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns "$SET" -o "$APP_DIR/Resources/AppIcon.icns"
cp "$SET/icon_256x256.png" "$WORK/preview.png"
echo "$WORK/preview.png"
