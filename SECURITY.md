# Security Policy

## Reporting a vulnerability

Please report security issues **privately**, not through a public issue.

- Preferred: open a [private security advisory](https://github.com/sirmmo/drivelite/security/advisories/new)
- Or email **marco.montanari@gmail.com**

Please include what an attacker can achieve, the steps to reproduce it, and
the version or image tag you tested. A proof of concept helps enormously.

This is a spare-time project, so I cannot promise a service-level response,
but I will acknowledge reports as quickly as I can and credit you in the fix
unless you prefer otherwise.

## Supported versions

The latest release on `main` is the supported one. Fixes go into a new release
rather than being backported.

## What drivelite defends against

These are treated as security bugs and are worth reporting:

- **Path escape.** Any way to read a file outside the configured root — via
  `..`, encoding tricks, or a symlink pointing out of the tree. Containment is
  checked lexically *and* again after symlink resolution.
- **Authentication bypass.** Reaching any protected route without valid
  credentials, forging or replaying a session cookie, or extending a session
  past its expiry.
- **Content injection.** Getting the server to serve attacker-controlled
  active content (HTML, SVG, JavaScript) inline in the app's origin. Only
  recognised media is sent inline; everything else is forced to download, and
  a strict Content-Security-Policy is applied.
- **Writes to the served tree.** drivelite is read-only. Anything that causes
  it to create, modify or delete a file under the root is a bug.

## What is out of scope

These are known and intentional properties, not vulnerabilities:

- **Anyone with the password sees the whole tree.** There are no per-folder
  permissions, and no attempt at one. drivelite has one shared credential.
- **`DRIVELITE_ALLOW_ANONYMOUS=true` disables authentication.** That is what
  it is for. The service refuses to start unauthenticated unless you set it.
- **Plain HTTP by default.** drivelite does not terminate TLS. Run it behind a
  reverse proxy and set `DRIVELITE_SECURE_COOKIE=true`. Over plain HTTP the
  password and session cookie travel in the clear.
- **Passwords come from the environment.** They are visible to anyone who can
  read the process environment or your compose file. There is no secret store.
- **No brute-force lockout.** Login attempts are rate limited per IP (a burst
  of 8, refilling at one per 4s), which slows guessing but does not stop a
  determined distributed attempt. Use a strong password.
- **Resource exhaustion by an authenticated user.** A logged-in user can ask
  for large ZIP archives or many thumbnails. Use `DRIVELITE_MAX_ZIP_MB` and
  `DRIVELITE_THUMB_JOBS` to bound it.

## Hardening checklist

- Mount the data volume read-only: `-v /your/folder:/data:ro`
- Put HTTPS in front, then set `DRIVELITE_SECURE_COOKIE=true`
- Set a long random `DRIVELITE_SESSION_KEY` so sessions survive restarts
- Keep the container's dropped capabilities and non-root user as shipped
- Set `DRIVELITE_MAX_ZIP_MB` if the tree is large
