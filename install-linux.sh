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
# systemd's %E: the user manager's XDG_CONFIG_HOME, else ~/.config.
config_home="$(systemctl --user show-environment 2>/dev/null | sed -n 's/^XDG_CONFIG_HOME=//p')"
unit_dir="${config_home:-$HOME/.config}/systemd/user"
env_file="${config_home:-$HOME/.config}/oshirase/env"
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

command -v curl >/dev/null || { say "curl is needed for the health check"; exit 1; }

mkdir -p "$(dirname "$binary")" "$unit_dir"
# 0700: the email backend keeps its tokens beside the env file.
mkdir -p -m 0700 "$(dirname "$env_file")"
chmod 0700 "$(dirname "$env_file")"

# The env file is the daemon's settings. A rerun keeps one that exists, hand
# edits included, unless OSHIRASE_LISTEN asks for new addresses.
if [[ -n ${OSHIRASE_LISTEN:-} || ! -f $env_file ]]; then
  listen="${OSHIRASE_LISTEN:-}"
  if [[ -z $listen ]]; then
    listen="127.0.0.1:$port"
    while read -r address; do
      listen+=",$address:$port"
    done < <(ip -4 -o addr show 2>/dev/null | awk '$2 ~ /^virbr/ { split($4, a, "/"); print a[1] }')
  fi
  {
    echo "# oshirase serve's settings (install-linux.sh). Addresses separated by"
    echo "# commas; a VM bridge's accepts connections from that bridge's network only."
    echo "OSHIRASE_LISTEN=$listen"
    echo "# The backend: auto (the desktop's notifications), email or log."
    echo "#OSHIRASE_BACKEND=email"
  } > "$env_file"
else
  say "Keeping $env_file"
fi
listen="$(sed -n 's/^OSHIRASE_LISTEN=//p' "$env_file" | tail -1)"
probe="${listen%%,*}"
probe="${probe:-127.0.0.1:$port}"

say "Installing $binary ($version), listening on ${listen:-the default address}"
install -m 0755 "$source_binary" "$binary"
install -m 0644 "$repo/systemd/oshirase.service" "$unit_dir/oshirase.service"

systemctl --user daemon-reload
systemctl --user enable oshirase.service >/dev/null 2>&1
systemctl --user restart oshirase.service

say "Waiting for the daemon"
for _ in $(seq 20); do
  if health="$(curl -fsS "http://$probe/v1/health" 2>/dev/null)"; then
    say "Running: $health"
    say "Log: journalctl --user -u oshirase"
    exit 0
  fi
  sleep 0.25
done
say "The daemon did not answer; see journalctl --user -u oshirase"
exit 1
