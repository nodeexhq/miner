#!/bin/sh
# Signs a command-line binary with a Developer ID and has Apple notarize it.
# A bare binary cannot carry its notarization ticket, so macOS checks it
# online the first time it runs.
#
#   SIGN_IDENTITY=<SHA-1> scripts/sign-binary.sh path/to/binary
#
# Notary credentials: APPLE_ID, APPLE_TEAM_ID and APPLE_APP_PASSWORD in the
# environment, or else the keychain profile NOTARY_PROFILE (default nodeex-notary).
set -eu
BIN=$1
: "${SIGN_IDENTITY:?set SIGN_IDENTITY to the SHA-1 of the signing certificate}"
codesign --force --options runtime --timestamp -s "$SIGN_IDENTITY" "$BIN"
codesign --verify --strict "$BIN"
WORK=$(mktemp -d)
ditto -c -k --keepParent "$BIN" "$WORK/submit.zip"
if [ -n "${APPLE_ID:-}" ]; then
  xcrun notarytool submit "$WORK/submit.zip" --apple-id "$APPLE_ID" --team-id "$APPLE_TEAM_ID" --password "$APPLE_APP_PASSWORD" --wait > "$WORK/notary.log"
else
  xcrun notarytool submit "$WORK/submit.zip" --keychain-profile "${NOTARY_PROFILE:-nodeex-notary}" --wait > "$WORK/notary.log"
fi
grep -E "status:" "$WORK/notary.log" | tail -1
grep -q "status: Accepted" "$WORK/notary.log"
