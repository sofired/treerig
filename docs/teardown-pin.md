# Teardown pin

Install and check the pinned treerig build that `worktree remove` is meant to run.

`worktree remove` is meant to run a pinned, trusted build of treerig, not the
code of whichever build you invoked, so that code from a branch does not run
while it takes a worktree apart ([`design/teardown.md`](design/teardown.md),
#85). **Only the pin exists so far.** `worktree remove` does not run the pinned
binary yet and does not refuse without one (#146); nothing here changes what it
does. The commands below install a pin and check it.

The pin lives under the control home, in `teardown/`: `pin.json` and the pinned
binaries, each named by its sha256. The control home is `control-home/` under
the treerig root (`~/.treerig` by default) unless `TREERIG_CONTROL_HOME` moves it, and
`treerig doctor` prints it. There is one pin per control home, shared by every
treerig root that uses it. Nothing pins, updates or unpins on its own, and
nothing downloads except `teardown pin --release` or `--latest`, which you run.

## `teardown pin --release` and `--latest`

Download a release, verify it, and pin the `treerig` inside it:

```bash
treerig teardown pin --release 0.1.0-rc.1     # a version, as the tag without the v
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
10-minute limit for the lot; your `HTTPS_PROXY` settings apply. Nothing is
written to the control home until you approve the plan, which shows the release,
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
run the binary: the first time it is executed is by the removal gate (#146),
which is also where a filesystem that cannot run programs will be reported.

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

Reports whether the pin is intact on this machine, and nothing about removal:
whether it can be read, and whether the stored binary for this platform exists
and has the sha256 the pin records. It prints the pin on stdout and exits 0 when
all is well. When there is no pin, the pin is of a newer format or is not one
treerig wrote, or the binary is missing or different, it says so on stderr and
exits 1; the first two print nothing on stdout, and the others print the report
first. With `--json` it prints one document instead:
`pinned`, `ok`, `release`, `source`, `signer` (absent for a file pin),
`pinnedAt`, `platform`, `binary` (`path` and `sha256`, absent when the state is
`none`, and `state`: `ok`, `missing`, `mismatch` or `none` for a platform the
pin has no binary for) and `running` (`version`, `isPin`: this build is the
pinned binary, and `newerThanPin`). `mismatch` also covers a stored binary that
cannot be read or is not a regular file; the text form says which. With no pin
it prints `{"pinned": false}` and exits 1. When the running build is newer than
the pinned release, the text form adds a line on stderr saying to pin again.

## What it does not protect against

The pin is meant to keep unreviewed branch code from running as treerig during a
removal. It does not protect against a hostile process running as you with write
access to the control home, which can rewrite the pin as easily as anything else
there, and a file pin is exactly as trustworthy as the digest you checked.
