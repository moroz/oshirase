#!/usr/bin/env bash
# Install oshirase as a systemd user service on a Linux desktop, or reinstall
# it over an existing one.
#
#   ./install-linux.sh                  build from this checkout, then install
#   ./install-linux.sh path/to/oshirase install a binary built elsewhere
#
# The daemon shows notifications through the desktop's freedesktop.org
# notification service, over D-Bus with busctl (systemd). It listens on
# loopback and on every libvirt bridge (virbr*) address, port 4649, so a
# libvirt guest reaches it at its gateway, e.g. 192.168.122.1. Set
# OSHIRASE_LISTEN to choose the addresses yourself.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
binary="$HOME/.local/bin/oshirase"
unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
env_file="${XDG_CONFIG_HOME:-$HOME/.config}/oshirase/env"
port=4649

say() { printf '==> %s\n' "$*" >&2; }

if [[ "$(uname -s)" != Linux ]]; then
  say "This installs a systemd user service; run it on Linux."
  exit 1
fi
command -v busctl >/dev/null || say "busctl is missing; the daemon will refuse every notification"

if [[ $# -ge 1 ]]; then
  source_binary="$1"
else
  say "Building"
  # Go stamps the version from the git tag itself (see buildVersion).
  mkdir -p "$repo/build"
  (cd "$repo" && go build -o build/oshirase .)
  source_binary="$repo/build/oshirase"
fi
# On its own line so set -e stops here if the binary cannot run.
version="$("$source_binary" version)"

listen="${OSHIRASE_LISTEN:-}"
if [[ -z $listen ]]; then
  listen="127.0.0.1:$port"
  while read -r address; do
    listen+=",$address:$port"
  done < <(ip -4 -o addr show 2>/dev/null | awk '$2 ~ /^virbr/ { split($4, a, "/"); print a[1] }')
fi

say "Installing $binary ($version), listening on $listen"
mkdir -p "$(dirname "$binary")" "$unit_dir" "$(dirname "$env_file")"
install -m 0755 "$source_binary" "$binary"
install -m 0644 "$repo/systemd/oshirase.service" "$unit_dir/oshirase.service"
printf 'OSHIRASE_LISTEN=%s\n' "$listen" > "$env_file"

systemctl --user daemon-reload
systemctl --user enable oshirase.service >/dev/null 2>&1
systemctl --user restart oshirase.service

say "Waiting for the daemon"
for _ in $(seq 20); do
  if health="$(curl -fsS "http://127.0.0.1:$port/v1/health" 2>/dev/null)"; then
    say "Running: $health"
    say "Log: journalctl --user -u oshirase"
    exit 0
  fi
  sleep 0.25
done
say "The daemon did not answer; see journalctl --user -u oshirase"
exit 1
