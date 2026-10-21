# Teardown pin

Install and check the pinned treerig build that `worktree remove` can run.

A pin keeps code from a branch from running while `worktree remove` takes a
worktree apart ([`design/teardown.md`](design/teardown.md), #85, #146). Whether
you need one depends on the build you run:

| Build | No pin | A pin |
| --- | --- | --- |
| A release (`0.1.0-rc.3`, `1.2.3`: a version cut from a release tag) | Removes with itself. Nothing to set up. | Removal runs the pinned, verified binary instead. |
| A development build (from source, a snapshot, anything `0.0.0-…`) | **Refuses**, unless `--dev-teardown`. | Removal runs the pinned, verified binary. |

A usable pin wins for every build: it is checked (its sha256, where it lives, who
owns it) and its binary is run in place of the one you invoked, with the same
arguments and environment. A pin that is broken (missing or different binary,
foreign or newer format, no binary for this platform) refuses, whatever the build.
`worktree remove --dev-teardown` removes with the build you invoked and skips
the pin, with a warning.

**When to pin a release.** Pinning means removal keeps running that exact
verified build, even after you upgrade treerig. That is worth having when you
want removal fixed to a version you checked (a shared machine, or a build you
verified by hand), or when you also run development builds and want them to remove
with a release. If you only run releases, you can ignore this page.

## Getting a pin

A development build needs one. Two routes:

1. With cosign 3.0.0 or newer on your `PATH`: `treerig teardown pin --release
   0.1.0-rc.2` (a published version; the build's own, once it is a release).
2. Without cosign, or offline: verify a download by hand
   ([Verifying a download](install.md#verifying-a-download)), take the
   `treerig` binary's own digest with `sha256sum treerig` (`shasum -a 256
   treerig` on macOS), and run `treerig teardown pin --file ./treerig --sha256
   <that digest> --release <a label>`.

Then `treerig teardown status` shows whether removal will run it.

The pin lives under the control home, in `teardown/`: `pin.json` and the pinned
binaries, each named by its sha256. The control home is `control-home/` under
the treerig root (`~/.treerig` by default) unless `TREERIG_CONTROL_HOME` moves it, and
`treerig doctor` prints it. There is one pin per control home, shared by every
treerig root that uses it. Nothing pins, updates or unpins on its own, and
nothing downloads except `teardown pin --release` or `--latest`, which you run.

## `teardown pin --release` and `--latest`

Download a release, verify it, and pin the `treerig` inside it:

```bash
treerig teardown pin --release 0.1.0-rc.2     # a version, as the tag without the v
treerig teardown pin --latest                 # the newest release that is not a pre-release
```

`--latest` only finds the version: it reads `releases/latest/download/checksums.txt`
for the version in the archive names, discards it, and then behaves as
`--release` with that version, fetching everything again from
`https://github.com/sofired/treerig/releases/download/vX.Y.Z/`. It never selects a
pre-release, so while every release is one it refuses and tells you to name the
release with `--release`. `--release` and `--latest` exclude each other.

Verification needs **cosign 3.0.0 or newer** on your `PATH`. treerig finds it once
and runs that path for both steps, so check what `PATH` finds. It checks
`checksums.txt` with `cosign verify-blob` against the release workflow's exact
identity for that tag
(`https://github.com/sofired/treerig/.github/workflows/release.yml@refs/tags/vX.Y.Z`,
issued by `https://token.actions.githubusercontent.com`), so another tag's
signature does not verify. Only then does it read `checksums.txt`, compare the
archive for your platform with its line there, extract `treerig` (the one regular
entry named `treerig`; a link, an absolute or `..` name, a second `treerig` or none
refuses the archive) and check that it is an executable for this machine. The pin
records the archive digest of all four platforms and the extracted binary's
digest for yours. Downloads are HTTPS only, redirects included, with size limits
(1 MiB for `checksums.txt` and the signature, 100 MiB for the archive) and a
10-minute limit for the lot; your `HTTPS_PROXY` settings apply. Nothing of the
pin is written to the control home until you approve the plan (cosign's own
directories under `<control home>/teardown/cosign/` are made earlier: a cache,
not the pin), which shows the release,
source, signer, the cosign that ran and its version, and your platform's digest.

treerig refuses a cosign that lives inside a checkout (the current one, its
repository, or any registered worktree) or inside the control home, since branch
code could have put it there. What it checks is the current checkout and its
repository, the worktrees that repository lists (`.git/worktrees`, registered
with treerig or not), the worktrees treerig has registered (found through the index
card `up` writes, so one that never ran `up` and is not listed is not checked)
and the control home; if it cannot work these out it refuses. cosign runs with
a fixed environment, not yours: `PATH`, `LANG` and `LC_*`, the `HTTP_PROXY`,
`HTTPS_PROXY` and `NO_PROXY` variables (either case) and `SSL_CERT_FILE` and
`SSL_CERT_DIR`, plus a `HOME`, `TUF_ROOT` and `TMPDIR` of treerig's own under
`<control home>/teardown/cosign/`, so no `COSIGN_*` or `SIGSTORE_*` setting in a
checkout's environment can choose the trust root or switch a check off.
`ALL_PROXY` is not passed. cosign fetches its trust root on the first run and
keeps it there. A cosign elsewhere on a hijacked `PATH` is trusted. A cosign that is an asdf or mise shim is not supported: it needs the `HOME` and variables treerig withholds, so install a cosign binary on `PATH`. Run `teardown pin` from a shell you trust. The pin reaches `github.com`,
`release-assets.githubusercontent.com` (where GitHub redirects the downloads) and
`tuf-repo-cdn.sigstore.dev` (cosign). When cosign fails, treerig says which kind: a
signature or identity failure means do not trust that release; only a trust root
that could not be loaded suggests `cosign initialize` once with network access,
or verifying by hand and using `teardown pin --file`.

Pinning the same release for another platform merges into the pin; any other
release replaces it, a downgrade included, and the plan says so. The pin does not
run the binary: the first time it is executed is by the removal gate, which is
also where a filesystem that cannot run programs is reported.

## `teardown pin --file`

Pin a treerig binary you have verified yourself, such as one you built. For a
download, first verify the archive by the steps under
[Verifying a download](install.md#verifying-a-download), then extract `treerig` from it and
take that binary's own digest:

```bash
sha256sum treerig     # macOS: shasum -a 256 treerig
treerig teardown pin --file ./treerig --sha256 <that digest> --release my-build
```

The digest you give approves exactly the bytes you hold: treerig hashes the file,
requires the same digest, and stores those bytes. It also checks that the file is
an executable for this machine (an ELF file on Linux, a Mach-O file on macOS, for
this architecture) and refuses an empty file or a binary for another platform;
beyond that, the digest is the approval. It is a meaningful check only
when the digest comes from a second trusted source, such as your own build; a
digest taken from the file you are about to pin proves nothing more than that
you meant to pin it. `--release` is a label for the pin: letters, digits, `.`,
`_`, `+` and `-`, starting with a letter or digit, at most 64 characters. The
command shows what it will pin and asks you to type `pin teardown binary`;
`--yes` skips the phrase, and without a terminal it refuses to ask. A file pin
records only this machine's platform, so another platform pins its own binary.
Pinning again replaces the pin, a downgrade included. There is no unpin: delete
`<control home>/teardown/`.

No signature is checked, cosign is not run, and no program is run. For a file
pin `--release` is only the label. To pin a downloaded release with cosign's
verification, use `--release X.Y.Z` or `--latest` above.

## `teardown status`

Reports whether the pin is intact on this machine: whether it can be read, and
whether the stored binary for this platform exists and has the sha256 the pin
records. It prints the pin on stdout and exits 0 when all is well. The report
ends with a line that says what removal would do:

- `worktree remove: will run this binary`, with an intact pin;
- `worktree remove: would refuse`, for a pin that is broken, and for a
  development build with no pin; it adds `(see the warning below)` when the
  reason is one of the warnings the command prints on stderr (a control home
  inside a git work tree, a pin store others can write, a `noexec` mount; they
  appear with `--json` too, and do not change the exit code);
- `worktree remove: will run this build (a release; no pin needed)`, for a
  release build with no pin.

With no pin, a release build exits 0 (stderr says none is needed and how to
pin one) and a development build exits 1 with an error. A pin of a newer
format, or one treerig did not write, prints nothing on stdout and exits 1; a
missing or different binary prints the report first and exits 1.

With `--json` it prints one document instead:
`pinned`, `ok`, `release`, `source`, `signer` (absent for a file pin),
`pinnedAt`, `platform`, `binary` (`path` and `sha256`, absent when the state is
`none`, and `state`: `ok`, `missing`, `mismatch` or `none` for a platform the
pin has no binary for) and `running` (`version`, `isPin`: this build is the
pinned binary, and `newerThanPin`). `mismatch` also covers a stored binary that
cannot be read or is not a regular file; the text form says which. With no pin
it prints `{"pinned": false}` (exit 0 for a release build, 1 for a development
build). When the running build is newer than the pinned release, the text form
adds a line on stderr saying to pin again.

## `worktree remove` and the pin

`worktree remove` is gated before it does anything else, before it takes a lock
or reads `treerig.toml`. With no pin and no `--dev-teardown`, a release build
removes with itself (relocated into a copy under the control home first, so
deleting its checkout cannot delete the running program), and a development
build refuses. Otherwise:

- With an intact pin it prints `Running the pinned teardown binary <path>
  (release <release>) to remove <this worktree | the worktree X>.` on stderr,
  followed by the newer-build sentence below when it applies, and replaces itself
  with the stored copy. The program name you invoked it as (`trig` or `treerig`)
  and your arguments pass through, and so does your environment without the
  `LD_*`, `DYLD_*`, `GODEBUG` and `GOFLAGS` variables, which change what a
  program loads or how its runtime behaves before its first line runs. The pinned
  binary prints `Running as the pinned teardown binary (version <version>).`, does
  the removal, asks the confirmation, and exits with its own status. The file
  that runs is the one whose sha256 was just checked, not a path that could be
  swapped in between: on Linux it is run through `/proc/self/fd`, so `/proc` must
  be mounted; on macOS, where a file cannot be run through an open descriptor,
  the path is checked again immediately before.
- It refuses, removes nothing, and says what to run when a development build has
  no pin; whatever the build, when the pin is of a newer format or not one
  treerig wrote; the stored binary is missing
  or has other bytes; the pin has no binary for this machine's platform; or the
  pin store is not trustworthy, below. When the build you invoked is newer than
  the pin, the refusal says so, as `teardown status` does, and so does the line
  it prints before running an older pin. Its commands run as written: a released
  build names its own version, and a pin that is broken is pinned again from its
  own release, or from its own `--file`.
- **The pin store must be private to you.** The control home is refused when it,
  or its `teardown/` directory, is inside a git work tree. A branch could commit
  its own `pin.json` and a binary named by its sha256, and a `.envrc` could point
  `TREERIG_CONTROL_HOME` at them. The control home, `teardown/`, `pin.json` and
  the stored binary are also refused unless you own them and neither their group
  nor others can write them, which is why `sudo treerig worktree remove` refuses.
  The one exemption is a dotfiles repository whose work tree is your home
  directory: it is recognised from the home directory the user database lists
  for you, never from `$HOME`, which a checkout can set, and only when that is
  the same directory as `$HOME`. Where the database cannot be read (a user not in
  `/etc/passwd` on a Linux release build), there is no exemption, and a control
  home under a home directory that is itself a work tree is refused: set
  `TREERIG_CONTROL_HOME` to a directory outside any work tree. A work tree rooted
  at your real home directory is therefore not guarded. `teardown status` and
  `doctor` report all of this before a removal does.
- **A filesystem that cannot run programs.** If the exec fails with `permission
  denied` or `exec format error`, the control home is probably on a `noexec`
  mount: remount it with `exec`, or move the control home with
  `TREERIG_CONTROL_HOME`. Pinning never runs the binary, so this first shows at
  a removal, but `teardown pin` and `teardown status` warn when they can see
  that the mount is `noexec`.
- A flag error or `--help` still comes from the build you invoked, which
  parses the command line first; neither removes anything.

When a development build has no pin, the refusal says so and gives both routes,
on lines of their own: `treerig teardown pin --release <a published version, such
as 0.1.0-rc.2>`, or `treerig teardown pin --file <a treerig you trust> --sha256
<its sha256> --release <a label>`, with `treerig teardown pin --help` for both. A
release build names its own version where a message tells it to pin
(`treerig teardown pin --release 0.1.0-rc.3`).

**What counts as a release build.** One strict rule decides (`semver.IsPublishedRelease`):
the version is a release version such as `0.1.0` or `0.1.0-rc.2`, with no `+build`
part, not `0.0.0` in any form, and not the tail `git describe` adds past a tag
(`0.1.0-rc.2-5-gabc1234`). Release builds, GoReleaser archives and Homebrew's formula
(which stamps the tag's version) stamp such a version; a plain `go build` or `go
install` reports `0.0.0-dev`, and a snapshot `0.0.0-dev+<sha>`, so they are development
builds. Someone who stamps a release-looking version on a build by hand gets past
the rule; treerig does not defend against a hostile process of the same user.

`worktree remove --dev-teardown` removes with the build you invoked instead (any
build, release or development, pinned or not), for work on removal itself, and prints a warning on every run naming its version and
sha256 (`unknown` when the build cannot be read, in which case it goes on). It is
a per-invocation flag and nothing else: no setting, no `treerig.toml` key, no
environment variable, and nothing remembers it. The pinned binary itself refuses
it.

A pin made by a release that predates the gate (`v0.1.0-rc.2`) runs the old
relocation of its own bytes and then removes; those are the pinned bytes, so
nothing unpinned runs. Pin the first release that contains the gate to have the
pinned binary enforce it too.

## What it does not protect against

The pin is meant to keep unreviewed branch code from running as treerig during a
removal. It does not protect against a hostile process running as you with write
access to the control home, which can rewrite the pin as easily as anything else
there, and a file pin is exactly as trustworthy as the digest you checked.
