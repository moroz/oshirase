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
notification. Today that is macOS (Notification Center, through `osascript`).

## Install on macOS

```sh
./install-macos.sh            # builds with Go from mise.toml, then installs
./install-macos.sh ./oshirase # or install a binary built elsewhere
```

That puts the binary at `~/.local/bin/oshirase`, loads the launchd agent
`dev.moroz.oshirase` (started at login, restarted if it exits), and logs to
`~/Library/Logs/oshirase.log`.

macOS files `osascript` notifications under **Script Editor**: allow it in
System Settings → Notifications if nothing appears.

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

The daemon listens on `127.0.0.1:4649` only, so nothing else on the network
can reach it. A Lima guest reaches the host's loopback as
`host.lima.internal`, which is `oshirase send`'s default. Elsewhere, set
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

| Status | Body |
|---|---|
| 200 | `{"delivered": true, "backend": "osascript"}` |
| 400, 413, 415 | `{"error": "..."}` — the request is wrong |
| 403 | the request carried an `Origin` header |
| 502 | `{"error": "..."}` — the platform refused to show it |

`GET /v1/health` → `{"protocol": 1, "version": "...", "backend": "..."}`.

Any request with an `Origin` header is refused, and `/v1/notify` requires a
JSON content type, so a web page open on the host cannot send notifications
through the daemon.

## Licence

BSD 3-Clause; see `LICENSE`.
