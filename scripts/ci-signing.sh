#!/bin/sh
# Prepares a GitHub runner to sign: puts the Developer ID certificate from
# the repository's secrets into a keychain made for this job, and prints the
# identity to sign with. The keychain and the decoded certificate live in the
# runner's temporary directory and go when the job ends.
#
# Needs MACOS_CERT_P12_BASE64 and MACOS_CERT_PASSWORD in the environment.
set -eu
: "${MACOS_CERT_P12_BASE64:?the signing certificate is not configured}"
: "${MACOS_CERT_PASSWORD:?the password of the signing certificate is not configured}"
KEYCHAIN="$RUNNER_TEMP/signing.keychain-db"
PASS=$(openssl rand -hex 24)
security create-keychain -p "$PASS" "$KEYCHAIN"
security set-keychain-settings -lut 3600 "$KEYCHAIN"
security unlock-keychain -p "$PASS" "$KEYCHAIN"
printf '%s' "$MACOS_CERT_P12_BASE64" | base64 --decode > "$RUNNER_TEMP/cert.p12"
security import "$RUNNER_TEMP/cert.p12" -k "$KEYCHAIN" -P "$MACOS_CERT_PASSWORD" -T /usr/bin/codesign >/dev/null
rm -f "$RUNNER_TEMP/cert.p12"
security set-key-partition-list -S apple-tool:,apple: -s -k "$PASS" "$KEYCHAIN" >/dev/null
security list-keychains -d user -s "$KEYCHAIN" $(security list-keychains -d user | tr -d '"')
security find-identity -v -p codesigning "$KEYCHAIN" | awk '/Developer ID Application/ {print $2; exit}'
