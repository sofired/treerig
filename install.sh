#!/bin/sh
# treerig installer — https://github.com/sofired/treerig
#
# Install the latest release into ~/.local/bin:
#
#   curl -fsSL https://raw.githubusercontent.com/sofired/treerig/main/install.sh | sh
#
# Pin a version or choose the directory (arguments after `sh -s --`):
#
#   curl -fsSL https://raw.githubusercontent.com/sofired/treerig/main/install.sh \
#     | sh -s -- --version 1.2.3 --dir /usr/local/bin
#
# What it does, in order:
#   1. Works out which release archive matches this machine.
#   2. Finds the latest release (or the version you asked for). "Latest" is
#      GitHub's latest release, which is never a pre-release: install one of
#      those with --version.
#   3. Downloads the archive and checksums.txt, and VERIFIES the archive's
#      sha256 against checksums.txt before it extracts anything. That catches
#      a damaged or swapped archive; it does not check the signature on
#      checksums.txt (docs/install.md says how to do that).
#   4. Installs the binary into the directory you chose. It never uses sudo:
#      if the directory is not writable, it says so and stops before it
#      downloads anything.
#   5. Adds `trig`, a short alias, as a symlink to treerig beside it. It never
#      replaces anything: if a different `trig` is on your PATH, or a `trig`
#      is already in the directory and is not a symlink to treerig, it skips
#      the alias, says why on stderr, and still installs treerig. Set
#      TREERIG_NO_ALIAS=1 to skip it without looking.
#   6. Sets up shell completion from the scripts in the archive, without
#      running the new binary: it copies them for each of bash, zsh and fish
#      that is on your PATH (into the XDG data and config directories, which
#      bash-completion, zsh's fpath and fish read), for treerig and, when the
#      alias is yours, for trig. When your login shell is zsh it also adds one
#      marked block to ${ZDOTDIR:-$HOME}/.zshrc, once, unless that file is a
#      symlink or not writable (then it prints the lines instead). It leaves a
#      shell's user-level copy out when a system-wide one (a package, Homebrew)
#      is already there, never rewrites an existing block, never fails the
#      install, and does nothing as root. Pass --no-completions or set
#      TREERIG_NO_COMPLETIONS=1 to skip it.
#
# Only the verified treerig binary is ever run, and only to print its version.
# The one other program it may run is `brew --prefix`, read-only, when brew is
# on your PATH, to find Homebrew's completion directory.
# Nothing is written outside a private temporary directory until the checksum
# matches.
#
# The file names and the archive layout it relies on are listed in
# docs/design/release.md.
#
# Options:
#   -d, --dir DIR        Install into DIR (used as given).
#   -v, --version VER    Install VER (1.2.3 or v1.2.3) instead of the latest.
#       --no-completions Do not set up shell completion.
#   -h, --help           Show this help.
#
# Environment (an option wins over its variable):
#   TREERIG_INSTALL_DIR    Install directory. Default: $HOME/.local/bin.
#   TREERIG_VERSION        Version to install. Default: the latest release.
#   TREERIG_BASE_URL       Release-download base URL, laid out as
#                        <base>/v<version>/<file>. For mirrors and testing.
#   TREERIG_LATEST_URL     A checksums.txt that stands for the latest release
#                        (default: GitHub's releases/latest/download/
#                        checksums.txt redirect). For testing.
#   TREERIG_NO_ALIAS       Any non-empty value except 0: do not create the `trig`
#                        alias. Set it on the shell side of a pipe:
#                        curl -fsSL <url> | TREERIG_NO_ALIAS=1 sh
#   TREERIG_NO_COMPLETIONS Any non-empty value except 0: do not set up shell
#                        completion (--no-completions wins over this).
#
# POSIX sh only, since it is piped into `sh`; passes `shellcheck -s sh`.

set -eu

REPO="sofired/treerig"
BIN="treerig"
ALIAS="trig"
BASE_URL="${TREERIG_BASE_URL:-https://github.com/${REPO}/releases/download}"
LATEST_URL="${TREERIG_LATEST_URL:-https://github.com/${REPO}/releases/latest/download/checksums.txt}"
RELEASES_PAGE="https://github.com/${REPO}/releases"

info() { printf '%s\n' "$*"; }
err() { printf '%s\n' "treerig install: $*" >&2; }
die() {
  err "$*"
  exit 1
}

usage() {
  cat <<'USAGE'
Usage: install.sh [--dir DIR] [--version VER] [--no-completions]

Installs the treerig release for this machine, after checking its sha256 against
the release's checksums.txt.

  -d, --dir DIR        Install into DIR (default: $TREERIG_INSTALL_DIR, else
                       $HOME/.local/bin).
  -v, --version VER    Install VER (1.2.3 or v1.2.3) instead of the latest
                       (default: $TREERIG_VERSION).
      --no-completions Do not set up shell completion (default: set it up,
                       unless $TREERIG_NO_COMPLETIONS is non-empty and not 0).
  -h, --help           Show this help.

Also links `trig`, a short alias, to treerig in the same directory, unless a
different `trig` is on your PATH or already in that directory (it never
replaces one). Set TREERIG_NO_ALIAS=1 to skip it.

Also copies the bash, zsh and fish completion scripts from the archive into your
XDG data and config directories, for each of those shells on your PATH, and, when
your login shell is zsh, adds one marked block to ${ZDOTDIR:-$HOME}/.zshrc. It
does nothing as root and never fails the install.
USAGE
}

# --- Arguments ---------------------------------------------------------------

want_dir="${TREERIG_INSTALL_DIR:-}"
want_version="${TREERIG_VERSION:-}"
no_completions=0
case "${TREERIG_NO_COMPLETIONS:-}" in
  "" | 0) : ;;
  *) no_completions=1 ;;
esac
while [ $# -gt 0 ]; do
  case "$1" in
    -d | --dir)
      [ $# -ge 2 ] || die "$1 needs a directory"
      want_dir="$2"
      shift 2
      ;;
    --dir=*)
      want_dir="${1#--dir=}"
      shift
      ;;
    -v | --version)
      [ $# -ge 2 ] || die "$1 needs a version"
      want_version="$2"
      shift 2
      ;;
    --version=*)
      want_version="${1#--version=}"
      shift
      ;;
    --no-completions)
      no_completions=1
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1 (try --help)"
      ;;
  esac
done

# --- Tooling: curl or wget to download, sha256sum or shasum to verify --------

if command -v curl >/dev/null 2>&1; then
  DOWNLOADER=curl
elif command -v wget >/dev/null 2>&1; then
  DOWNLOADER=wget
else
  die "need curl or wget to download files, found neither"
fi

# Linux usually has sha256sum, macOS usually only shasum.
if command -v sha256sum >/dev/null 2>&1; then
  SHA_TOOL=sha256sum
elif command -v shasum >/dev/null 2>&1; then
  SHA_TOOL=shasum
else
  die "need sha256sum or shasum to verify the download, found neither"
fi

# download <url> <dest-file>: fails on any HTTP or network error. The tools'
# own messages are silenced (curl -s without -S, wget -q) so that the caller's
# message is the only one printed.
download() {
  if [ "$DOWNLOADER" = curl ]; then
    case "$1" in
      https://*)
        # Hold the transport to HTTPS, redirects included: GitHub sends a
        # release asset to a CDN, and that hop must not drop to plaintext.
        curl -fsL --proto '=https' --proto-redir '=https' --tlsv1.2 "$1" -o "$2"
        ;;
      *)
        # Only a TREERIG_BASE_URL or TREERIG_LATEST_URL override (a mirror, or a test
        # server) gets here.
        curl -fsL "$1" -o "$2"
        ;;
    esac
  else
    # busybox wget has no --https-only; the default URLs are https already.
    wget -q -O "$2" "$1"
  fi
}

# sha256_of <file>: print the file's hex digest.
sha256_of() {
  if [ "$SHA_TOOL" = sha256sum ]; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# --- Platform ----------------------------------------------------------------

# detect_platform: print "<os>_<arch>" as the archive names spell it, or stop
# with a message for a platform treerig is not built for.
detect_platform() {
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) die "unsupported operating system: $(uname -s) (treerig runs on Linux and macOS)" ;;
  esac
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "unsupported architecture: $(uname -m) (treerig is built for amd64 and arm64)" ;;
  esac
  printf '%s_%s\n' "$os" "$arch"
}

# --- Version -----------------------------------------------------------------

# validate_version <version>: the version becomes part of a URL and a file
# name, and may come from JSON on a host the caller can override. Allow only
# what a semantic version uses, starting with a digit, and no "..".
validate_version() {
  case "$1" in
    "") die "empty version string" ;;
    *[!0-9A-Za-z.+-]*) die "unexpected version string: $1" ;;
    *..*) die "unexpected version string: $1" ;;
    [0-9]*) : ;;
    *) die "unexpected version string: $1" ;;
  esac
}

# resolve_version <platform> <scratch-file>: print the version to install,
# without a leading v. The latest release is read from the file names in its
# checksums.txt, fetched through GitHub's releases/latest redirect: that is an
# ordinary download, not an API call, so it has no rate limit to run into.
resolve_version() {
  if [ -n "$want_version" ]; then
    v="${want_version#v}"
    validate_version "$v"
    printf '%s\n' "$v"
    return
  fi
  download "$LATEST_URL" "$2" ||
    die "could not find the latest release ($LATEST_URL). There may be no release yet, only a pre-release (install one with --version), or no network. Releases: $RELEASES_PAGE"
  v="$(sed -n "s/^[0-9a-fA-F]*[[:space:]][[:space:]]*${BIN}_\\(.*\\)_$1\\.tar\\.gz\$/\\1/p" "$2" | head -n 1)"
  [ -n "$v" ] || die "the latest release has no archive for $1. Releases: $RELEASES_PAGE"
  validate_version "$v"
  printf '%s\n' "$v"
}

# can_write_under <dir>: true if <dir> exists and is writable, or does not exist
# yet and its nearest existing parent is writable (so mkdir -p could make it).
# Only tests; it creates nothing.
can_write_under() {
  d="$1"
  while [ ! -e "$d" ]; do
    parent="$(dirname "$d")"
    [ "$parent" != "$d" ] || return 1
    d="$parent"
  done
  [ -d "$d" ] && [ -w "$d" ]
}

# --- Install directory -------------------------------------------------------

# on_path <dir>: true if <dir> is a literal entry in $PATH.
on_path() {
  case ":${PATH:-}:" in
    *":$1:"*) return 0 ;;
    *) return 1 ;;
  esac
}

# print_path_guidance <dir>: the line that puts <dir> on PATH for the user's
# shell. Printed only; no shell startup file is edited.
print_path_guidance() {
  shell_path="${SHELL:-}"
  case "${shell_path##*/}" in
    zsh) add="echo 'export PATH=\"$1:\$PATH\"' >> ~/.zshrc" ;;
    bash) add="echo 'export PATH=\"$1:\$PATH\"' >> ~/.bashrc" ;;
    fish) add="fish_add_path $1" ;;
    *) add="echo 'export PATH=\"$1:\$PATH\"' >> ~/.profile" ;;
  esac
  info ""
  info "$1 is not on your PATH. Add it with:"
  info "    $add"
  info "then restart your shell."
}

# install_alias <dir>: link $ALIAS to $BIN inside <dir>, a relative symlink so
# the pair can move together. It adds a name and takes none: it is skipped, with
# the reason on stderr, when
#   - TREERIG_NO_ALIAS asks for that;
#   - the name is already in <dir> and is not a symlink to this treerig;
#   - another $ALIAS is on PATH, which the new link would shadow or be shadowed
#     by, either way confusing.
# A link that is already ours is left alone. `ln -s` without -f is the last
# guard: if something appears in the gap, it fails instead of replacing it.
# Sets ALIAS_OURS=1 when ${ALIAS} is a link to ${BIN} after this (created now,
# already in place, or provided elsewhere on PATH by a link to ${BIN}), which is
# when install_completions also covers it. Returns 0 whether or not the alias
# exists: it never fails an install that has already succeeded.
install_alias() {
  case "${TREERIG_NO_ALIAS:-}" in
    "" | 0) : ;;
    *) return 0 ;;
  esac
  alias_path="$1/$ALIAS"
  if [ -L "$alias_path" ]; then
    alias_target="$(readlink "$alias_path" 2>/dev/null)" || alias_target=""
    if [ "$alias_target" = "$BIN" ] || [ "$alias_target" = "$1/$BIN" ]; then
      info "The ${ALIAS} alias is already in place."
      ALIAS_OURS=1
      return 0
    fi
    err "skipped the ${ALIAS} alias: $alias_path is a symlink to '$alias_target', not to $BIN, so it was left alone. Move it aside if you want the alias."
    return 0
  fi
  if [ -e "$alias_path" ]; then
    err "skipped the ${ALIAS} alias: $alias_path exists and is not a symlink to $BIN, so it was left alone. Move it aside if you want the alias."
    return 0
  fi
  alias_other="$(command -v "$ALIAS" 2>/dev/null)" || alias_other=""
  if [ -n "$alias_other" ]; then
    # A trig that is itself a link to a treerig (another install of this
    # tool) is not a clash, only already provided.
    alias_via="$(readlink "$alias_other" 2>/dev/null)" || alias_via=""
    case "${alias_via##*/}" in
      "$BIN")
        err "skipped the ${ALIAS} alias: it is already provided by $alias_other, a link to $BIN."
        ALIAS_OURS=1
        ;;
      *) err "skipped the ${ALIAS} alias: a different ${ALIAS} is already on your PATH ($alias_other). Use ${BIN}, or set TREERIG_NO_ALIAS=1 to stop seeing this." ;;
    esac
    return 0
  fi
  if ln -s "$BIN" "$alias_path"; then
    info "Linked ${ALIAS} to ${BIN} (a short alias; TREERIG_NO_ALIAS=1 skips it)."
    ALIAS_OURS=1
  else
    err "could not create the ${ALIAS} alias at $alias_path; ${BIN} is installed without it."
  fi
  return 0
}

# --- Shell completion --------------------------------------------------------

ZSH_BEGIN="# >>> treerig completion (added by install.sh; delete this block to undo) >>>"
ZSH_END="# <<< treerig completion <<<"
# The markers are matched by these prefixes, not by their whole text: a future
# release may reword the parenthesis, and an older block must still be found.
ZSH_BEGIN_MATCH="# >>> treerig completion"
ZSH_END_MATCH="# <<< treerig completion <<<"
GUIDE_URL="https://github.com/${REPO}/blob/main/docs/install.md"

# xdg_dir <value> <default>: an XDG base directory is used only when it is
# absolute, as the XDG spec says; otherwise <default>.
xdg_dir() {
  case "$1" in
    /*) printf '%s\n' "$1" ;;
    *) printf '%s\n' "$2" ;;
  esac
}

# report <text>: one line of the completion report, under its heading.
report() {
  if [ "$report_started" = 0 ]; then
    info ""
    info "Shell completion:"
    report_started=1
  fi
  info "  $*"
}

# put_completion <source> <dest>: copy one script to <dest>, creating its
# directories (755, whatever the umask: zsh's compinit distrusts a group-
# writable fpath directory; directories that already exist are not touched).
# The source must be a regular file, not a symlink. It writes a private
# temporary file beside the destination and renames it over, so a killed
# install leaves no half-written script, and a re-run replaces its own file. It
# leaves a symlink or a directory at <dest> alone (a dotfiles manager may own
# it). Returns non-zero, after saying why on stderr, when it wrote nothing.
put_completion() {
  if [ ! -f "$1" ] || [ -L "$1" ]; then
    err "skipped $2: ${1##*/} in the archive is not a regular file."
    return 1
  fi
  pc_dir="${2%/*}"
  if [ -L "$2" ] || [ -d "$2" ]; then
    err "left $2 alone: it is a symlink or a directory, not a file this script wrote."
    return 1
  fi
  if ! (umask 022 && mkdir -p "$pc_dir") 2>/dev/null; then
    err "could not create $pc_dir, so $2 was not written."
    return 1
  fi
  if ! pc_tmp="$(mktemp "$pc_dir/.treerig-XXXXXX" 2>/dev/null)"; then
    err "could not write in $pc_dir, so $2 was not written."
    return 1
  fi
  if ! { cp "$1" "$pc_tmp" && chmod 644 "$pc_tmp" && mv -f "$pc_tmp" "$2"; } 2>/dev/null; then
    rm -f "$pc_tmp"
    err "could not write $2."
    return 1
  fi
  return 0
}

# has_bash_completion: true if one of the places bash-completion 2 installs
# its loader exists. A cheap guess, only used to decide whether to print a hint.
has_bash_completion() {
  for hbc_file in \
    /usr/share/bash-completion/bash_completion \
    /usr/local/share/bash-completion/bash_completion \
    /opt/homebrew/share/bash-completion/bash_completion \
    /usr/local/etc/profile.d/bash_completion.sh \
    /opt/homebrew/etc/profile.d/bash_completion.sh; do
    [ -r "$hbc_file" ] && return 0
  done
  return 1
}

# system_completion <shell> <command>: print the path of a system-wide
# completion script for <command>, if there is one: where the .deb, .rpm and
# .apk put it (.goreleaser.yaml), or in Homebrew's prefix when brew is on PATH.
# A user-level copy would shadow it, and this one may be newer.
system_completion() {
  # Homebrew links a formula's bash completions into etc/bash_completion.d, not
  # share/bash-completion/completions, so bash checks both.
  case "$1" in
    fish) sc_rels="share/fish/vendor_completions.d/$2.fish" ;;
    bash) sc_rels="share/bash-completion/completions/$2 etc/bash_completion.d/$2" ;;
    zsh) sc_rels="share/zsh/site-functions/_$2" ;;
  esac
  for sc_prefix in /usr "$BREW_PREFIX"; do
    [ -n "$sc_prefix" ] || continue
    for sc_rel in $sc_rels; do
      if [ -f "$sc_prefix/$sc_rel" ]; then
        printf '%s\n' "$sc_prefix/$sc_rel"
        return 0
      fi
    done
  done
  return 1
}

# zsh_rc_block <dir> <command>...: print the block for .zshrc. <dir> goes in
# single-quoted, so a space (or a quote) in it is safe. The block works whether
# or not the file ran compinit before it: with compinit already run it registers
# the functions itself (compinit has scanned fpath by then); without, it runs
# compinit, which finds them on the new fpath.
zsh_rc_block() {
  zb_dir="$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
  shift
  printf '%s\n' "$ZSH_BEGIN" "fpath=('$zb_dir' \$fpath)" 'if (( $+functions[compdef] )); then'
  for zb_cmd in "$@"; do
    printf '  autoload -Uz _%s && compdef _%s %s\n' "$zb_cmd" "$zb_cmd" "$zb_cmd"
  done
  printf '%s\n' 'else' '  autoload -Uz compinit && compinit' 'fi' "$ZSH_END"
}

# setup_zsh_rc <dir> <command>...: add the block to ${ZDOTDIR:-$HOME}/.zshrc,
# once, in one write (a blank line, if the file has content, then the block, so
# a failure cannot leave half of it). It creates the file if it is missing. It
# never rewrites a block that is there. It does not edit a symlink or a file it
# cannot write, or one with a begin marker and no end marker: it prints the
# lines to add, or what to fix. Sets ZSH_RC_PENDING=1 when the file is not in
# the state the block would put it in.
setup_zsh_rc() {
  rc="${ZDOTDIR:-$HOME}/.zshrc"
  if [ -f "$rc" ] && grep -q "^$ZSH_BEGIN_MATCH" "$rc" 2>/dev/null; then
    if ! grep -q "^$ZSH_END_MATCH" "$rc" 2>/dev/null; then
      err "did not edit $rc: it has an unfinished treerig block (a begin marker with no end marker); remove it and re-run."
      ZSH_RC_PENDING=1
      return 0
    fi
    report "zsh: left $rc as it was; it already has the treerig completion block."
    if [ "$#" -gt 2 ] && ! grep -qF "compdef _${ALIAS} ${ALIAS}" "$rc"; then
      info "      The block does not cover ${ALIAS}. To add it, put this line after the others in the block:"
      info "        autoload -Uz _${ALIAS} && compdef _${ALIAS} ${ALIAS}"
    fi
    return 0
  fi
  rc_why=""
  rc_text=0
  rc_compinit=0
  if [ -L "$rc" ]; then
    rc_why="it is a symlink"
  elif [ -e "$rc" ] && { [ ! -f "$rc" ] || [ ! -w "$rc" ]; }; then
    rc_why="it is not a writable file"
  elif [ ! -e "$rc" ] && { [ ! -d "${rc%/*}" ] || [ ! -w "${rc%/*}" ]; }; then
    rc_why="${rc%/*} is not a writable directory"
  elif [ -s "$rc" ]; then
    rc_text=1
  fi
  if [ -z "$rc_why" ]; then
    grep -qF compinit "$rc" 2>/dev/null && rc_compinit=1
    rc_block="$(zsh_rc_block "$@")"
    if [ "$rc_text" = 1 ]; then
      rc_block="
$rc_block"
    fi
    if printf '%s\n' "$rc_block" >>"$rc" 2>/dev/null; then
      ZSH_RC_ADDED=1
      if [ "$rc_compinit" = 1 ]; then
        report "zsh: added a block to $rc."
      else
        report "zsh: added a block to $rc. It runs compinit if your zsh setup hasn't, which adds a little start-up time."
      fi
      return 0
    fi
    err "could not append to $rc; it may not have been edited. Check it for a partial treerig block, and add the block by hand:"
    info "To add these lines to $rc:"
    zsh_rc_block "$@"
    info ""
    ZSH_RC_PENDING=1
    return 0
  fi
  err "did not edit $rc: $rc_why."
  info "To finish by hand, add these lines to $rc:"
  zsh_rc_block "$@"
  info ""
  ZSH_RC_PENDING=1
  return 0
}

# install_completions: copy the archive's completion scripts to where fish,
# bash and zsh find them, for each shell that is on PATH, and set zsh up in
# .zshrc when it is the login shell. Nothing here runs the new binary. Every
# problem is one `err` line and the install still succeeds: it always returns 0.
# Needs ALIAS_OURS (see install_alias) to decide whether trig is covered.
install_completions() {
  [ "$no_completions" = 0 ] || return 0
  ic_uid="$(id -u 2>/dev/null)" || ic_uid=""
  case "$ic_uid" in
    0)
      err "skipped shell completion: this is running as root, and HOME may belong to another user. Run it again as that user, or see $GUIDE_URL#shell-completion"
      return 0
      ;;
    "")
      err "skipped shell completion: could not tell which user this is (id -u failed)."
      return 0
      ;;
  esac
  case "${HOME:-}" in
    /*) : ;;
    *)
      err "skipped shell completion: HOME is not set to an absolute path."
      return 0
      ;;
  esac
  case "${ZDOTDIR:-/}" in
    /*) : ;;
    *)
      err "skipped shell completion: ZDOTDIR is not an absolute path."
      return 0
      ;;
  esac

  ic_shells=""
  for ic_shell in fish bash zsh; do
    if command -v "$ic_shell" >/dev/null 2>&1; then
      ic_shells="$ic_shells $ic_shell"
    fi
  done
  if [ -z "$ic_shells" ]; then
    info "No bash, zsh or fish is on your PATH, so no shell completion was set up."
    return 0
  fi

  # Only files from the verified archive, extracted one command at a time so a
  # missing file costs only its own command.
  ic_cmds=""
  ic_want="$BIN"
  if [ "$ALIAS_OURS" = 1 ]; then
    ic_want="$BIN $ALIAS"
  fi
  for ic_cmd in $ic_want; do
    if tar -xzf "$tmp/$archive" -C "$tmp" "completions/$ic_cmd.bash" "completions/_$ic_cmd" "completions/$ic_cmd.fish" 2>/dev/null; then
      ic_cmds="$ic_cmds $ic_cmd"
    else
      err "could not extract the completion scripts for $ic_cmd from the archive (an older release may not include them)."
    fi
  done
  [ -n "$ic_cmds" ] || return 0

  BREW_PREFIX=""
  if command -v brew >/dev/null 2>&1; then
    BREW_PREFIX="$(brew --prefix 2>/dev/null | head -n 1)" || BREW_PREFIX=""
    case "$BREW_PREFIX" in /*) : ;; *) BREW_PREFIX="" ;; esac
  fi

  ic_data="$(xdg_dir "${XDG_DATA_HOME:-}" "$HOME/.local/share")"
  ic_config="$(xdg_dir "${XDG_CONFIG_HOME:-}" "$HOME/.config")"
  ic_login="${SHELL:-}"
  ic_login="${ic_login##*/}"
  report_started=0
  ic_zsh_cmds=""
  ic_zsh_dir=""
  ic_changed=0
  ZSH_RC_PENDING=0
  ZSH_RC_ADDED=0

  for ic_shell in $ic_shells; do
    case "$ic_shell" in
      fish) ic_dir="$ic_config/fish/completions" ;;
      bash) ic_dir="$ic_data/bash-completion/completions" ;;
      zsh) ic_dir="$ic_data/zsh/site-functions" ;;
    esac
    ic_done=""
    ic_done_cmds=""
    ic_cover=""
    for ic_cmd in $ic_cmds; do
      if ic_sys="$(system_completion "$ic_shell" "$ic_cmd")"; then
        report "$ic_shell: kept the system-wide $ic_sys; wrote no user-level copy, which would shadow it."
        ic_cover="$ic_cover $ic_cmd"
        continue
      fi
      case "$ic_shell" in
        fish) ic_src="$ic_cmd.fish"; ic_name="$ic_cmd.fish" ;;
        bash) ic_src="$ic_cmd.bash"; ic_name="$ic_cmd" ;;
        zsh) ic_src="_$ic_cmd"; ic_name="_$ic_cmd" ;;
      esac
      if put_completion "$tmp/completions/$ic_src" "$ic_dir/$ic_name"; then
        ic_done="${ic_done:+$ic_done, }$ic_name"
        ic_done_cmds="$ic_done_cmds $ic_cmd"
        ic_cover="$ic_cover $ic_cmd"
      fi
    done
    if [ "$ic_shell" = zsh ]; then
      ic_zsh_cmds="${ic_cover# }"
      ic_zsh_dir="$ic_dir"
    fi
    [ -n "$ic_done" ] || continue
    ic_changed=1
    ic_note=""
    if [ "$ic_shell" != "$ic_login" ]; then
      ic_note="; in place for when you use $ic_shell"
      case "$ic_shell" in
        bash) ic_note="$ic_note, which needs bash-completion 2" ;;
        # zsh does not autoload from there: only the .zshrc block of a zsh
        # login shell puts the directory on fpath.
        zsh) ic_note="; to use them in zsh, add the fpath lines from $GUIDE_URL#shell-completion" ;;
      esac
    fi
    report "$ic_shell: wrote $ic_done to $ic_dir$ic_note"
    if [ "$ic_shell" = bash ] && [ "$ic_login" = bash ] && ! has_bash_completion; then
      info "      bash loads these through bash-completion 2, which does not look installed."
      info "      Debian, Ubuntu: apt install bash-completion. macOS: brew install bash-completion@2, with Homebrew's bash (the system bash 3.2 is too old)."
    fi
  done

  if [ "$ic_login" = zsh ] && [ -n "$ic_zsh_cmds" ]; then
    # shellcheck disable=SC2086 # one word per command, no spaces in them
    setup_zsh_rc "$ic_zsh_dir" $ic_zsh_cmds
  fi
  if [ "$ZSH_RC_PENDING" = 1 ]; then
    info "After adding those lines, or fixing the file, open a new terminal."
  elif [ "$ic_changed" = 1 ] || [ "$ZSH_RC_ADDED" = 1 ]; then
    info "Open a new terminal to use completion."
  fi
  if [ "$ic_changed" = 1 ] || [ "$ZSH_RC_ADDED" = 1 ]; then
    info "To undo: $GUIDE_URL#undoing-the-completion-setup"
  fi
  return 0
}

# --- Main --------------------------------------------------------------------

if [ -z "$want_dir" ]; then
  [ -n "${HOME:-}" ] || die "HOME is not set; pass --dir or set TREERIG_INSTALL_DIR"
  want_dir="$HOME/.local/bin"
fi

platform="$(detect_platform)"

# Before any download: a directory we cannot write to is the commonest failure.
can_write_under "$want_dir" ||
  die "cannot write to $want_dir; choose another directory with --dir, or run this as a user who can write there"

tmp="$(mktemp -d 2>/dev/null || mktemp -d -t treerig)"
cleanup() { rm -rf "$tmp"; }
# A signal must end the script, not just clean up and carry on, or Ctrl-C
# could still finish the install it was meant to cancel.
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM
trap 'cleanup; exit 129' HUP

version="$(resolve_version "$platform" "$tmp/latest-checksums.txt")"
archive="${BIN}_${version}_${platform}.tar.gz"
release_url="${BASE_URL}/v${version}"

info "Installing ${BIN} ${version} (${platform})"

download "${release_url}/${archive}" "$tmp/$archive" ||
  die "failed to download ${release_url}/${archive}; check that release ${version} exists and has a build for ${platform}. Releases: $RELEASES_PAGE"
download "${release_url}/checksums.txt" "$tmp/checksums.txt" ||
  die "failed to download ${release_url}/checksums.txt; check that release ${version} exists. Releases: $RELEASES_PAGE"

# The line for this archive, by exact file name (the second field).
expected="$(awk -v f="$archive" '$2 == f { print $1; exit }' "$tmp/checksums.txt")"
[ -n "$expected" ] || die "checksums.txt has no entry for $archive"
actual="$(sha256_of "$tmp/$archive")"
if [ "$expected" != "$actual" ]; then
  err "checksum mismatch for $archive; refusing to install."
  err "  expected: $expected"
  err "  actual:   $actual"
  die "aborting; nothing was installed"
fi
info "Checksum verified."

tar -xzf "$tmp/$archive" -C "$tmp" "$BIN" || die "$archive did not contain $BIN"
[ -f "$tmp/$BIN" ] || die "$archive did not contain $BIN"

mkdir -p "$want_dir" || die "cannot create $want_dir"
if [ ! -w "$want_dir" ]; then
  die "cannot write to $want_dir; choose another directory with --dir, or run this as a user who can write there"
fi
dest="$want_dir/$BIN"
[ ! -d "$dest" ] || die "$dest is a directory; cannot install the binary there"

# Copy beside the destination, then rename over it: an interrupted install
# never leaves a half-written binary, and a running treerig keeps its old file.
tmp_dest="$dest.tmp.$$"
if ! { cp "$tmp/$BIN" "$tmp_dest" && chmod 755 "$tmp_dest" && mv -f "$tmp_dest" "$dest"; }; then
  rm -f "$tmp_dest"
  die "could not install $dest"
fi

installed="$("$dest" --version 2>&1)" ||
  die "installed $dest, but it does not run: $installed"
info "Installed ${BIN} ${installed} to ${dest}"

ALIAS_OURS=0
install_alias "$want_dir"

if on_path "$want_dir"; then
  info "Run '${BIN} doctor' to check Git, Docker and Docker Compose."
else
  print_path_guidance "$want_dir"
fi

install_completions || err "shell completion setup stopped early; ${BIN} is installed."
