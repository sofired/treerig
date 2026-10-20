# Install

How to install treerig and its `trig` alias, check a download, set up shell
completion, and rename the binary.

> **Only a pre-release exists so far**, `v0.1.0-rc.2`. Install it with
> `install.sh --version` or from its downloadable files (see
> [Pre-releases](#pre-releases)), or build from source (see
> [From source](#from-source)). Homebrew and the install script's default, the
> latest release, need a stable release, which is never a pre-release.

treerig is a single static binary for Linux and macOS, on amd64 and arm64
(Windows is not supported). It needs Git, Docker and Docker Compose at run
time; `treerig doctor` checks all three once it is on PATH. Once a stable
release exists, Homebrew and the install script install the latest release,
which is never a pre-release (see [Pre-releases](#pre-releases)); the packages
and archives are per release. Coming from a build before the
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

Neither makes the [`trig` alias](#the-trig-alias). To add it, link it beside
`treerig`, wherever that is on your PATH:

```bash
ln -s treerig "$(dirname "$(command -v treerig)")/trig"
```

A plain build prints `0.0.0-dev` for `treerig --version`, not a release's
version. To stamp one, build with
`-ldflags "-X github.com/sofired/treerig/internal/brand.Version=<version>"`.

## Homebrew

*Available from the first stable release, which has not shipped yet.*

```bash
brew install sofired/tap/treerig
```

Homebrew installs a prebuilt bottle where the tap has one for your platform;
otherwise it builds treerig from the release's source, which needs Go (Homebrew
installs that for the build). It links `trig`, the [short alias](#the-trig-alias),
beside `treerig`, and installs shell completions for both names.

## Install script

*Without `--version`, available from the first stable release, which has not
shipped yet. With `--version`, available now.*

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

It also links `trig`, the [short alias](#the-trig-alias), beside `treerig`. It
never replaces anything, and `treerig` is installed, with exit status 0,
whatever happens to the link:

- **Already linked.** A `trig` in the install directory that points at
  `treerig` is left alone ("already in place"), and so is a `trig` elsewhere on
  PATH that is a link to a `treerig` ("already provided").
- **Something else in the way.** A file, a symlink that points elsewhere or is
  broken, or a different `trig` on PATH: the link is skipped, with the reason
  on stderr.
- **The link can't be made.** It prints "could not create" on stderr.

It reminds you to load the `trig` completion only when it created the link. To
skip the alias, set `TREERIG_NO_ALIAS` to any non-empty value except `0`. With
`curl | sh` the variable goes on the `sh` side of the pipe:

```bash
curl -fsSL https://raw.githubusercontent.com/sofired/treerig/main/install.sh | TREERIG_NO_ALIAS=1 sh
```

## Linux packages

Each release attaches a `.deb`, an `.rpm` and an `.apk` for amd64 and arm64.
They put `treerig` in `/usr/bin`, `trig` (the [short alias](#the-trig-alias))
beside it, and the bash, zsh and fish completions for both names where those
shells look for them:

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
package manager. The packages always include `/usr/bin/trig`: if another
package owns that file, the install fails (see [The `trig` alias](#the-trig-alias)).

## Release archive

Each release also has a `treerig_<version>_<os>_<arch>.tar.gz` for `linux` and
`darwin`, with `amd64` and `arm64`. It holds `treerig` at its root, with
`LICENSE`, `NOTICE`, `README.md` and the shell completions under
`completions/`; extract it and put `treerig` anywhere on PATH. The archive has
no `trig` link, since archives hold regular files; to add the
[alias](#the-trig-alias), run `ln -s treerig trig` in the directory that holds
`treerig`. The completions folder already has the `trig` scripts
(`trig.bash`, `_trig`, `trig.fish`).

## Pre-releases

*The first pre-release is `v0.1.0-rc.2`. Homebrew needs a stable release.*

A tag with a suffix, such as `v1.2.3-rc.1`, is published as a GitHub
pre-release. It is never "latest", so the commands above do not install it,
and Homebrew does not see it. Install one with `install.sh --version
1.2.3-rc.1`, or download its files from the
[Releases page](https://github.com/sofired/treerig/releases).

## Verifying a download

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
prints a script for the name it was invoked as, so `trig completion
bash|zsh|fish` is the same command under the other name. A completion loaded
for `treerig` does not cover `trig`: load both if you use both. Homebrew and the
packages install completions for both names; `install.sh` reminds you to load
the `trig` one when it created the link.

## The `trig` alias

*Homebrew and the install script without `--version` provide it from the first
stable release. Every other route provides it now.*

Homebrew, `install.sh` and the Linux packages give you `trig` automatically.
With a release archive or a source build, make the link yourself.

`trig` is a short name for the same program: a symlink named `trig` that points
at `treerig`. Run through it, the help text, usage lines and the `[trig]`
message prefix say `trig`.

`trig` is only a name. The config file (`treerig.toml`), the `TREERIG_*`
variables, the labels and the directories keep their `treerig` names. Some
hints in output still name `treerig`; either name works, because both run the
same program.

### When something else is already called `trig`

| Route | What happens |
| --- | --- |
| Homebrew | A different `trig` in Homebrew's `bin` makes the install fail at the link step. Remove it, then run `brew link --overwrite treerig`. |
| `install.sh` | It skips the alias, says why on stderr, and installs `treerig` anyway. See [Install script](#install-script). |
| `.deb`, `.rpm`, `.apk` | The packages always include `/usr/bin/trig`. If another package owns that file, the install fails; a file nobody owns is replaced. |
| Release archive, source build | You make the link, so you choose: `ln -s treerig trig` where `treerig` is, or skip it. |

### Removing it

Homebrew and the packages remove `trig` along with `treerig`. If you installed
by hand (`install.sh`, an archive or a source build), remove the `trig` link
when you remove `treerig`.

## Calling it something else

Name the binary or symlink whatever you like. Help text, the log prefix, and
most "run `X urls`" hints follow the name it was invoked as, which is read
from `argv[0]` and from nowhere else. A few hints, such as the one shown when
no `treerig.toml` is found, always name `treerig`; either name works, because
both run the same program:

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
