# Security policy

## Reporting a vulnerability

Report it privately through GitHub: open the repository's **Security** tab,
choose **Report a vulnerability**, and fill in the draft security advisory
([direct link](https://github.com/sofired/treerig/security/advisories/new)).
Only you and the maintainers can see it.

Please include:

- the treerig version (`treerig --version`) and how you installed it;
- your operating system and architecture;
- your Docker and Docker Compose versions (`docker version`,
  `docker compose version`);
- the steps to reproduce it;
- the impact you expect, and what an attacker would need to start with.

Please do not disclose the problem publicly, in an issue, a pull request or
elsewhere, until a fix has been released. If you have had no reply in 14 days,
say so on the advisory. If a fix is taking long, we will agree a disclosure
date with you.

Bugs that are not security problems go to the normal
[issue tracker](https://github.com/sofired/treerig/issues). If you are not
sure which kind yours is, report it privately.

## What to expect

treerig is a small project. We will acknowledge your report and keep you
informed through the advisory as we confirm, fix and release. We give no
guaranteed response or fix times.

A fix ships in a new release. Once it is out, we publish the advisory, and we
credit you by name or handle unless you ask us not to.

## Supported versions

Before 1.0, only the newest release gets security fixes, including a
pre-release if that is the newest. What the install docs call the "latest"
release never includes a pre-release (see
[Pre-releases](docs/install.md#pre-releases)), so check the
[Releases page](https://github.com/sofired/treerig/releases).

## Scope

In scope:

- Anything that lets branch code run during `worktree remove` despite the
  teardown check ([Teardown pin](docs/teardown-pin.md)). The check protects
  development builds, which refuse to remove without a pinned, verified teardown
  binary, and anyone who pins, whose pin every build then uses. A way around it
  counts: a way to make removal run something other than the bytes the pin names
  (a control home redirected into a checkout, say), a development build that
  removes without a pin and without `--dev-teardown`, or a way to turn
  `--dev-teardown` on without the flag. A release build without a pin runs its own
  (released) code by design, so that is not a report. A build whose version was
  stamped to look like a release, by hand, by `-ldflags` or `GOFLAGS` from an
  environment a branch controls (direnv, mise, a devcontainer), or by a build
  wrapper, is trusted as a release when no pin exists; treerig does not defend
  against that, and the mitigation is to pin, because a usable pin overrides it for
  every build. `v0.1.0-rc.2` predates the check
  and removes with itself, so a report about it is not new. Reports about the pin
  commands themselves are welcome too.
- Removal of a Docker resource that treerig cannot prove it owns by its
  ownership labels.
- The router, the index page, a routed service's pinned port, or a shared
  service's declared ports being reachable beyond loopback (`127.0.0.1`).
- A bypass of the cosign signature check or the checksum check in
  `treerig teardown pin --release` or `--latest`.
- A bypass of the checksum check in `install.sh`. `install.sh` checks the
  archive's sha256 against `checksums.txt` and does not verify the signature.
- The integrity of `install.sh`, the release archives and the packages. The
  packages are covered by `checksums.txt` and its signature, not by a
  package-manager signature. The Homebrew formula in `sofired/homebrew-tap` is
  in scope as a release artifact; report it here.
- Path traversal or link tricks in archive extraction.
- A write, to a path treerig manages, that follows a symlink or lands outside
  the control home or the router's state directory.
- A file or directory created with looser permissions than documented
  (control-home directories `0700`, the pin file `0600`, lock targets `0600`
  with a `0700` parent). The router's dynamic and index directories and files
  are `0755` and `0644` by design: the router reads them, and they hold no
  secrets.
- A lock cleared without an explicit, checked recovery, for example by age
  alone.

Out of scope:

- A hostile process already running as your user. It can already rewrite the
  teardown pin and anything else under your home.
- Anyone who can already control your Docker daemon. Access to the Docker
  socket is equivalent to root in the Docker engine's environment (on Linux,
  the host).
- The security of your own Compose services, images, hooks and `[commands]`.
  Bringing up a worktree runs its branch's code, as you; see the
  [Trust model](docs/trust-model.md), which also lists what is open to other
  local users by design (for example, a routed service's loopback port and
  the index page).
- Listeners and `ports:` entries that your own config or processes create, such
  as a host-route dev server bound to `0.0.0.0` or a service's own `ports:`.
- A hostile `PATH` that supplies `cosign` from outside the checkouts treerig
  checks and the control home. This is a documented limit of `teardown pin`;
  see [What it does not protect against](docs/teardown-pin.md#what-it-does-not-protect-against).
- A `teardown pin --file` pin is exactly as trustworthy as the digest you
  checked.
- Vulnerabilities in third-party tools such as Docker, Docker Compose,
  Traefik, Git and cosign. Please report those to their own projects.

## Verifying a release

To check that a download is the one this repository's release workflow built,
follow [Verifying a download](docs/install.md#verifying-a-download).
