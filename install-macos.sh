#!/usr/bin/env bash
# Install oshirase as a launchd agent for the current user, or reinstall it
# over an existing one.
#
#   ./install-macos.sh                  build from this checkout, then install
#   ./install-macos.sh path/to/oshirase install a binary built elsewhere
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
label="dev.moroz.oshirase"
binary="$HOME/.local/bin/oshirase"
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
  stamp="$(git -C "$repo" describe --always --dirty 2>/dev/null || echo dev)"
  mkdir -p "$repo/build"
  (cd "$repo" && go build -ldflags "-X main.version=$stamp" -o build/oshirase .)
  source_binary="$repo/build/oshirase"
fi

say "Installing $binary"
mkdir -p "$(dirname "$binary")" "$(dirname "$plist")" "$(dirname "$log")"
install -m 0755 "$source_binary" "$binary"

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
