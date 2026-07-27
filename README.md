# FileBadara

> 파일 받아라.

*Badara* (받아라) is Korean for "take it", so the name reads as "take the file" —
and the tagline adds that nothing is left behind on the server.

**Send someone a download link for a file on your machine, without uploading it
anywhere first.**

Run one command next to the file and it prints a URL. Send that URL to anyone.
When they open it, the bytes stream from your machine through the FileBadara
server to them — the server relays the transfer and never keeps a copy.

```console
$ curl -fsSL https://badara.example.com/sh | sh -s -- ./archive.tar.zst
https://badara.example.com/Uqaaf7f_gxSPjXYUPqdE5HhM/archive.tar.zst
```

Leave that command running for as long as the link should work.

`badara.example.com` above is a FileBadara server you run yourself — there is no
hosted service. It is a single static binary with no database and no storage.

**Why bother**

- The file never sits on someone else's disk, so nothing outlives the transfer.
- No account and no upload wait — the link works the moment you run the command.
- The server needs no storage at all, so a $5 VPS can relay files of any size.

## What each side needs

Nobody installs a FileBadara client. The sender helper is a short script the
server prints on request, and the downloader just fetches a URL.

| Role | Needs |
| --- | --- |
| Downloader | Any HTTP client — `curl`, `wget`, or a browser. |
| Sender on Unix | A POSIX shell and `curl`. The helper also calls `wc` for the file size. |
| Sender on Windows | PowerShell and `curl.exe`, which ships with Windows 10 1803 and later. |
| Server operator | The `filebadara` binary. No runtime, database, or disk space. |

Nothing is fetched from a package registry, and you can read the helper before
running it with `curl https://badara.example.com/sh`.

## How it works

Three parties: you (the sender), the FileBadara server, and whoever you sent the
link to. Your command holds a connection open to the server, waiting to be told
that someone wants the file. Each time that happens, it uploads the file again on
a fresh connection, and the server pipes those bytes straight to that one
downloader.

```text
Downloader A ── GET ──> FileBadara <── PUT A ── sender curl A
Downloader B ── GET ──> FileBadara <── PUT B ── sender curl B
```

So two people downloading at once get two separate uploads from you, and a slow
downloader never slows down anyone else. Nothing is ever written to disk on the
server, and no single stream is shared between downloaders.

## Quick start

**1. Build and run the server**

```bash
make build
./filebadara -addr :8080
```

**2. Ask the server how to use it**

The root URL prints the sender commands with this server's hostname already filled
in, so there is nothing to edit by hand:

```console
$ curl http://SERVER:8080/
FileBadara

Unix:
  curl -fsSL http://SERVER:8080/sh | sh -s -- FILE

PowerShell:
  & ([scriptblock]::Create((irm http://SERVER:8080/ps))) FILE

The sender command stays open while the URL is available.
Each download starts a separate upload stream from the sender.
```

**3. Share a file** — on the sender's machine

```console
$ curl -fsSL http://SERVER:8080/sh | sh -s -- ./archive.tar.zst
http://SERVER:8080/Uqaaf7f_gxSPjXYUPqdE5HhM/archive.tar.zst
```

Leave it running. It re-uploads the file for every download and prints nothing more.

**4. Download** — on anyone else's machine

```bash
curl -O http://SERVER:8080/Uqaaf7f_gxSPjXYUPqdE5HhM/archive.tar.zst
```

## Sending a file

Open the server's root URL at any time to see these commands with the correct hostname filled in:

```bash
curl http://SERVER/
```

### Unix

```bash
curl -fsSL http://SERVER/sh | sh -s -- ./archive.tar.zst
```

### Windows PowerShell

```powershell
& ([scriptblock]::Create((irm http://SERVER/ps))) .\archive.tar.zst
```

The helper prints one download URL and then stays open. Every request to that URL starts another upload of the same local file. Stop the helper to end sharing.

## Running the server

### Plain HTTP

The default listen address is `:80`, which needs root or `CAP_NET_BIND_SERVICE`:

```bash
sudo ./filebadara
```

Use `-addr` for an unprivileged port instead:

```bash
./filebadara -addr :8080
```

### Automatic HTTPS

Point the domain's A or AAAA record at the server, allow inbound TCP ports 80 and 443, then:

```bash
./filebadara -domain badara.example.com
```

FileBadara obtains and renews the TLS certificate automatically through ACME. Port 80 serves the ACME challenge and redirects to HTTPS. Certificates are cached in `.filebadara-certs` unless `-cert-cache` says otherwise — set it to a persistent directory in production.

### Upload password

A password may never travel over plain HTTP, so `-password-file` works only together with `-domain`. Combining it with plain HTTP is not a warning — the server exits before it listens:

```console
$ ./filebadara -password-file /etc/filebadara/password
-password-file requires -domain because passwords must use HTTPS
```

```bash
./filebadara \
  -domain badara.example.com \
  -password-file /etc/filebadara/password
```

Only the creation of a sharing URL is protected; downloaders never need the password. The helper scripts add `curl -u upload`, so `curl` prompts the sender for it. Because the helpers are served with the `https://` base URL baked in, the password only ever leaves the sender over TLS. Responses also carry `Strict-Transport-Security`, so clients that honour HSTS will not downgrade later.

See [Production deployment](#production-deployment) for creating the password file safely.

## Options

| Flag | Default | Purpose |
| --- | --- | --- |
| `-addr` | `:80` | HTTP listen address. Ignored when `-domain` is set. |
| `-domain` | *(empty)* | Domain name. Enables automatic HTTPS on ports 80 and 443. |
| `-ttl` | `10m` | Sharing URL lifetime. Any Go duration (`90s`, `30m`, `2h`); must be greater than zero. |
| `-password-file` | *(empty)* | File containing the upload password. Requires `-domain`. |
| `-cert-cache` | `.filebadara-certs` | ACME certificate cache directory. |
| `-version` | | Print the version and exit. |

## Production deployment

An example systemd unit is included at [`deploy/filebadara.service`](deploy/filebadara.service).

**1. Create the service account and config directory**

```bash
sudo useradd --system --home /var/lib/filebadara --shell /usr/sbin/nologin filebadara
sudo install -d -o filebadara -g filebadara -m 700 /etc/filebadara
```

**2. Set an upload password (optional)**

Type the password, then press Ctrl-D:

```bash
sudo install -m 600 -o filebadara -g filebadara /dev/stdin /etc/filebadara/password
```

**3. Install the binary and unit**

```bash
sudo install -m 0755 filebadara /usr/local/bin/filebadara
sudo install -m 0644 deploy/filebadara.service /etc/systemd/system/filebadara.service
```

**4. Set the domain, then start**

Edit the `-domain` and `-password-file` arguments in the unit before starting it.

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now filebadara
```

## Behavior and limits

**Transfers**

- Every download creates an independent sender-to-server upload.
- One sharing URL supports concurrent and repeated downloads while it is valid.
- A download that already started keeps going past the URL's expiry.
- Range requests work, so `curl -C -` and `wget -c` can resume a broken download.

**Limits**

- The sender helper must keep running; closing it ends the transfer.
- Restarting the server invalidates all sharing URLs and active transfers.

### Range requests

The server answers a single `Range` request with `206 Partial Content`, and reports
`416` with `Content-Range: bytes */SIZE` for a range past the end of the file.
Multi-range requests are answered with the whole body, which RFC 9110 permits.

A range costs the sender only the bytes that are actually needed. The helper reports
the file size when it creates the sharing URL, so the server can resolve the range
before it asks for anything, and then tells the sender which offset to start at:

```text
downloader ── GET Range: bytes=160000- ──> FileBadara
                                              │  "start at 160000"
                                              v
                                           sender ── curl -C 160000 ──> 40000 bytes
```

Resuming a 200 KB download at 80% transfers 40 KB on both legs, not 200 KB on the
sender's. Two consequences follow from knowing the size up front:

- An impossible range returns `416` without waking the sender at all.
- A bounded range such as `bytes=1000-1999` still lets the sender run to the end of
  the file, because `curl` can seek to an offset but cannot stop early. The server
  cuts the tail off, so the overshoot is capped by the connection buffer rather than
  by the file size. Open-ended ranges, which is what resuming uses, are exact.

## Routes

| Route | Purpose |
| --- | --- |
| `/` | Usage instructions |
| `/sh` | Unix upload helper |
| `/ps` | PowerShell upload helper |
| `/new` | Internal helper endpoint that creates a sharing URL |
| `/{token}/{filename}` | Public download URL |

The `/wait` and `/upload` routes are private implementation details protected by random secrets.

## Security notes

- A password cannot be used in plain HTTP mode; FileBadara refuses to start rather than expose it. It also rejects an empty password file, so a typo can never silently leave uploads open to everyone.
- Do not hand-write `curl -u upload http://...` against an HTTPS deployment. `curl` sends Basic credentials on the first request, so the password would reach port 80 in the clear before the redirect to HTTPS. Use the `/sh` or `/ps` helpers, which always use the `https://` base URL.
- Keep the password file readable only by the FileBadara service account.
- Anyone holding a download URL can download the file while the URL is active.
- `curl | sh` and dynamic PowerShell execution run code returned by the server. Use only a server you trust, or inspect `/sh` or `/ps` before executing it.

## Build

Requires Go 1.26 or newer.

```bash
make build          # -trimpath, stripped, version stamped from git
go build -o filebadara ./cmd/filebadara
```

Other targets: `make test`, `make test-race`, `make vet`, `make fmt`, `make clean`.

`make test-race` is separate from `make test` because `-race` needs cgo and a C
compiler, which a plain Go install does not have.

`TESTFLAGS` is passed to every test target, so a per-test breakdown is:

```console
$ make test TESTFLAGS=-v
--- PASS: TestRangeDownload (0.01s)
    --- PASS: TestRangeDownload/middle (0.00s)
    --- PASS: TestRangeDownload/open_ended (0.00s)
    --- PASS: TestRangeDownload/suffix (0.00s)
...
```

Use it to narrow down too: `make test TESTFLAGS="-v -run TestRange"`.

`make cover` reports coverage per function, and `make cover-html` writes
`coverage.html` with the uncovered lines highlighted.

```console
$ make cover
ok  github.com/pjknkda/filebadara/internal/filebadara  coverage: 78.3% of statements
server.go:389:  parseByteRange      100.0%
server.go:241:  handleDownload       79.6%
...
```

### Testing the helper scripts

The `/sh` and `/ps` helpers are string constants, so a shell or PowerShell
mistake in them is invisible to ordinary Go tests. `make test-helpers` fetches
each script from a running server and executes it against a real file, then
checks a whole download, a repeat download, and a resumed one.

Each helper needs its interpreter, and skips when it is missing. CI asserts both
rather than allowing a silent skip: the Linux job runs the shell helper, and a
Windows job runs the PowerShell one, where `pwsh` and `curl.exe` are genuine.
Setting `FILEBADARA_REQUIRE_HELPERS=1` turns a missing interpreter into a
failure.

### Releases

Pushing a `v*` tag builds statically linked binaries for linux and darwin on
amd64 and arm64 and attaches them to a GitHub release, unpacked, alongside a
`SHA256SUMS` file. `make dist VERSION=v1.2.3` produces the same set locally.

Downloading one is a curl and a chmod, since a release asset does not carry the
executable bit:

```bash
curl -fsSLO https://github.com/pjknkda/FileBadara/releases/latest/download/filebadara_v1.2.3_linux_amd64
chmod +x filebadara_v1.2.3_linux_amd64
```

The tag is stamped into the binary:

```console
$ filebadara -version
filebadara v1.2.3 (go1.26.5 linux/amd64)
```

## License

MIT
