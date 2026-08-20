# drivelite

[![CI](https://github.com/sirmmo/drivelite/actions/workflows/ci.yml/badge.svg)](https://github.com/sirmmo/drivelite/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/sirmmo/drivelite?sort=semver)](https://github.com/sirmmo/drivelite/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/sirmmo/drivelite.svg)](https://pkg.go.dev/github.com/sirmmo/drivelite)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A small Go service that exposes storage as a Google-Drive-style gallery:
browse folders, preview images and video, and download files or whole folders
as ZIP — behind a minimal login.

It can serve **a local folder**, **a git repository**, or **an S3-compatible
bucket**, with the same interface over all three.

Single static binary, no third-party Go dependencies, ~30 MB container image
(20 MB of that is drivelite; the rest is the `git` client the git backend shells out to).

**Documentation: <https://ingmmo.com/drivelite>**

```
docker run -d -p 8080:8080 \
  -v /path/to/your/folder:/data:ro \
  -e DRIVELITE_PASSWORD='pick-something-good' \
  ghcr.io/sirmmo/drivelite:latest
```

Then open <http://localhost:8080> and sign in.

## What it does

- **Browse** — grid or list view, breadcrumbs, sort by name / date / size.
  Folders always sort first. Dotfiles are hidden.
- **Thumbnails** — generated on demand for JPEG, PNG and GIF, cached to disk,
  EXIF-orientation aware so camera photos are never shown sideways.
- **Lightbox** — click an image to open a full viewer; `←`/`→` move between
  items, `Esc` closes. Video and audio play inline where the browser supports
  the format.
- **Download** — per file, or the current folder as a streaming ZIP.
- **Search** — substring match on filename, recursive from the current folder.
- **Keyboard** — `/` focuses the search box.

## Backends

Pick one with `DRIVELITE_BACKEND`, or let it be inferred: setting
`DRIVELITE_S3_BUCKET` selects `s3`, setting `DRIVELITE_GIT_URL` selects `git`,
and otherwise it serves a local folder.

### Local folder (default)

```bash
docker run -d -p 8080:8080 \
  -v /srv/photos:/data:ro \
  -e DRIVELITE_PASSWORD='secret' \
  ghcr.io/sirmmo/drivelite:latest
```

### Git repository

The repository is cloned into the cache directory and served from the working
tree, so thumbnails, ZIP and search all behave exactly as for a folder. A
background timer re-fetches; if a refresh fails, the previous checkout keeps
being served.

```bash
docker run -d -p 8080:8080 \
  -e DRIVELITE_BACKEND=git \
  -e DRIVELITE_GIT_URL=https://github.com/owner/repo.git \
  -e DRIVELITE_GIT_REF=main \
  -e DRIVELITE_GIT_INTERVAL=5m \
  -e DRIVELITE_PASSWORD='secret' \
  ghcr.io/sirmmo/drivelite:latest
```

For a private repository set `DRIVELITE_GIT_TOKEN`. The token is passed to git
as a per-command HTTP header and is **never written to `.git/config`**, so it
cannot end up inside the tree being served.

### S3-compatible bucket

Works with AWS S3, MinIO, Ceph, Garage, Backblaze B2 and Cloudflare R2.
Requests are signed with SigV4 implemented on the standard library — no AWS
SDK. Range requests are translated into ranged `GET`s, so video seeking works
without downloading the whole object.

```bash
docker run -d -p 8080:8080 \
  -e DRIVELITE_BACKEND=s3 \
  -e DRIVELITE_S3_ENDPOINT=https://s3.eu-west-1.amazonaws.com \
  -e DRIVELITE_S3_BUCKET=my-photos \
  -e DRIVELITE_S3_REGION=eu-west-1 \
  -e DRIVELITE_S3_ACCESS_KEY=AKIA... \
  -e DRIVELITE_S3_SECRET_KEY=... \
  -e DRIVELITE_PASSWORD='secret' \
  ghcr.io/sirmmo/drivelite:latest
```

Leave the key and secret unset to read a public bucket anonymously.

## Configuration

All settings are environment variables. Only the password is required.

### Common

| Variable | Default | Meaning |
| --- | --- | --- |
| `DRIVELITE_BACKEND` | inferred | `local`, `git` or `s3`. |
| `DRIVELITE_CACHE_DIR` | `/cache` | Thumbnails and git checkouts. Must be outside a local `ROOT`. |
| `DRIVELITE_ADDR` | `:8080` | Listen address. |
| `DRIVELITE_TITLE` | `Drive` | Name shown in the header. |
| `DRIVELITE_PASSWORD` | — | Password for the single account. |
| `DRIVELITE_USER` | `admin` | Username paired with `DRIVELITE_PASSWORD`. |
| `DRIVELITE_USERS` | — | Multiple accounts: `alice:secret,bob:hunter2`. |
| `DRIVELITE_ALLOW_ANONYMOUS` | `false` | Serve with no login at all. Opt-in. |
| `DRIVELITE_SESSION_KEY` | random | HMAC key. Set it to keep sessions across restarts. |
| `DRIVELITE_SESSION_HOURS` | `168` | Session lifetime. |
| `DRIVELITE_SECURE_COOKIE` | `false` | Set `true` when serving over HTTPS. |
| `DRIVELITE_THUMB_PX` | `400` | Longest edge of a generated thumbnail. |
| `DRIVELITE_THUMB_JOBS` | `4` | Concurrent thumbnail decodes. |
| `DRIVELITE_ENABLE_ZIP` | `true` | Allow folder-as-ZIP downloads. |
| `DRIVELITE_MAX_ZIP_MB` | `0` | Refuse ZIPs above this size. `0` = unlimited. |

### Local backend

| Variable | Default | Meaning |
| --- | --- | --- |
| `DRIVELITE_ROOT` | `/data` | Folder to serve. Mount it read-only. |

### Git backend

| Variable | Default | Meaning |
| --- | --- | --- |
| `DRIVELITE_GIT_URL` | — | Repository to clone. Required. |
| `DRIVELITE_GIT_REF` | remote default | Branch, tag or commit. |
| `DRIVELITE_GIT_SUBDIR` | — | Serve only this directory of the repository. |
| `DRIVELITE_GIT_INTERVAL` | `5m` | Re-fetch interval. Minimum `1m`; `0` disables. |
| `DRIVELITE_GIT_DEPTH` | `1` | Shallow clone depth. `0` for a full clone. |
| `DRIVELITE_GIT_USERNAME` | `x-access-token` | Username paired with the token. |
| `DRIVELITE_GIT_TOKEN` | — | Access token for a private repository. |

### S3 backend

| Variable | Default | Meaning |
| --- | --- | --- |
| `DRIVELITE_S3_ENDPOINT` | — | e.g. `https://s3.eu-west-1.amazonaws.com`. Required. |
| `DRIVELITE_S3_BUCKET` | — | Bucket name. Required. |
| `DRIVELITE_S3_PREFIX` | — | Serve only this prefix of the bucket. |
| `DRIVELITE_S3_REGION` | `us-east-1` | Signing region. |
| `DRIVELITE_S3_ACCESS_KEY` | — | Omit both keys to read a public bucket. |
| `DRIVELITE_S3_SECRET_KEY` | — | |
| `DRIVELITE_S3_SESSION_TOKEN` | — | For temporary STS credentials. |
| `DRIVELITE_S3_PATH_STYLE` | `true` | Bucket in the path. Set `false` for AWS-style virtual hosts. |
| `DRIVELITE_S3_CACHE_TTL` | `1m` | How long listings are reused. `0` disables caching. |
| `DRIVELITE_S3_TIMEOUT` | `60s` | Response-header timeout per request. |

The service **refuses to start** with no password unless
`DRIVELITE_ALLOW_ANONYMOUS=true` is set explicitly, so an unauthenticated
deployment is always a deliberate choice rather than an oversight.

## docker compose

`docker-compose.yml` defines one service per backend, plus a MinIO instance so
the S3 example runs out of the box:

```bash
docker compose up folder    # a local directory
docker compose up repo      # a git repository
docker compose up bucket    # an S3 bucket, backed by MinIO
```

## Authentication

Deliberately minimal — a shared password, not an identity system.

- A login form issues an **HMAC-signed session cookie** (`HttpOnly`,
  `SameSite=Lax`, `Secure` when configured). The cookie carries only a username
  and expiry; nothing is stored server side.
- **HTTP Basic** also works, for `curl` and scripts:
  `curl -u admin:pass 'http://host:8080/dl?p=folder/file.jpg' -O`
- Password comparison is **constant-time**, and unknown usernames run the same
  comparison work so timing does not reveal which accounts exist.
- Failed logins are **rate limited per IP** (burst of 8, refilling at one
  attempt per 4s).
- Removing a user from the config immediately invalidates their sessions.

## Security notes

- **The backing store is never written to.** Thumbnails and git checkouts go
  to a separate cache directory, and startup fails if that cache is inside a
  local served root. Mount the data volume `:ro`.
- **Dotfiles are refused, not merely hidden.** Any path with a dot-prefixed
  component returns "not found" — hiding a name from the listing while still
  serving it on request is no protection, and for the git backend `.git/`
  holds repository internals.
- **Path containment** is enforced lexically and re-checked *after* symlink
  resolution, so a symlink inside the tree cannot point outside it. Symlinks
  escaping the root are skipped in listings entirely. An S3 prefix scopes every
  operation the same way.
- **Only media is served inline.** HTML, SVG, text and anything unrecognised
  are sent as `Content-Disposition: attachment` so they cannot execute in the
  app's origin. Every response carries `X-Content-Type-Options: nosniff`.
- **A strict Content-Security-Policy** is applied; the UI uses no inline
  scripts or styles.
- The container runs as a **non-root user** with no capabilities.

This is a read-only viewer with one shared password. It is a good fit behind a
reverse proxy or on a trusted network; put HTTPS in front of it (and set
`DRIVELITE_SECURE_COOKIE=true`) before exposing it to the internet.

## Format support

Thumbnails require a decoder in the binary, so they cover **JPEG, PNG and GIF**.
Other images (HEIC, WebP, AVIF, RAW) still list and download normally, and
display if the browser itself supports them — they just show a type icon
instead of a preview.

Inline playback follows what browsers actually support: MP4, WebM, OGV, and the
common audio formats. **AVCHD camcorder files (`.MTS`, `.M2TS`) cannot play in
any browser**, so the viewer detects them and offers a download button instead
of a dead player.

`.ts` is treated as an MPEG transport stream rather than TypeScript. The
extension is genuinely ambiguous; a TypeScript file still lists and downloads
correctly, it just shows a video icon.

## Endpoints

| Path | Purpose |
| --- | --- |
| `/`, `/browse?p=` | Folder listing |
| `/search?q=&p=` | Recursive filename search |
| `/raw?p=` | File contents, inline, with HTTP Range support |
| `/dl?p=` | File contents as a download |
| `/thumb?p=&w=` | Cached thumbnail |
| `/zip?p=` | Folder as a streaming ZIP |
| `/api/list?p=` | Directory listing as JSON |
| `/healthz` | Liveness probe |

Paths travel in the `p` query parameter rather than the URL path, so filenames
containing `#`, `%` or spaces work correctly.

Range support means video seeking works, and interrupted downloads resume.

## Building

```bash
docker build -t drivelite:latest .     # container
go build -o drivelite .                # local binary, Go 1.23+
make check                             # gofmt, vet, tests with -race
```

The git backend shells out to `git`, which the container image includes. The
other two backends have no external requirements.
