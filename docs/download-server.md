# Download server

**Usually not needed any more.** A PAI server can fetch a private repository's releases itself and serve them to its machines: `pai-server source github <owner/name> --token-file <file>` (see [self-hosting.md](self-hosting.md#where-releases-come-from)). Workers update from the server, and new machines install from `<server URL>/install.sh`. Moving the server to another address then needs no new release. A download server is still useful to install the *first* PAI server of a private repository, or for machines that cannot reach a PAI server.

Serve PAI releases from your own machine instead of GitHub, for a private repository whose release files nobody can download without logging in. The code stays private; installs and updates come from the download server.

```
private repo → release workflow builds + signs → private GitHub release
  → download server syncs it (only it holds a token) → https://<host>/pai/<version>/<file>
  → installers, pai update, the server's update and worker updates download from there
```

Every file is still checked against the release's signed `SHA256SUMS`, so the download server only has to be available: it cannot make a machine run a release the key did not sign. (The install scripts check the signature too when OpenSSL 3 is there, else `SHA256SUMS` only, so serve over HTTPS.)

Works for releases built after this change. Older releases have the GitHub address built in.

## 1. The repository

In the private repository's **Settings → Secrets and variables → Actions**:

| Name | Kind | Value |
|---|---|---|
| `RELEASE_SIGNING_KEY` | secret | from `go run ./pkg/release/keygen` (a new pair: machines then accept only this repo's releases) |
| `RELEASE_PUBLIC_KEY` | variable | the public half it prints |
| `PAI_DOWNLOAD_BASE` | variable | `https://<host>.<tailnet>.ts.net/pai`, must start with `https://` |

A tag then builds a release whose binaries and install scripts use `PAI_DOWNLOAD_BASE`. Without the variable, releases come from GitHub as before.

## 2. The download server (Linux)

Needs `gh` and Tailscale. Make a fine-grained GitHub token for the private repository only, with **Contents: read-only**.

```sh
sudo install -d -m 755 /srv/pai-releases /opt/pai-download
sudo install -m 755 deploy/download-server/sync.sh /opt/pai-download/
sudo sh -c 'umask 077; echo "GH_TOKEN=<token>" > /etc/pai-download.env'

sudo tee /etc/systemd/system/pai-download-sync.service >/dev/null <<'EOF'
[Service]
Type=oneshot
EnvironmentFile=/etc/pai-download.env
ExecStart=/opt/pai-download/sync.sh <owner>/<repo> /srv/pai-releases
EOF
sudo tee /etc/systemd/system/pai-download-sync.timer >/dev/null <<'EOF'
[Timer]
OnBootSec=1min
OnUnitActiveSec=10min
[Install]
WantedBy=timers.target
EOF
sudo systemctl daemon-reload && sudo systemctl enable --now pai-download-sync.timer

# HTTPS for the tailnet, with a certificate Tailscale provides
sudo tailscale serve --bg --set-path /pai /srv/pai-releases
```

`journalctl -u pai-download-sync` shows each run: `added <version>`, `latest is <version>`, or `FAILED <version>` (that release is skipped and retried next run; the others carry on).

## 2b. The download server (Windows)

```powershell
New-Item -ItemType Directory -Force C:\pai-releases, C:\pai-download | Out-Null
Copy-Item deploy\download-server\sync.ps1 C:\pai-download\
[Environment]::SetEnvironmentVariable('GH_TOKEN', '<token>', 'Machine')
$a = New-ScheduledTaskAction powershell.exe '-NoProfile -ExecutionPolicy Bypass -File C:\pai-download\sync.ps1 -Repo <owner>/<repo> -Dir C:\pai-releases'
$t = New-ScheduledTaskTrigger -Once -At (Get-Date) -RepetitionInterval (New-TimeSpan -Minutes 10)
Register-ScheduledTask pai-download-sync -Action $a -Trigger $t -User SYSTEM -RunLevel Highest
tailscale serve --bg --set-path /pai C:\pai-releases
```

## Layout

| Path | Holds |
|---|---|
| `<version>/` | one release: binaries, install scripts, `SHA256SUMS`, `SHA256SUMS.sig`, `VERSION`, `CHANGELOG.md` |
| `latest/` | the newest stable (non-beta) release, as GitHub's `releases/latest` is |

## Installing

```sh
curl -fsSL https://<host>.<tailnet>.ts.net/pai/latest/install.sh | sh -s -- server
```
```powershell
& ([scriptblock]::Create((irm https://<host>.<tailnet>.ts.net/pai/latest/install.ps1))) server
```

A beta has no `latest/`: use its version path, `.../pai/v0.5.0-beta.2/install.sh`. The server's own install lines for workers and keys use the download server too.

## Changing the address

The address is built into each release. To move (for example to Cloudflare), keep serving the old address until every machine runs a release built with the new one.

## Rollback

Delete the `PAI_DOWNLOAD_BASE` variable: the next release comes from GitHub again. Machines on a download-server release keep updating from the download server until they install a release built without it.
