#!/usr/bin/env bash
# Install oshirase as a launchd agent for the current user, or reinstall it
# over an existing one.
#
#   ./install-macos.sh                  build from this checkout, then install
#   ./install-macos.sh path/to/oshirase install a binary built elsewhere
#
# Either way it builds Oshirase.app from macos/ (Xcode or its command-line
# tools needed), which is what the daemon shows notifications through.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
label="dev.moroz.oshirase"
binary="$HOME/.local/bin/oshirase"
app="$HOME/Applications/Oshirase.app"
plist="$HOME/Library/LaunchAgents/$label.plist"
log="$HOME/Library/Logs/oshirase.log"
domain="gui/$(id -u)"

say() { printf '==> %s\n' "$*" >&2; }

if [[ "$(uname -s)" != Darwin ]]; then
  say "This installs a launchd agent; run it on macOS."
  exit 1
fi

if [[ $# -ge 1 ]]; then
  source_binary="$1"
else
  say "Building"
  # Go stamps the version from the git tag itself (see buildVersion).
  mkdir -p "$repo/build"
  (cd "$repo" && go build -o build/oshirase .)
  source_binary="$repo/build/oshirase"
fi

say "Building Oshirase.app"
"$repo/macos/build-app.sh" "$repo/build" "$("$source_binary" version)" >/dev/null

say "Installing $binary and $app"
mkdir -p "$(dirname "$binary")" "$(dirname "$plist")" "$(dirname "$log")" "$(dirname "$app")"
install -m 0755 "$source_binary" "$binary"
rm -rf "$app"
ditto "$repo/build/Oshirase.app" "$app"

# Launching through LaunchServices registers the bundle, and the first run
# asks for permission here rather than when the first notification arrives.
say "Asking macOS for notification permission (answer the prompt if one appears)"
answer="$(mktemp)"
trap 'rm -f "$answer"' EXIT
if ! open -W -n --stdout "$answer" --stderr "$answer" -a "$app" --args authorize; then
  say "Could not launch $app"
  exit 1
fi
say "$(cat "$answer")"

sed -e "s|__BINARY__|$binary|" -e "s|__LOG__|$log|" \
  "$repo/launchd/$label.plist" > "$plist"

# bootout fails when the agent is not loaded yet; that is the first install.
launchctl bootout "$domain/$label" 2>/dev/null || true
launchctl bootstrap "$domain" "$plist"

say "Waiting for the daemon"
for _ in $(seq 20); do
  if health="$(curl -fsS http://127.0.0.1:4649/v1/health 2>/dev/null)"; then
    say "Running: $health"
    say "Log: $log"
    exit 0
  fi
  sleep 0.25
done
say "The daemon did not answer; see $log"
exit 1
