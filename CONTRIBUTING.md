# Contributing to drivelite

Thanks for taking the time to look at this. drivelite is small on purpose, so
the most useful contributions are usually focused ones: a bug fix, a sharper
error message, a format that should be recognised.

## Getting set up

You need Go 1.23 or newer. There are no third-party Go dependencies, so there
is nothing to download beyond the toolchain itself.

```bash
git clone https://github.com/sirmmo/drivelite.git
cd drivelite

mkdir -p share && cp ~/some/photos/* share/     # anything to look at
DRIVELITE_ROOT=./share \
DRIVELITE_CACHE_DIR=./cache \
DRIVELITE_PASSWORD=dev \
go run .
```

Then open <http://localhost:8080> and sign in with `dev`.

`make help` lists the shortcuts for building, testing and running in Docker.

## Before you open a pull request

```bash
make check      # gofmt, go vet, and the full test suite with -race
```

CI runs the same checks plus `staticcheck`, a cross-compile across six
platforms, and a container smoke test. Please make sure `make check` is clean
locally first — it is much faster than waiting on a red build.

- **Format with `gofmt`.** CI fails on unformatted files.
- **Add a test** for behaviour changes. The security-sensitive areas —
  path containment in `internal/vault`, session handling in `internal/auth` —
  should never change without a test that would have caught the old bug.
- **Keep it dependency-free** if you reasonably can. Staying on the standard
  library is what keeps the image at ~20 MB and the build trivial. A new
  dependency is not forbidden, but please say in the PR why it earns its place.
- **Update the docs** when you change configuration or behaviour: `README.md`
  for the reference, `docs/index.html` for the published page, and add a line
  to `CHANGELOG.md` under "Unreleased".

## Commit messages

Short imperative subject, a blank line, then the reasoning if it is not
obvious. Explaining *why* matters more than restating the diff.

```
Serve .mp4 as video/mp4 without /etc/mime.types

Alpine ships no MIME database and Go's built-in table omits .mp4, so
videos were sent as application/octet-stream and would not play inline.
```

## Reporting bugs

Open an issue with the version (or image tag), how drivelite is configured,
and what you expected instead. If it involves a particular file, the extension
and rough size usually matter more than the contents.

## Security issues

Please do not open a public issue for a vulnerability. See
[SECURITY.md](SECURITY.md) for how to report one privately.

## Scope

drivelite is a **read-only viewer**. Uploads, renaming, deleting and editing
are deliberately out of scope — that boundary is what lets it mount the data
volume read-only and make a simple promise about never touching your files.
Ideas that keep it a viewer are very welcome.

By contributing, you agree that your work is licensed under the MIT License.
