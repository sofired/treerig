# Install

How to install treerig, check a download, set up shell completion, and rename the binary.

> **No release exists yet.** Until a tag exists, build treerig from source (see
> [From source](#from-source)). From the first pre-release, `v0.1.0-rc.1`,
> `install.sh --version` and the release's downloadable files work (see
> [Pre-releases](#pre-releases)). Homebrew and the install script's default,
> the latest release, need a stable release, which is never a pre-release.

treerig is a single static binary for Linux and macOS, on amd64 and arm64
(Windows is not supported). It needs Git, Docker and Docker Compose at run
time; `treerig doctor` checks all three once it is on PATH. Once a release
exists, Homebrew and the install script install the latest release, which is
never a pre-release (see [Pre-releases](#pre-releases)); the packages and
archives are per release. Coming from a build before the
rename? See [Switching from a build before the rename](upgrading-from-a-pre-rename-build.md)
first.

## From source

Build it with Go, at least the version [`go.mod`](../go.mod) names:

```bash
git clone https://github.com/sofired/treerig && cd treerig
go install ./cmd/treerig   # installs $(go env GOPATH)/bin/treerig (or $GOBIN)
```

or build a binary yourself and put it wherever you like:

```bash
mkdir -p ~/.local/bin && go build -o ~/.local/bin/treerig ./cmd/treerig   # any directory on PATH
```

A plain build prints `0.0.0-dev` for `treerig --version`, not a release's
version. To stamp one, build with
`-ldflags "-X github.com/sofired/treerig/internal/brand.Version=<version>"`.

## Homebrew

*Available from the first stable release. None has shipped yet.*

```bash
brew install sofired/tap/treerig
```

Homebrew installs a prebuilt bottle where the tap has one for your platform;
otherwise it builds treerig from the release's source, which needs Go (Homebrew
installs that for the build). Shell completions install with it.

## Install script

*Without `--version`, available from the first stable release. With
`--version`, from the first pre-release. None has shipped yet.*

```bash
curl -fsSL https://raw.githubusercontent.com/sofired/treerig/main/install.sh | sh
```

[`install.sh`](../install.sh) picks the release archive for your platform,
checks its sha256 against the release's `checksums.txt` before extracting
anything, and installs `treerig` into `~/.local/bin`. It never uses `sudo`. It
checks the checksum, not the signature on `checksums.txt`: that is a separate
step (see [Verifying a download](#verifying-a-download)). Pass options after
`sh -s --`, and pin the script itself to a tag if you want the exact script a
release shipped with:

```bash
curl -fsSL https://raw.githubusercontent.com/sofired/treerig/v1.2.3/install.sh \
  | sh -s -- --version 1.2.3 --dir /usr/local/bin
```

`--dir` (or `TREERIG_INSTALL_DIR`) is the directory to install into and
`--version` (or `TREERIG_VERSION`) pins a release; without it the latest release
is installed. It uses `curl`, or `wget` when there is no `curl`, and
`sha256sum`, or `shasum -a 256` when there is no `sha256sum`.
If the directory is not on your PATH, it prints the line to add.

## Linux packages

*Available from the first release or pre-release. None has shipped yet.*

Each release attaches a `.deb`, an `.rpm` and an `.apk` for amd64 and arm64.
They put `treerig` in `/usr/bin` and the bash, zsh and fish completions where
those shells look for them:

```bash
VERSION=1.2.3    # a release on the Releases page
curl -fsSLO "https://github.com/sofired/treerig/releases/download/v${VERSION}/treerig_${VERSION}_amd64.deb"
sudo apt install "./treerig_${VERSION}_amd64.deb"                    # Debian, Ubuntu

curl -fsSLO "https://github.com/sofired/treerig/releases/download/v${VERSION}/treerig-${VERSION}-1.x86_64.rpm"
sudo dnf install "./treerig-${VERSION}-1.x86_64.rpm"                 # Fedora, RHEL

curl -fsSLO "https://github.com/sofired/treerig/releases/download/v${VERSION}/treerig_${VERSION}_x86_64.apk"
sudo apk add --allow-untrusted "./treerig_${VERSION}_x86_64.apk"     # Alpine
```

On ARM the architecture is `arm64` in the `.deb` name and `aarch64` in the
`.rpm` and `.apk` names. The packages are not signed with a package-manager
key, so apk needs `--allow-untrusted`; check the download as described under
[Verifying a download](#verifying-a-download) instead. Uninstall them with your
package manager.

## Release archive

*Available from the first release or pre-release. None has shipped yet.*

Each release also has a `treerig_<version>_<os>_<arch>.tar.gz` for `linux` and
`darwin`, with `amd64` and `arm64`. It holds `treerig` at its root, with
`LICENSE`, `NOTICE`, `README.md` and the shell completions under
`completions/`; extract it and put `treerig` anywhere on PATH.

## Pre-releases

*Available from the first pre-release (`v0.1.0-rc.1`). None has shipped yet.
Homebrew needs a stable release.*

A tag with a suffix, such as `v1.2.3-rc.1`, is published as a GitHub
pre-release. It is never "latest", so the commands above do not install it,
and Homebrew does not see it. Install one with `install.sh --version
1.2.3-rc.1`, or download its files from the
[Releases page](https://github.com/sofired/treerig/releases).

## Verifying a download

*Available from the first release or pre-release. None has shipped yet.*

Every release publishes `checksums.txt`, covering the archives, the source
archive and the packages. Compare the file you downloaded with it (run in the
directory holding both; this works with `sha256sum` or, on macOS,
`shasum -a 256`):

```bash
VERSION=1.2.3
FILE="treerig_${VERSION}_linux_amd64.tar.gz"
grep " ${FILE}\$" checksums.txt | sha256sum --check     # macOS: shasum -a 256 --check
```

Optionally, check that `checksums.txt` came from this repository's release
workflow. It is signed with [cosign](https://github.com/sigstore/cosign)
(keyless, with the workflow's identity), and each file has a GitHub build
provenance attestation. Download `checksums.txt.sigstore.json` beside it:

```bash
VERSION=1.2.3
FILE="treerig_${VERSION}_linux_amd64.tar.gz"
cosign verify-blob \
  --certificate-identity "https://github.com/sofired/treerig/.github/workflows/release.yml@refs/tags/v${VERSION}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --bundle checksums.txt.sigstore.json checksums.txt
gh attestation verify "$FILE" --repo sofired/treerig
```

[`design/release.md`](design/release.md) lists every asset name and
the identity.

## Shell completion

`treerig completion <shell>` prints a completion script for bash, zsh, fish or
PowerShell. It completes commands, their flags with descriptions, and
`--config` with `.toml` files. It also completes registered values,
read-only: no lock is taken, Docker is never touched, and nothing is
written:

- `worktree remove` and `worktree unlock` complete a registered worktree's
  slug, described with its domain and its last-known path; `worktree
  unlock` also offers a worktree it can still recover a stale lock for even
  though it was never registered (or is no longer), named by its UUID.
- `worktree rename` completes nothing: the new slug is yours to choose.
- `--domain` on `worktree remove` and `worktree unlock` completes a
  registered domain, and narrows the slugs offered to that domain once one
  is given — hiding any recoverable, unregistered worktree `worktree
  unlock` would otherwise also offer, since one carries no domain to match.
- `up` completes the service names declared in this worktree's `treerig.toml`,
  skipping any already named on the command line. Each is described by its
  `[service.<name>.index]` label, else its subdomain or path, else "host root".
- `backup` and `restore` complete directories, for their bundle directory.
- `teardown pin --file` completes file names, for the binary to pin. `--release`
  completes nothing.
- `run` completes the command names under `[commands]`, in the order the file
  lists them, each described by its command line. Only the name is completed,
  never what follows it.

A registry that is missing, empty or unreadable, or a `treerig.toml` that
cannot be found or parsed, completes nothing rather than failing.
`treerig completion <shell> --help` says how to load the script for good; to
try it in the current shell:

```bash
source <(treerig completion bash)   # or zsh
treerig completion fish | source    # fish
```

A renamed binary or symlink (see [Calling it something else](#calling-it-something-else))
prints a script for the name it was invoked as.

## Calling it something else

Name the binary or symlink whatever you like. Help text, the log prefix, and
every "run `X urls`" hint follow the name it was invoked as, which is read
from `argv[0]` and from nowhere else:

```bash
ln -s "$(go env GOPATH)/bin/treerig" ~/.local/bin/duke
duke --help               # Usage:
                          #   duke [flags]
                          #   duke [command]
```

A binary built or invoked with no meaningful name to read — `main`, a bare
`.`, or an empty `argv[0]` — says `treerig` instead.

What does **not** follow the name is anything written to disk: the config
and settings filenames (`treerig.toml`, `settings.toml`), the treerig root and
control-home directories, the environment variables (`TREERIG_HOME`,
`TREERIG_CONTROL_HOME`, …), the variables handed to hooks (`TREERIG_STAGE`, …),
and the Docker label namespace. Those are storage contracts. If they followed
the binary name, renaming a symlink would orphan state.
