#!/usr/bin/env bash
# Build and sign Oshirase.app, the bundle the daemon posts notifications
# through.
#
#   macos/build-app.sh <output dir> [version]
#
# Signs with $OSHIRASE_SIGN_IDENTITY when set, else the first "Apple
# Development" identity in the keychain, else ad hoc. macOS ties the
# notification permission to the bundle identifier and signature, so a
# rebuild signed the same way keeps the permission.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
out="${1:?usage: build-app.sh <output dir> [version]}"
version="${2:-0}"
app="$out/Oshirase.app"

identity="${OSHIRASE_SIGN_IDENTITY:-}"
if [[ -z "$identity" ]]; then
  identity="$(security find-identity -v -p codesigning \
    | sed -n 's/^ *[0-9]*) \([0-9A-F]\{40\}\) "Apple Development: .*/\1/p' | head -n 1)"
fi
identity="${identity:--}"

rm -rf "$app"
mkdir -p "$app/Contents/MacOS"

# The bundle version must be numbers and dots: v0.1.0+dirty → 0.1.0. Past a
# tag Go stamps a pseudo-version for the next patch
# (v0.1.1-0.20261112091225-9209d2bdb051 → 0.1.1), and "devel" becomes 0.
bundle_version="$(printf '%s' "$version" | sed -E 's/^v//; s/[^0-9.].*$//')"
sed -e "s|__VERSION__|${bundle_version:-0}|g" "$here/Info.plist" > "$app/Contents/Info.plist"

xcrun swiftc -O -swift-version 5 -parse-as-library \
  -target "$(uname -m)-apple-macos13.0" \
  -o "$app/Contents/MacOS/oshirase-notifier" "$here/Notifier.swift"

codesign --force --sign "$identity" --identifier dev.moroz.oshirase.notifier "$app"
codesign --verify --strict "$app"
printf '%s\n' "$app"
