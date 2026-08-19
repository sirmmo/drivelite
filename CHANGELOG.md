# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-08-19

First public release.

### Added

- Browse a mounted folder in grid or list view, with breadcrumbs and sorting
  by name, date or size. Folders always sort first; dotfiles are hidden.
- On-demand thumbnails for JPEG, PNG and GIF, cached to disk outside the
  served tree and keyed by path, modification time, size and target width.
- EXIF orientation support, so photos straight off a camera are not shown
  rotated or mirrored.
- Lightbox viewer with keyboard navigation (`←`, `→`, `Esc`) and inline
  playback for browser-supported video and audio. AVCHD (`.MTS`, `.M2TS`) is
  detected and offered as a download instead of a player that cannot work.
- Per-file downloads and whole-folder streaming ZIP archives, storing
  already-compressed formats without re-deflating them.
- Recursive filename search from the current folder, capped at 400 results.
- HTTP Range support, so video seeking works and interrupted downloads resume.
- Minimal authentication: a shared password with an HMAC-signed session
  cookie, plus HTTP Basic for command-line use. Constant-time comparison and
  per-IP rate limiting on login.
- Optional multi-user mode via `DRIVELITE_USERS`, and an explicit
  `DRIVELITE_ALLOW_ANONYMOUS` opt-in for public deployments.
- JSON listing endpoint at `/api/list`, and `/healthz` for probes.
- Light and dark themes, responsive down to phone widths.
- Multi-architecture container image (amd64, arm64) running as a non-root
  user with all capabilities dropped.

### Security

- Path containment is enforced lexically and re-checked after symlink
  resolution; symlinks leaving the root are skipped in listings entirely.
- Only recognised media is served inline. HTML, SVG and unknown types are
  forced to `Content-Disposition: attachment` so they cannot execute in the
  app's origin.
- A strict Content-Security-Policy is applied, and the UI contains no inline
  script or style.
- Startup fails when no credentials are configured, and when the thumbnail
  cache would be written inside the served tree.

[Unreleased]: https://github.com/sirmmo/drivelite/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/sirmmo/drivelite/releases/tag/v0.1.0
