# Release contract

What a berth release publishes, and which of it other things depend on. The
pipeline is `.goreleaser.yaml` (what is built and named) and
`.github/workflows/release.yml` (who builds it and signs it); CONTRIBUTING.md
says how to cut one. Everything below is a contract with people and programs
that cannot be changed with the next release: **renaming `release.yml`,
changing a file-name template, or moving `berth` inside the archive is a
migration, not a refactor.**

## The assets of release `vX.Y.Z`

The tag is the version with a leading `v`; file names carry the version without
it. All of them are attached to the GitHub Release.

| Asset | Name |
| --- | --- |
| Platform archive | `berth_X.Y.Z_<os>_<arch>.tar.gz`, `<os>` `linux` or `darwin`, `<arch>` `amd64` or `arm64` |
| Source archive | `berth-X.Y.Z.tar.gz`, everything under `berth-X.Y.Z/` |
| Debian package | `berth_X.Y.Z_<arch>.deb`, `<arch>` `amd64` or `arm64` |
| RPM package | `berth-X.Y.Z-1.<arch>.rpm`, `<arch>` `x86_64` or `aarch64` |
| Alpine package | `berth_X.Y.Z_<arch>.apk`, `<arch>` `x86_64` or `aarch64` |
| Checksums | `checksums.txt`: `<sha256>  <file name>` for every asset above |
| Signature | `checksums.txt.sigstore.json`: a Sigstore bundle for `checksums.txt` |

A platform archive holds exactly `berth` (at the archive root, mode 0755),
`LICENSE`, `NOTICE`, `README.md` and `completions/` (`berth.bash`, `_berth`,
`berth.fish`). The packages install `berth` to `/usr/bin` and the completions
where bash, zsh and fish look for them. `packaging/verify-snapshot.sh` asserts
all of this on every pull request.

Two builds of one commit are byte-identical: files carry the commit's date and
root ownership, and the pull-request check rebuilds and diffs `checksums.txt`.

## Who signed it

`checksums.txt` is signed keyless with cosign. The certificate's identity is
the release workflow at the tag, and its issuer is GitHub's OIDC:

```
identity: https://github.com/sofired/berth/.github/workflows/release.yml@refs/tags/vX.Y.Z
issuer:   https://token.actions.githubusercontent.com
```

Every file in `checksums.txt` also has a GitHub build provenance attestation
from the same workflow (`gh attestation verify <file> --repo sofired/berth`).
The workflow's file name is part of the identity a verifier pins, so a rename
breaks every verification instruction already published. Nothing the pipeline
emits is signed by a package-manager key.

## What depends on it

- **`install.sh`** builds the archive name from the platform and version,
  reads `checksums.txt` to find the latest version (from the archive names in
  it) and the archive's digest, and extracts `berth` from the archive root.
  It does not check the signature. The script is served from `main`
  (`raw.githubusercontent.com`) or from a tag, so a change to it ships on merge,
  not with a release.
- **The Homebrew formula** in `sofired/homebrew-tap` builds from the source
  archive; the release workflow takes its digest from `checksums.txt`. The
  template is `packaging/homebrew/berth.rb`.
- **The README's verification steps** pin the identity above.
- **#85** (running `worktree remove` from a pinned, trusted binary) is built on
  this: what it pins to is the digest list and identity in this note.

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
