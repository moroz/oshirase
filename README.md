# oshirase

お知らせ, "notice". Carries notifications from a development VM to the
machine it runs on, so something inside the VM — an SSH key waiting for a
YubiKey touch, an agent waiting for input — can reach the person at the host.

One binary does both ends:

```sh
oshirase serve                       # on the host: the daemon
oshirase send "Touch the YubiKey" "git push origin"   # in the guest
```

The daemon keeps a stable HTTP protocol and hides how each platform shows a
notification: macOS through Oshirase.app, and a Linux desktop through its
freedesktop.org notification service (both below).

## Install on macOS

```sh
./install-macos.sh            # builds with Go from mise.toml, then installs
./install-macos.sh ./oshirase # or install a binary built elsewhere
```

That puts the binary at `~/.local/bin/oshirase`, loads the launchd agent
`dev.moroz.oshirase` (started at login, restarted if it exits), and logs to
`~/Library/Logs/oshirase.log`.

It also builds **Oshirase.app** (Xcode or its command-line tools needed) and
installs it to `~/Applications`. The daemon shows every notification through
it: a small signed app using Apple's UserNotifications framework, listed as
**oshirase** in System Settings → Notifications. The installer launches it
once so macOS asks for permission then, not when the first notification
arrives.

When notifications for oshirase are turned off, or its alert style is None,
the app refuses and the daemon answers 502 with the reason, so the caller
knows nothing was shown. Focus is the exception: macOS does not tell an app
whether a Focus is holding its notifications back.

The app is signed with `$OSHIRASE_SIGN_IDENTITY`, else the first "Apple
Development" identity in the keychain, else ad hoc.

## Install on Linux

```sh
./install-linux.sh            # builds with Go from mise.toml, then installs
./install-linux.sh ./oshirase # or install a binary built elsewhere
```

That puts the binary at `~/.local/bin/oshirase` and installs the systemd user
service `oshirase.service` (started with the session, restarted if it exits);
it logs to the journal (`journalctl --user -u oshirase`).

The daemon shows notifications through the freedesktop.org notification
service on the session bus, which GNOME, Cinnamon, KDE, XFCE, dunst and mako
all provide. It calls it with `busctl`, which comes with systemd, so it needs
no D-Bus library. The source goes on the first line of the body, in italics
where the server renders markup, since the specification has no subtitle.
`critical` asks the server to keep the notification until it is dismissed.
With no notification service running, the daemon answers 502 with the
reason.

The installer has the daemon listen on loopback and on every libvirt bridge
(`virbr*`) address, written to `~/.config/oshirase/env` as
`OSHIRASE_LISTEN`; set `OSHIRASE_LISTEN` when running the installer to choose
the addresses yourself. A host firewall usually drops a guest's connections
to the host, so allow the port on the bridge, e.g. with ufw:

```sh
sudo ufw allow in on virbr0 to 192.168.122.1 port 4649 proto tcp comment 'oshirase from libvirt guests'
```

## Install in the guest

```sh
GOBIN=~/.local/bin go install .
```

`GOBIN` is set because mise points Go's own default at the directory of the
current Go release, which goes away with the next upgrade.

## Versions

Releases are git tags, `vMAJOR.MINOR.PATCH`. Go stamps the version into the
binary from the tag, so there is nothing to bump in the source:
`oshirase version` prints `v0.1.0` for a build at that tag, a pseudo-version
past it for later commits, and `+dirty` for uncommitted changes. The daemon
reports the same string in `GET /v1/health`.

The protocol has its own version, in the URL (`/v1`), and changes far less
often than the binary.

## Reaching it from the guest

The daemon listens on `127.0.0.1:4649` by default, so nothing else on the
network can reach it. A Lima guest reaches the host's loopback as
`host.lima.internal`, which is `oshirase send`'s default. A libvirt guest
cannot reach the host's loopback; the daemon listens on the bridge as well
(`-listen` takes several addresses separated by commas, and
`install-linux.sh` sets that up), and the guest sets
`OSHIRASE_URL=http://192.168.122.1:4649`, its gateway. Elsewhere, set
`OSHIRASE_URL` (or `-url`).

`oshirase send` exits non-zero when the daemon did not answer or refused, so a
caller can fall back to something else.

## Protocol (v1)

Fields may be added to v1; none are removed or change meaning. Unknown fields
are ignored.

`POST /v1/notify`, `Content-Type: application/json`:

| Field | | |
|---|---|---|
| `title` | required | up to 256 characters |
| `body` | optional | up to 4096 characters |
| `urgency` | optional | `low`, `normal` (default) or `critical` |
| `source` | optional | what sent it, e.g. `ssh`; up to 64 characters |
| `tag` | optional | groups notifications for `POST /v1/withdraw`, e.g. `ssh-touch`; up to 64 characters |

| Status | Body |
|---|---|
| 200 | `{"delivered": true, "backend": "macos"}` |
| 400, 413, 415 | `{"error": "..."}` — the request is wrong |
| 403 | the request carried an `Origin` header |
| 502 | `{"error": "..."}` — the platform refused to show it |

`POST /v1/withdraw`, `Content-Type: application/json`, `{"tag": "..."}`
takes down every notification sent with that tag that may still be on
screen, e.g. a "touch your key" notice once the key has been touched
(`oshirase withdraw -tag ssh-touch`). It answers
`{"withdrawn": 2, "backend": "freedesktop"}`, counting what it closed, or
501 from a backend that cannot withdraw (macOS, for now).

`GET /v1/health` → `{"protocol": 1, "version": "...", "backend": "..."}`.

Any request with an `Origin` header is refused, and both POST endpoints require a
JSON content type, so a web page open on the host cannot send notifications
through the daemon.

## Licence

BSD 3-Clause; see `LICENSE`.
