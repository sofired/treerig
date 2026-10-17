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
#
# Only the verified treerig binary is ever run, and only to print its version.
# Nothing is written outside a private temporary directory until the checksum
# matches.
#
# The file names and the archive layout it relies on are listed in
# docs/design/release.md.
#
# Options:
#   -d, --dir DIR        Install into DIR (used as given).
#   -v, --version VER    Install VER (1.2.3 or v1.2.3) instead of the latest.
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
#
# POSIX sh only, since it is piped into `sh`; passes `shellcheck -s sh`.

set -eu

REPO="sofired/treerig"
BIN="treerig"
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
Usage: install.sh [--dir DIR] [--version VER]

Installs the treerig release for this machine, after checking its sha256 against
the release's checksums.txt.

  -d, --dir DIR        Install into DIR (default: $TREERIG_INSTALL_DIR, else
                       $HOME/.local/bin).
  -v, --version VER    Install VER (1.2.3 or v1.2.3) instead of the latest
                       (default: $TREERIG_VERSION).
  -h, --help           Show this help.
USAGE
}

# --- Arguments ---------------------------------------------------------------

want_dir="${TREERIG_INSTALL_DIR:-}"
want_version="${TREERIG_VERSION:-}"
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

if on_path "$want_dir"; then
  info "Run '${BIN} doctor' to check Git, Docker and Docker Compose."
else
  print_path_guidance "$want_dir"
fi
info "Shell completion: ${BIN} completion bash|zsh|fish (see https://github.com/sofired/treerig/blob/main/docs/install.md#shell-completion)."
