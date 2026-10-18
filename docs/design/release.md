# Release contract

What a treerig release publishes, and which of it other things depend on. The
pipeline is `.goreleaser.yaml` (what is built and named) and
`.github/workflows/release.yml` (who builds it and signs it); CONTRIBUTING.md
says how to cut one. Everything below is a contract with people and programs
that cannot be changed with the next release: **renaming `release.yml`,
changing a file-name template, or moving `treerig` inside the archive is a
migration, not a refactor.**

## The assets of release `vX.Y.Z`

The tag is the version with a leading `v`; file names carry the version without
it. All of them are attached to the GitHub Release.

| Asset | Name |
| --- | --- |
| Platform archive | `treerig_X.Y.Z_<os>_<arch>.tar.gz`, `<os>` `linux` or `darwin`, `<arch>` `amd64` or `arm64` |
| Source archive | `treerig-X.Y.Z.tar.gz`, everything under `treerig-X.Y.Z/` |
| Debian package | `treerig_X.Y.Z_<arch>.deb`, `<arch>` `amd64` or `arm64` |
| RPM package | `treerig-X.Y.Z-1.<arch>.rpm`, `<arch>` `x86_64` or `aarch64` |
| Alpine package | `treerig_X.Y.Z_<arch>.apk`, `<arch>` `x86_64` or `aarch64` |
| Checksums | `checksums.txt`: `<sha256>  <file name>` for every asset above |
| Signature | `checksums.txt.sigstore.json`: a Sigstore bundle for `checksums.txt` |

A platform archive holds exactly `treerig` (at the archive root, mode 0755),
`LICENSE`, `NOTICE`, `README.md` and `completions/` (`treerig.bash`, `_treerig`,
`treerig.fish`, and the same three for the alias: `trig.bash`, `_trig`,
`trig.fish`). The packages install `treerig` to `/usr/bin`, `trig` beside it as
a symlink, and the completions where bash, zsh and fish look for them. `packaging/verify-snapshot.sh` asserts
all of this on every pull request.

A signing or notarization step for the `treerig` binary, if one is ever added,
runs before archiving and checksumming, so every digest describes the signed
bytes; the teardown pin records those digests.

Two builds of one commit are byte-identical: files carry the commit's date and
root ownership, and the pull-request check rebuilds and diffs `checksums.txt`.

## The `trig` alias

`trig` is a short name for the same binary, not a second program: a symlink
named `trig` pointing at `treerig`. It is presentation only. It creates no new
trust boundary and no new storage name: labels, directories, environment
variables and the teardown pin all keep the `treerig` names.

`brand.CLIName` follows `argv[0]`, so run through the link, the help text, the
usage lines and the `[trig]` message prefix say `trig`. Hints and stored text
keep naming `treerig` by design (`brand.Name`; for one, the missing-config hint
in `internal/config/load.go`, which is not handed the invoked name). The
completion scripts are generated per name, so `trig` has its own;
`completions.sh` writes them by running the binary through a `trig` symlink.
`worktree remove`'s relocation and the future removal gate (#146) exec a copy
with the caller's `argv[0]`, so the name survives the exec.

Where the link is made, and what each route does about a clash:

| Route | The link | A clash |
| --- | --- | --- |
| Homebrew | `bin.install_symlink "treerig" => "trig"` | A foreign `trig` in Homebrew's `bin` makes `brew install treerig` fail at the link step, and may leave treerig unlinked. Remove the other `trig`, then run `brew link --overwrite treerig`. |
| `install.sh` | `trig` beside `treerig` in the install directory | Skipped, with the reason on stderr, when another `trig` is on `PATH` or `trig` already exists in the directory (a file, a broken link, a link elsewhere). `treerig` is installed either way. `TREERIG_NO_ALIAS=1` (any non-empty value except `0`) skips it. |
| `.deb`, `.rpm`, `.apk` | `/usr/bin/trig`, a relative symlink, always | A `/usr/bin/trig` owned by another package makes the whole treerig install fail (dpkg says "trying to overwrite", and apt can be left half-unpacked). An unowned `/usr/bin/trig`, such as a hand-copied script, is silently replaced. |
| Platform archive | none | Archives hold regular files and users unpack them by hand; the install docs say to run `ln -s treerig trig`. |

**Decision: the packages ship the link unconditionally.** There is no
pre-install script, `Conflicts` or `Replaces`: a script that probes for a
`trig` would run as root on every install to guard against a name no distro
package uses (checked October 2026), and `Conflicts` would block installing
the other package for no reason. The residual risk is the two clashes in the
table. It is accepted because no distro ships a `trig`, hand-installed
commands belong in `/usr/local/bin`, which precedes `/usr/bin` on `PATH`, and
a clash fails loudly instead of corrupting anything. Revisit it if a distro
ever packages a `trig`.

The archive carries the `trig` completion scripts anyway, so a user who makes
the link by hand has them. A change to any of these spellings must change all
of them: `internal/style/release_files_test.go` checks that they agree, and
`packaging/verify-snapshot.sh` checks the deb, rpm and apk and `install.sh`
end to end. The rpm and apk list the link with mode `l---------`; that is
known and harmless, since a symlink's mode is not used, but it has not been
checked on Alpine. User docs for the alias (the install docs, after #170) are
written before any release ships it.

## Who signed it

`checksums.txt` is signed keyless with cosign. The certificate's identity is
the release workflow at the tag, and its issuer is GitHub's OIDC:

```
identity: https://github.com/sofired/treerig/.github/workflows/release.yml@refs/tags/vX.Y.Z
issuer:   https://token.actions.githubusercontent.com
```

Every file in `checksums.txt` also has a GitHub build provenance attestation
from the same workflow (`gh attestation verify <file> --repo sofired/treerig`).
The workflow's file name is part of the identity a verifier pins, so a rename
breaks every verification instruction already published. The repository path
in the identity is `brand.Repository`, a trust root: the download source and
the signing identity both derive from it, so changing it is a migration, like
a storage name, and the identity literal pinned in
`internal/releasefetch/cosign_test.go` changes deliberately with it.

The signing tool is pinned: `release.yml` sets the installer's `cosign-release:`
to `v3.0.6`, which cosign-installer v4.1.2 installed by default when it was
pinned. `go.yml`'s `release-snapshot` job installs the same cosign, with the
same installer commit and `cosign-release:`, to run the teardown pin flow
against the snapshot; the two files change together, and
`TestCosignPinnedTheSameInBothWorkflows` (`internal/style`) fails if they
differ. The bundle it writes is what the teardown pin's minimum, cosign 3.0.0,
reads, so **changing the pinned cosign is a migration for the pin**: raise it
together with the pin's minimum and its tests. Dependabot bumps the installer
action but not `cosign-release:`, so raising it is a manual step, in both
files. Nothing the pipeline emits is signed by a package-manager key.

## What depends on it

- **`install.sh`** builds the archive name from the platform and version,
  reads `checksums.txt` to find the latest version (from the archive names in
  it) and the archive's digest, and extracts `treerig` from the archive root.
  It does not check the signature. The script is served from `main`
  (`raw.githubusercontent.com`) or from a tag, so a change to it ships on merge,
  not with a release.
- **The Homebrew formula** in `sofired/homebrew-tap` builds from the source
  archive; the release workflow takes its digest from `checksums.txt`. The
  template is `packaging/homebrew/treerig.rb`, and
  `packaging/homebrew/set-source.py` points it at a release. The tap job and
  the snapshot (`dist/treerig.rb`) both render it with that script.
- **The verification steps in `docs/install.md`** pin the identity above.
- **The teardown pin** (`treerig teardown pin`, `docs/design/teardown.md`; #85)
  downloads `checksums.txt`, `checksums.txt.sigstore.json` and the platform
  archive by the names above, verifies `checksums.txt` with `cosign verify-blob`
  against the exact identity above (with the tag's version in it) and issuer,
  and checks the archive against its single line in `checksums.txt`. It takes
  `treerig` from the archive root. The asset names, the archive layout, the
  workflow file name and the bundle format (cosign 3 or newer reads it) are
  therefore what it depends on.

Only a release that is not a pre-release is ever "latest": `install.sh` and
Homebrew skip a tag with a suffix such as `-rc.1`.

## Who may release

A tag is the trigger, so who can push one is the access control. The `v*` tag
ruleset on the repository and the workflow's check that the tagged commit is on
`main` are what control the signing identity: a tag on a commit whose
`release.yml` dropped its `environment:` line would still sign. The `release`
environment (required reviewers, a deployment rule for `v*`) protects the tap
token and adds a human approval to each gated job. CONTRIBUTING.md lists the
setup.
