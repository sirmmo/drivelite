# drivelite

[![CI](https://github.com/sirmmo/drivelite/actions/workflows/ci.yml/badge.svg)](https://github.com/sirmmo/drivelite/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/sirmmo/drivelite?sort=semver)](https://github.com/sirmmo/drivelite/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/sirmmo/drivelite.svg)](https://pkg.go.dev/github.com/sirmmo/drivelite)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A small Go service that turns any mounted folder into a Google-Drive-style
gallery: browse folders, preview images and video, and download files or whole
folders as ZIP — behind a minimal login.

Single static binary, no third-party Go dependencies, ~20 MB container image.

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
- **Lightbox** — click an image to open a full viewer; arrow keys and `←`/`→`
  move between items, `Esc` closes. Video and audio play inline where the
  browser supports the format.
- **Download** — per file, or the current folder as a streaming ZIP.
- **Search** — substring match on filename, recursive from the current folder.
- **Keyboard** — `/` focuses the search box.

## Configuration

All settings are environment variables. Only the password is required.

| Variable | Default | Meaning |
| --- | --- | --- |
| `DRIVELITE_ROOT` | `/data` | Folder to serve. Mount it read-only. |
| `DRIVELITE_CACHE_DIR` | `/cache` | Thumbnail cache. Must be outside `ROOT`. |
| `DRIVELITE_ADDR` | `:8080` | Listen address. |
| `DRIVELITE_TITLE` | `Drive` | Name shown in the header. |
| `DRIVELITE_PASSWORD` | — | Password for the single account. |
| `DRIVELITE_USER` | `admin` | Username paired with `DRIVELITE_PASSWORD`. |
| `DRIVELITE_USERS` | — | Multiple accounts: `alice:secret,bob:hunter2`. Overrides the two above. |
| `DRIVELITE_ALLOW_ANONYMOUS` | `false` | Serve with no login at all. Opt-in. |
| `DRIVELITE_SESSION_KEY` | random | HMAC key for session cookies. Set it to keep sessions across restarts. |
| `DRIVELITE_SESSION_HOURS` | `168` | Session lifetime. |
| `DRIVELITE_SECURE_COOKIE` | `false` | Set `true` when serving over HTTPS. |
| `DRIVELITE_THUMB_PX` | `400` | Longest edge of a generated thumbnail. |
| `DRIVELITE_THUMB_JOBS` | `4` | Concurrent thumbnail decodes. |
| `DRIVELITE_ENABLE_ZIP` | `true` | Allow folder-as-ZIP downloads. |
| `DRIVELITE_MAX_ZIP_MB` | `0` | Refuse ZIPs above this size. `0` = unlimited. |

The service **refuses to start** with no password unless
`DRIVELITE_ALLOW_ANONYMOUS=true` is set explicitly, so an unauthenticated
deployment is always a deliberate choice rather than an oversight.

## docker compose

```yaml
services:
  drivelite:
    image: drivelite:latest
    ports: ["8080:8080"]
    volumes:
      - ./share:/data:ro
      - thumbs:/cache
    environment:
      DRIVELITE_PASSWORD: "change-me"
      DRIVELITE_TITLE: "Shared Files"
volumes:
  thumbs:
```

See `docker-compose.yml` for a fuller example.

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
- Removing a user from the config immediately invalidates their existing
  sessions.

If no `DRIVELITE_SESSION_KEY` is set, a random one is generated at startup,
which simply means sessions do not survive a restart.

## Security notes

- **The served folder is never written to.** Thumbnails go to a separate cache
  directory, and startup fails if that cache is inside the served root. Mount
  the data volume `:ro`.
- **Path containment** is enforced twice: the request path is lexically cleaned
  (neutralising `..`), then the resolved path is checked again *after* symlink
  resolution, so a symlink inside the tree cannot point outside it. Symlinks
  escaping the root are skipped in listings entirely.
- **Only media is served inline.** HTML, SVG and anything unrecognised are sent
  as `Content-Disposition: attachment` so they cannot execute in the app's
  origin. Every response carries `X-Content-Type-Options: nosniff`.
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
```
