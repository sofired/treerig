# Install

How to install treerig and its `trig` alias, check a download, set up shell
completion, and rename the binary.

> **Quickest routes.** `brew install sofired/tap/treerig`, or the
> [install script](#install-script), which installs the latest release. Packages,
> archives, a build from source and pre-releases are below.

treerig is a single static binary for Linux and macOS, on amd64 and arm64
(Windows is not supported). It needs Git, Docker and Docker Compose at run
time; `treerig doctor` checks all three once it is on PATH. Homebrew and the
install script install the latest release, which is never a pre-release (see
[Pre-releases](#pre-releases)); the packages and archives are per release.
Coming from a build before the rename? See [Switching from a build before the rename](upgrading-from-a-pre-rename-build.md)
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
Stamp a local build `0.0.0-dev+<label>`, not a release-looking version such as
`1.2.3`: `worktree remove` treats a build whose version looks like a release's as a
release, which removes with itself and needs no pinned teardown binary
([Teardown pin](teardown-pin.md)). The same goes for a version set by `-ldflags`
or `GOFLAGS` from an environment you have not reviewed (direnv, mise, a
devcontainer): do not `go build` a branch in a shell whose auto-loaded
environment you have not read, and install from a release instead.

## Homebrew

```bash
brew install sofired/tap/treerig
```

Homebrew installs a prebuilt bottle where the tap has one for your platform;
otherwise it builds treerig from the release's source, which needs Go (Homebrew
installs that for the build). It links `trig`, the [short alias](#the-trig-alias),
beside `treerig`, and installs shell completions for both names.

## Install script

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

To skip the alias, set `TREERIG_NO_ALIAS` to any non-empty value except `0`.
With `curl | sh` the variable goes on the `sh` side of the pipe:

```bash
curl -fsSL https://raw.githubusercontent.com/sofired/treerig/main/install.sh | TREERIG_NO_ALIAS=1 sh
```

### What the script sets up for completion

Completion setup is on by default, because a one-line install should give you
working completion. To skip it, pass `--no-completions` or set
`TREERIG_NO_COMPLETIONS` to any non-empty value except `0` (the option wins).
Use that for CI and scratch installs:

```bash
curl -fsSL https://raw.githubusercontent.com/sofired/treerig/main/install.sh | sh -s -- --no-completions
```

After the binary and the alias are in place, `install.sh` sets up
[shell completion](#shell-completion) from the scripts inside the archive. It
copies them; it does not run `treerig completion`. The only thing it runs from
the release is the verified binary's `--version`. The only other programs it may
run are read-only lookups: `brew --prefix`, when `brew` is on PATH, to find
Homebrew's completion directory, and `zsh -f`, when `zsh` is, to read zsh's
default `fpath`. It covers `treerig`, and `trig`
when this run created the alias, found it already in place, or found it
provided elsewhere on PATH by a link to `treerig`. It leaves `trig` out when a
different `trig` is in the way, and under `TREERIG_NO_ALIAS`.

Completions are per user, so `--dir` does not move them: they go under your home
directory whatever directory the binary goes to.

For each of fish, bash and zsh that is on PATH, it writes (creating the
directories, and replacing any regular file of that name on a re-run; a symlink
or directory at one of these paths is left alone):

| Shell | Files | Loaded because |
| --- | --- | --- |
| fish | `${XDG_CONFIG_HOME:-~/.config}/fish/completions/treerig.fish` (and `trig.fish`) | fish loads that directory on its own. |
| bash | `${XDG_DATA_HOME:-~/.local/share}/bash-completion/completions/treerig` (and `trig`) | bash-completion 2 loads a file named after the command. |
| zsh | `${XDG_DATA_HOME:-~/.local/share}/zsh/site-functions/_treerig` (and `_trig`) | The block below puts the directory on `fpath`. |

A shell you do not log in with still gets its files. fish and bash load theirs
on their own, and the report says they are in place for when you use them. zsh
does not: nothing puts the directory on its `fpath` unless zsh is your login
shell (no `.zshrc` is written otherwise), so the report tells you to add the
fpath lines from [Shell completion](#shell-completion). bash needs
bash-completion 2 (macOS's own bash 3.2 cannot use it: install `bash-completion@2` and a newer bash with Homebrew), and
if bash is your login shell and bash-completion does not look installed, the
script says so. The script creates directories as 755 whatever your umask, since
zsh's `compinit` distrusts a group-writable directory on `fpath`.

If a system-wide copy of a command's completion is already there for a shell,
from a package (`/usr/share/...`) or from Homebrew (under `brew --prefix`; those
two are the only places it looks), the
script leaves that shell's user-level copy out and says so, because a copy in
your home directory would shadow the system one, which may be newer. For zsh
the system copy counts only if its directory is on zsh's default `fpath`:
Homebrew's on Apple silicon (`/opt/homebrew/share/zsh/site-functions`) is not on
macOS's own zsh's, and `/usr/share/zsh/site-functions` is not on Debian's or
Ubuntu's, so zsh would never load those copies and the script writes its own. The zsh
block below is still added: `compinit` may be missing even when the scripts are
there. The reverse does not hold. A user-level copy that an earlier `install.sh`
wrote shadows the packages' and Homebrew's, so before you switch to either,
[undo the setup](#undoing-the-completion-setup) first.

When your login shell (`$SHELL`) is zsh, it also appends one block to
`${ZDOTDIR:-~}/.zshrc`, creating the file if it is missing, in a single write:

```zsh
# >>> treerig completion (added by install.sh; delete this block to undo) >>>
fpath=('/home/you/.local/share/zsh/site-functions' $fpath)
if (( $+functions[compdef] )); then
  autoload -Uz _treerig && compdef _treerig treerig
  autoload -Uz _trig && compdef _trig trig
else
  autoload -Uz compinit && compinit
fi
# <<< treerig completion <<<
```

The block works whether or not your `.zshrc` ran `compinit` before it
(oh-my-zsh, prezto and the like do). If your `.zshrc` runs `compinit` later,
that is harmless, and it picks the functions up too. The `compinit` in the
block, which runs only for a zsh setup that had none, adds a little shell
start-up time; a plain `compinit` also asks about "insecure directories" if one
on `fpath` is group-writable, and `compaudit | xargs chmod g-w` fixes that.

The script never adds the block twice, and never rewrites one that is there: if
the start marker is already in the file it leaves the file alone. A begin marker
with no end marker counts as damage: it reports that, edits nothing, and asks you
to remove the block and re-run. It does not edit a `.zshrc` that is a symlink or
not writable either; it prints the same lines for you to add. A `.zshrc` that you
track in a dotfiles repository as a regular file does get the block, so dotfiles
users may prefer `--no-completions` and the manual steps under
[Shell completion](#shell-completion). A re-run after the alias first appears
does not edit the block; it prints the one line to add.

`HOME` and `ZDOTDIR` must be absolute paths or the script skips completion with a
message. It reads `ZDOTDIR` from its own environment, so one you set only in
`~/.zshenv` is not seen: the block goes into `~/.zshrc`, and you move it.

The report names what was written where, and ends with "Open a new terminal to
use completion." (or, when it could not edit your `.zshrc` and printed lines for
you to add, "After adding those lines, or fixing the file, open a new terminal.")
and a link to the undo steps. Nothing here can fail the install:
a problem is one line on stderr, and `treerig` is installed anyway. It does
nothing when run as root, since `$HOME` may be another user's; it prints one line
saying so.

### Undoing the completion setup

Delete the files `install.sh` wrote (leave out any you did not get, such as
`trig` or a shell you do not have):

```bash
rm -f "${XDG_CONFIG_HOME:-$HOME/.config}"/fish/completions/{treerig,trig}.fish
rm -f "${XDG_DATA_HOME:-$HOME/.local/share}"/bash-completion/completions/{treerig,trig}
rm -f "${XDG_DATA_HOME:-$HOME/.local/share}"/zsh/site-functions/_{treerig,trig}
```

Then delete the block between the two marker lines in `${ZDOTDIR:-$HOME}/.zshrc`
(from `# >>> treerig completion` to `# <<< treerig completion <<<`).

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

These names are for a final release. A pre-release such as `1.2.3-rc.1` spells
its packages differently. In `checksums.txt` and in the package's own version,
the `.deb` and `.rpm` have `1.2.3~rc.1` and the `.apk` has `1.2.3_rc.1`. GitHub
rewrites the `~` to `.` in uploaded file names, so you download, and save,
`treerig_1.2.3.rc.1_amd64.deb` and `treerig-1.2.3.rc.1-1.x86_64.rpm` from
`…/download/v1.2.3-rc.1/`. To check one with the recipe under
[Verifying a download](#verifying-a-download), look it up by its `~` name:
`grep " treerig_1.2.3~rc.1_amd64.deb\$" checksums.txt` gives the hash to compare
with `sha256sum treerig_1.2.3.rc.1_amd64.deb`. [`docs/design/release.md`](design/release.md)
lists the exact names.

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

*Before `v0.1.0`, the first stable release, there were `v0.1.0-rc.2` and `v0.1.0-rc.3`. Homebrew sees only stable releases.*

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

`install.sh` sets completion up for you (see
[What the script sets up for completion](#what-the-script-sets-up-for-completion)),
and Homebrew and the Linux packages install the scripts for both names in the
shells' system-wide directories. Those are enough for fish, and for bash with
bash-completion 2. zsh still needs `compinit` to have run: a package or Homebrew
only puts the scripts on `fpath`, and a `.zshrc` that never runs `compinit`
loads none of them. With a release archive or a source build you load the script
yourself.

`treerig completion <shell> --help` prints the generic text cobra generates for
every program; the steps below are the recommended ones. To try the script in the
current shell:

```bash
source <(treerig completion bash)   # bash
treerig completion fish | source    # fish only: `| source` does not work in bash or zsh
```

zsh needs `compinit` before the script is sourced, or `source <(treerig
completion zsh)` fails with `command not found: compdef`:

```zsh
autoload -U compinit && compinit    # once, if your .zshrc does not already
source <(treerig completion zsh)
```

To make zsh permanent, write the script into a directory on `fpath` (before
`compinit` runs, or run `compinit` after adding it), and add the directory to
`.zshrc`:

```zsh
mkdir -p ~/.local/share/zsh/site-functions
treerig completion zsh > ~/.local/share/zsh/site-functions/_treerig
# in ~/.zshrc, before compinit (or before a plain `autoload -U compinit && compinit`):
fpath=(~/.local/share/zsh/site-functions $fpath)
autoload -U compinit && compinit
```

For bash, write it where bash-completion 2 looks, named after the command:
`treerig completion bash > ~/.local/share/bash-completion/completions/treerig`.
For fish: `treerig completion fish > ~/.config/fish/completions/treerig.fish`.

A renamed binary or symlink (see [Calling it something else](#calling-it-something-else))
prints a script for the name it was invoked as, so `trig completion
bash|zsh|fish` is the same command under the other name. A completion loaded
for `treerig` does not cover `trig`: load both if you use both. Homebrew, the
packages and `install.sh` provide both names (`install.sh` only when the alias is
yours).

## The `trig` alias

Homebrew, `install.sh` and the Linux packages give you `trig` automatically.
With a release archive or a source build, make the link yourself.

`trig` is a short name for the same program: a symlink named `trig` that points
at `treerig`. Run through it, the help text, usage lines and the `[trig]`
prefix on errors and warnings say `trig`.

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

Name the binary or symlink whatever you like. Help text, the `[name]` prefix on
errors and warnings, and most "run `X urls`" hints follow the name it was invoked as, which is read
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
