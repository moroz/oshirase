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

The service reads its settings from `~/.config/oshirase/env` (systemd's
`%E`, so `$XDG_CONFIG_HOME` if the user manager has one), which the
installer writes the first time and keeps afterwards:

- `OSHIRASE_LISTEN`: loopback and every libvirt bridge (`virbr*`) address at
  install time. Set `OSHIRASE_LISTEN` when running the installer to choose
  the addresses yourself (that rewrites the file).
- `OSHIRASE_BACKEND`: `email` to send mail instead of desktop notifications
  (see below), `log`, or unset for the desktop. `oshirase auth` must then
  write the grant where the service looks: the same config directory, or
  set `OSHIRASE_EMAIL_CONFIG` in this file to the path it used.

Anything that can connect to the daemon can show notifications and withdraw
them, and with the email backend send mail as you, so mind where it
listens. A listener on a specific non-loopback address, such as a bridge's,
accepts connections only from that address's own network (the guests on
that bridge) and loopback. Without that, Linux would also take a connection
for the bridge's address arriving from the LAN, from any machine that routes
the bridge's subnet through this host. Every guest on the bridge can still
reach it, not only the VM it is meant for. A wildcard address (`0.0.0.0`)
is not filtered. Bridge addresses are bound even before the bridge is up.

A host firewall usually drops a guest's connections to the host, so allow
the port on the bridge, e.g. with ufw:

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

## Email backend

```sh
oshirase serve -backend email
```

delivers every notification as an email instead of putting it on the
screen, for being reached away from the host. It works the same on macOS
and Linux. The subject is the title tagged with the source,
`[oshirase · ssh] Touch your security key`; the body is the body (or the
title again, when there is none). `critical` is marked `Importance: high`
and `X-Priority: 1`, `low` the opposite, so a mail client can sort or
alert on it. If the provider refuses — a revoked grant, an HTTP error — the
daemon answers 502 with the reason and what to run, e.g. `refresh token
revoked or expired (invalid_grant: ...); run oshirase auth google`.

Each grant is made once, interactively, **on the host** (the machine the
daemon runs on): `oshirase auth` prints a URL, you open it in a browser on
that machine, and the provider redirects back to a listener on
`127.0.0.1:<random port>`. The authorization-code flow uses PKCE. The
result goes to `email.json` in the user configuration directory:

| | |
|---|---|
| Linux | `$XDG_CONFIG_HOME/oshirase/email.json` (else `~/.config/oshirase/`) |
| macOS | `~/Library/Application Support/oshirase/email.json` |
| anywhere | `$OSHIRASE_EMAIL_CONFIG`, or `-config` on `oshirase auth` |

The file holds a long-lived credential, so it is written with mode 0600 in a
0700 directory, and the daemon warns if it finds it readable by others. It
is JSON (`provider`, `to`, `from`, `client_id`, `client_secret`,
`token_endpoint`, `refresh_token` or `api_token`, `jmap_session`). The
daemon reloads it when it changes, so re-running `oshirase auth` repairs a
revoked grant without a restart. The daemon refuses to start with
`-backend email` and no file. Neither it nor `oshirase auth` logs a token
or a message body.

### Gmail and Google Workspace

Sending goes through the Gmail API (`users.messages.send`) with the scope
`https://www.googleapis.com/auth/gmail.send`, which can send as you and
cannot read your mail. Google has no client for oshirase to share: you
create your own in a Google Cloud project of your own, and bring its id and
secret. A Desktop app client's "secret" is not secret in the usual sense —
Google treats installed apps as public clients and relies on PKCE and the
loopback redirect — but it is yours, not oshirase's.

1. In the [Google Cloud console](https://console.cloud.google.com/), create
   a project (or pick one) and enable the **Gmail API** for it.
2. **Google Auth platform → Branding / Audience**: configure the consent
   screen. With Google Workspace, choose **Internal**. With a personal
   account, choose **External** and add yourself as a test user.
3. **Google Auth platform → Clients → Create Client**, application type
   **Desktop app**. Download its JSON.
4. Run:

   ```sh
   oshirase auth google -credentials ~/Downloads/client_secret_….json -to you@example.com
   ```

   (or `-client-id` and `-client-secret`). `-from` is optional: Gmail sends
   as the account that granted access.

**Refresh tokens of an External app in "Testing" expire after 7 days**, so
the backend would stop with `invalid_grant` once a week. To avoid that, use
an Internal app (Workspace), or under **Audience** publish the app to
**In production**. gmail.send is a *sensitive* scope, so an unverified app
in production shows an "unverified app" warning on the consent screen,
which you click through once; verification is only needed to offer the app
to other people. A grant also lapses after six months unused, or when you
revoke it at myaccount.google.com/permissions.

### Fastmail

Fastmail implements the [OAuth Profile for Open Public
Clients](https://datatracker.ietf.org/doc/draft-ietf-mailmaint-oauth-public/):
a native app registers itself as a public client through dynamic client
registration (RFC 7591), with no client id, secret or agreement to obtain
from Fastmail first. Registering by hand with Fastmail is only for clients
with a public (https) redirect URI. So:

```sh
oshirase auth fastmail -to you@example.com
```

discovers the endpoints at
`https://api.fastmail.com/.well-known/oauth-authorization-server`,
registers a client, asks for the scopes `urn:ietf:params:oauth:scope:mail`
and `offline_access`, and sends you through the consent page. It then reads
the JMAP session with the new grant, to make sure it may send, and records
the sending identity (`-from`, else the account's first identity).

Fastmail rotates the refresh token on every use and revokes the grant if an
old one is used again. The daemon writes each new refresh token back to the
file before it sends, so **do not copy `email.json` to a second machine**:
the two would spend each other's tokens. Make one grant per machine.

If OAuth does not suit, a Fastmail **API token** does the same job without
it. Make one in Settings → Privacy & Security → API tokens with access to
**Email** (not read-only) and **Email submission**, then:

```sh
oshirase auth fastmail -api-token -to you@example.com < token.txt
```

An API token does not expire; revoke it on the same settings page.

### What it costs

An email is slower than a banner. The daemon's part is a token refresh
about once an hour and, for Fastmail, a JMAP session lookup on the first
notification, then one request to send: a few hundred milliseconds, within
the 10-second limit on a delivery. After that it is the provider's delivery
and the phone's push, typically seconds, sometimes a minute. Every
notification becomes a message — in Sent, and in the recipient's inbox,
`low` and `normal` ones included — so send to an address whose inbox you
are willing to fill, or filter on the `[oshirase` subject prefix.

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
`{"withdrawn": 2, "backend": "freedesktop"}`, counting the notifications it
asked the platform to close (some may already have been dismissed or have
expired; the platform does not say), or 501 from a backend that cannot
withdraw (macOS, for now). On Linux, notifications shown before the
notification server restarted are not closed: their ids may by then belong
to another application's notifications.

`GET /v1/health` → `{"protocol": 1, "version": "...", "backend": "..."}`.

Any request with an `Origin` header is refused, and both POST endpoints require a
JSON content type, so a web page open on the host cannot send notifications
through the daemon.

## Licence

BSD 3-Clause; see `LICENSE`.
