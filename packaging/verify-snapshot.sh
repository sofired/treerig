#!/bin/sh
# Checks what `goreleaser release --snapshot --clean --skip=sign` left in
# dist/: every archive, package and checksum is there and agrees, each archive
# and the host's .deb hold the files they should, the host's packages carry the
# archive's binary at /usr/bin/treerig, every binary was built the way
# a release is (no cgo, trimmed paths, the version stamped in), and install.sh
# installs the host's archive through a local HTTP server standing in for GitHub
# Releases, with curl and with wget, and refuses an archive whose checksum does
# not match. The `trig` alias is checked too: the host's packages ship it as a
# symlink to treerig, and install.sh links it, skips it (without touching
# anything) when another `trig` is in the way, and skips it when asked. Shell
# completion is checked too, in a scratch HOME: which shells' files are written
# (a shell is "present" when it is on PATH), the zsh block added once, a symlinked
# .zshrc left alone, the opt-outs, no `trig` files without the alias, and a HOME
# with a space in it, a damaged .zshrc block, directory modes under umask 002, a
# system-wide copy kept in place, and a bad or missing script in the archive; with
# zsh on this machine, a real interactive zsh loads them (CI requires zsh).
#
# usage: packaging/verify-snapshot.sh [dist-dir]
#
# Needs go, python3 and tar, plus dpkg-deb for the deb checks and rpm2cpio and
# cpio for the rpm's: without them those checks are skipped, except under CI
# (CI is set), where they fail. Runs on Linux or macOS. CI runs it after the
# snapshot build (.github/workflows/go.yml); run it by hand the same way.
set -eu

dist=$(cd "${1:-dist}" && pwd)
repo=$(cd "$(dirname "$0")/.." && pwd)

fail() {
  printf 'verify-snapshot: %s\n' "$*" >&2
  exit 1
}

version=$(sed -n 's/.*"version":"\([^"]*\)".*/\1/p' "$dist/metadata.json")
[ -n "$version" ] || fail "no version in $dist/metadata.json"

work=$(mktemp -d)
server_pid=
cleanup() {
  [ -z "$server_pid" ] || kill "$server_pid" 2>/dev/null || true
  rm -rf "$work"
}
# A signal has to stop the script, not just clean up and carry on.
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM
trap 'cleanup; exit 129' HUP

# --- The artifacts ------------------------------------------------------------

count() { find "$dist" -maxdepth 1 -type f -name "$1" | wc -l | tr -d ' '; }

expect() {
  [ "$(count "$1")" = "$2" ] || fail "expected $2 files matching $1 in $dist, found $(count "$1")"
}
# Four platform archives and the source archive.
expect '*.tar.gz' 5
expect '*.deb' 2
expect '*.rpm' 2
expect '*.apk' 2
[ -f "$dist/checksums.txt" ] || fail "checksums.txt is missing"

for os in linux darwin; do
  for arch in amd64 arm64; do
    [ -f "$dist/treerig_${version}_${os}_${arch}.tar.gz" ] ||
      fail "missing archive treerig_${version}_${os}_${arch}.tar.gz"
  done
done
source_archive="treerig-${version}.tar.gz"
[ -f "$dist/$source_archive" ] || fail "missing source archive $source_archive"

# Every artifact has a line in checksums.txt, and every line is true.
listed=$(wc -l <"$dist/checksums.txt" | tr -d ' ')
[ "$listed" = 11 ] || fail "checksums.txt lists $listed files, expected 11"
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$dist" && sha256sum -c checksums.txt >/dev/null) || fail "checksums.txt does not match dist/"
else
  (cd "$dist" && shasum -a 256 -c checksums.txt >/dev/null) || fail "checksums.txt does not match dist/"
fi
echo "artifacts: ok (5 tar.gz, 2 deb, 2 rpm, 2 apk, checksums.txt)"

# --- Contents -------------------------------------------------------------------

# Each platform archive holds exactly these, with treerig at the root.
want_archive="LICENSE
NOTICE
README.md
completions/_treerig
completions/_trig
completions/treerig.bash
completions/treerig.fish
completions/trig.bash
completions/trig.fish
treerig"
for os in linux darwin; do
  for arch in amd64 arm64; do
    got=$(tar tzf "$dist/treerig_${version}_${os}_${arch}.tar.gz" | LC_ALL=C sort)
    [ "$got" = "$want_archive" ] || {
      printf '%s\n' "$got" >&2
      fail "treerig_${version}_${os}_${arch}.tar.gz does not hold exactly: $(printf '%s' "$want_archive" | tr '\n' ' ')"
    }
  done
done
echo "archive contents: ok"

# The source archive is the repository at the tag, under one directory.
source_list=$(tar tzf "$dist/$source_archive")
for f in go.mod go.sum LICENSE cmd/treerig/main.go; do
  printf '%s\n' "$source_list" | grep -qx "treerig-${version}/$f" ||
    fail "$source_archive has no treerig-${version}/$f"
done
printf '%s\n' "$source_list" | grep -v "^treerig-${version}/" | grep -q . &&
  fail "$source_archive holds files outside treerig-${version}/"
echo "source archive: ok"

# The tap formula, rendered the way the release workflow's tap job renders it:
# packaging/homebrew/treerig.rb pointed at this build's source archive, with the
# checksum checksums.txt lists for it. It lands in dist/ beside the rest.
source_sha=$(awk -v f="$source_archive" '$2 == f { print $1; exit }' "$dist/checksums.txt")
printf '%s' "$source_sha" | grep -Eq '^[0-9a-f]{64}$' ||
  fail "checksums.txt has no sha256 for $source_archive"
source_url="https://github.com/sofired/treerig/releases/download/v${version}/${source_archive}"
cp "$repo/packaging/homebrew/treerig.rb" "$dist/treerig.rb"
python3 "$repo/packaging/homebrew/set-source.py" "$dist/treerig.rb" "$source_url" "$source_sha" ||
  fail "could not render the formula"
grep -qxF "  url \"$source_url\"" "$dist/treerig.rb" || fail "treerig.rb does not point at $source_url"
grep -qxF "  sha256 \"$source_sha\"" "$dist/treerig.rb" || fail "treerig.rb does not carry $source_archive's sha256"
grep -qF 'head "https://github.com/sofired/treerig.git", branch: "main"' "$dist/treerig.rb" ||
  fail "rendering treerig.rb changed its head stanza"
# The formula stamps the version through the same -X flag as the release.
grep -qF -- '-X github.com/sofired/treerig/internal/brand.Version=' "$dist/treerig.rb" ||
  fail "treerig.rb does not stamp brand.Version"
if command -v ruby >/dev/null 2>&1; then
  ruby -c "$dist/treerig.rb" >/dev/null || fail "treerig.rb is not valid Ruby"
  echo "formula: ok (dist/treerig.rb)"
else
  echo "formula: ok (dist/treerig.rb; Ruby syntax not checked, no ruby on this machine)"
fi

# Every binary was built without cgo and with trimmed paths, and carries the
# stamped version.
for dir in "$dist"/treerig_linux_amd64_* "$dist"/treerig_linux_arm64_* "$dist"/treerig_darwin_amd64_* "$dist"/treerig_darwin_arm64_*; do
  info=$(go version -m "$dir/treerig") || fail "go version -m failed on $dir/treerig"
  printf '%s\n' "$info" | grep -q 'CGO_ENABLED=0' || fail "$dir/treerig was not built with CGO_ENABLED=0"
  printf '%s\n' "$info" | grep -q -- '-trimpath=true' || fail "$dir/treerig was not built with -trimpath"
  # The -X flag itself is not recorded in the build info, but the string it
  # sets is in the binary.
  grep -qaF -- "$version" "$dir/treerig" || fail "$dir/treerig does not carry the version $version"
done
echo "binaries: ok (CGO_ENABLED=0, -trimpath, version stamped)"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) fail "unsupported host architecture $(uname -m)" ;;
esac

# not_checked <label> <tool>: a package check that cannot run here. It
# is skipped on a machine that is not Linux, or where a reader is missing, but
# fails under CI (GitHub sets CI=true): there a missing tool would make the check
# pass without ever running.
not_checked() {
  if [ "$os" != linux ]; then
    echo "$1: skipped, not Linux"
  elif [ -n "${CI:-}" ]; then
    fail "$1: $2 is not on this machine, and CI does not skip this check"
  else
    echo "$1: skipped, no $2 on this machine"
  fi
}

# The host's .deb, where dpkg-deb is here to read it.
if [ "$os" = linux ] && command -v dpkg-deb >/dev/null 2>&1; then
  deb=$(find "$dist" -maxdepth 1 -name "*_${arch}.deb")
  want_deb="-rwxr-xr-x /usr/bin/treerig
lrwxrwxrwx /usr/bin/trig
-rw-r--r-- /usr/share/bash-completion/completions/treerig
-rw-r--r-- /usr/share/bash-completion/completions/trig
-rw-r--r-- /usr/share/doc/treerig/NOTICE
-rw-r--r-- /usr/share/doc/treerig/copyright
-rw-r--r-- /usr/share/fish/vendor_completions.d/treerig.fish
-rw-r--r-- /usr/share/fish/vendor_completions.d/trig.fish
-rw-r--r-- /usr/share/zsh/vendor-completions/_treerig
-rw-r--r-- /usr/share/zsh/vendor-completions/_trig"
  got_deb=$(dpkg-deb -c "$deb" | awk '$1 !~ /^d/ { sub(/^\./, "", $6); print $1, $6 }' | LC_ALL=C sort -k2)
  [ "$got_deb" = "$want_deb" ] || {
    printf '%s\n' "$got_deb" >&2
    fail "$(basename "$deb") does not hold exactly the expected files and modes"
  }
  # The alias is a relative symlink to the binary beside it.
  deb_listing=$(dpkg-deb -c "$deb") || fail "dpkg-deb could not list $(basename "$deb")"
  printf '%s\n' "$deb_listing" | grep -q ' \./usr/bin/trig -> treerig$' ||
    fail "$(basename "$deb") does not link /usr/bin/trig to treerig"
  # Debian's and Ubuntu's zsh look for completions in vendor-completions, not
  # site-functions; where this is such a system with zsh, check that it does.
  if [ -f /etc/debian_version ] && command -v zsh >/dev/null 2>&1; then
    # shellcheck disable=SC2016 # zsh expands it
    zsh -fc 'print -rl -- $fpath' | grep -qxF /usr/share/zsh/vendor-completions ||
      fail "this Debian-family zsh does not have /usr/share/zsh/vendor-completions on its fpath"
  fi
  echo "deb contents: ok (with the trig symlink, zsh completions in vendor-completions)"
else
  not_checked "deb contents" dpkg-deb
fi

# (.github/scripts/release-smoke.sh reads a published release's packages the same
# way, by its own copy of this code, since this script is tied to dist/: keep
# the two in step.)
# /usr/bin/treerig in the host's packages is the release binary: the same bytes as
# the treerig in the host's archive, which is the file the teardown pin records as
# binary_sha256 (docs/design/teardown.md, "Platforms"). The deb is read with
# dpkg-deb and the rpm with rpm2cpio and cpio, where each is here.
sha256_stdin() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{ print $1 }'
  else
    shasum -a 256 | awk '{ print $1 }'
  fi
}

# The archive is read into a file first, so a failing tar is a failure here and
# not an empty hash.
tar xzf "$dist/treerig_${version}_${os}_${arch}.tar.gz" -C "$work" treerig ||
  fail "could not read treerig from treerig_${version}_${os}_${arch}.tar.gz"
archive_sha=$(sha256_stdin <"$work/treerig")
rm -f "$work/treerig"
printf '%s' "$archive_sha" | grep -Eq '^[0-9a-f]{64}$' || fail "could not hash the archive's treerig"

# package_binary_matches <label> <package> <sha>: fail unless <sha>, the hash of
# the package's /usr/bin/treerig, is the archive's.
package_binary_matches() {
  printf '%s' "$3" | grep -Eq '^[0-9a-f]{64}$' || fail "$1: could not read /usr/bin/treerig from $(basename "$2")"
  [ "$3" = "$archive_sha" ] ||
    fail "$1: /usr/bin/treerig in $(basename "$2") is not the treerig in the ${os}_${arch} archive (sha256 $3, the archive's is $archive_sha)"
}

if [ "$os" = linux ] && command -v dpkg-deb >/dev/null 2>&1; then
  dpkg-deb --fsys-tarfile "$deb" >"$work/deb.tar" || fail "dpkg-deb could not read $(basename "$deb")"
  tar xOf "$work/deb.tar" ./usr/bin/treerig >"$work/deb-treerig" 2>/dev/null || true
  [ -s "$work/deb-treerig" ] || fail "no /usr/bin/treerig in $(basename "$deb")"
  deb_sha=$(sha256_stdin <"$work/deb-treerig")
  rm -f "$work/deb.tar" "$work/deb-treerig"
  package_binary_matches deb "$deb" "$deb_sha"
  echo "deb binary: ok (/usr/bin/treerig is the archive's treerig)"
else
  not_checked "deb binary" dpkg-deb
fi
if [ "$os" = linux ] && command -v rpm2cpio >/dev/null 2>&1 && command -v cpio >/dev/null 2>&1; then
  case "$arch" in amd64) rpm_arch=x86_64 ;; *) rpm_arch=aarch64 ;; esac
  rpm=$(find "$dist" -maxdepth 1 -name "*.${rpm_arch}.rpm")
  [ -f "$rpm" ] || fail "expected one .rpm for $rpm_arch in $dist"
  # Ubuntu's rpm2cpio (rpm 4.18) can exit 1, silently, after writing the whole
  # payload of a GoReleaser rpm, so its status is not the test: an empty or cut
  # payload is caught below, by the missing file or the hash.
  rpm2cpio "$rpm" >"$work/rpm.cpio" || true
  [ -s "$work/rpm.cpio" ] || fail "rpm2cpio could not read $(basename "$rpm")"
  # rpm2cpio spells the member ./usr/bin/treerig or /usr/bin/treerig, by its version.
  cpio -i --to-stdout ./usr/bin/treerig /usr/bin/treerig usr/bin/treerig <"$work/rpm.cpio" >"$work/rpm-treerig" 2>/dev/null || true
  [ -s "$work/rpm-treerig" ] || fail "no /usr/bin/treerig in $(basename "$rpm")"
  rpm_sha=$(sha256_stdin <"$work/rpm-treerig")
  # The alias: a symlink to treerig. cpio -tv spells the member with or without
  # a leading ./ or / by rpm2cpio's version. (Matched on the listing's text; the
  # payload is read once and not extracted.)
  rpm_listing=$(cpio -itv <"$work/rpm.cpio" 2>/dev/null) || true
  printf '%s\n' "$rpm_listing" | grep -Eq '^l.* (\./|/)?usr/bin/trig -> treerig$' ||
    fail "$(basename "$rpm") does not link /usr/bin/trig to treerig"
  for f in _treerig _trig; do
    printf '%s\n' "$rpm_listing" | grep -Eq " (\./|/)?usr/share/zsh/site-functions/$f\$" ||
      fail "$(basename "$rpm") does not put $f in /usr/share/zsh/site-functions"
  done
  rm -f "$work/rpm.cpio" "$work/rpm-treerig"
  package_binary_matches rpm "$rpm" "$rpm_sha"
  echo "rpm binary: ok (/usr/bin/treerig is the archive's treerig, /usr/bin/trig links to it, zsh completions in site-functions)"
else
  if command -v rpm2cpio >/dev/null 2>&1; then missing=cpio; else missing=rpm2cpio; fi
  not_checked "rpm binary" "$missing"
fi

# The host's .apk is concatenated gzip streams of tar, which tar reads through;
# it holds the same alias symlink.
case "$arch" in amd64) apk_arch=x86_64 ;; *) apk_arch=aarch64 ;; esac
apk=$(find "$dist" -maxdepth 1 -name "*_${apk_arch}.apk")
[ -f "$apk" ] || fail "expected one .apk for $apk_arch in $dist"
apk_listing=$(tar tzvf "$apk" 2>/dev/null) || true
printf '%s\n' "$apk_listing" | grep -q ' usr/bin/trig -> treerig$' ||
  fail "$(basename "$apk") does not link /usr/bin/trig to treerig"
for f in _treerig _trig; do
  printf '%s\n' "$apk_listing" | grep -q " usr/share/zsh/site-functions/$f\$" ||
    fail "$(basename "$apk") does not put $f in /usr/share/zsh/site-functions"
done
echo "apk contents: ok (with the trig symlink, zsh completions in site-functions)"

# --- install.sh against a local stand-in for GitHub Releases -------------------

archive="treerig_${version}_${os}_${arch}.tar.gz"

mkdir -p "$work/good/v$version" "$work/good/latest" "$work/bad/v$version"
cp "$dist/$archive" "$dist/checksums.txt" "$work/good/v$version/"
cp "$dist/checksums.txt" "$work/good/latest/"
cp "$dist/checksums.txt" "$work/bad/v$version/"
{ cat "$dist/$archive"; printf 'x'; } >"$work/bad/v$version/$archive"

serve() { # serve <dir>: start a server for <dir> and set $base to its URL
  python3 -u -m http.server 0 --bind 127.0.0.1 --directory "$1" >"$work/server.log" 2>&1 &
  server_pid=$!
  port=
  for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    port=$(sed -n 's/.*port \([0-9][0-9]*\).*/\1/p' "$work/server.log" | head -n 1)
    [ -z "$port" ] || break
    sleep 0.25
  done
  [ -n "$port" ] || fail "the test server did not start"
  base="http://127.0.0.1:$port"
}

stop_server() {
  kill "$server_pid" 2>/dev/null || true
  wait "$server_pid" 2>/dev/null || true
  server_pid=
}

# A directory of the tools install.sh needs and nothing else, to stand in for
# PATH where a `trig` already on this machine (a package install of treerig
# puts one in /usr/bin) would make the alias scenarios depend on the host. It
# holds curl as well, unlike the "no curl" directory below.
plain="$work/plain"
mkdir "$plain"
for t in uname mktemp sed awk head tar gzip cp chmod mv rm mkdir cat tr dirname readlink ln wget curl shasum sha256sum env sh id grep; do
  p=$(command -v "$t" 2>/dev/null || true)
  case "$p" in /*) ln -s "$p" "$plain/$t" ;; esac
done

# check_install <label> <dir> [env...]: run install.sh into <dir> and require
# the installed binary to report this snapshot's version, and trig beside it
# to be a symlink to it that runs as trig.
check_install() {
  label=$1
  dir=$2
  shift 2
  env "$@" sh "$repo/install.sh" --dir "$dir" >"$work/install.log" 2>&1 ||
    { cat "$work/install.log" >&2; fail "$label: install.sh failed"; }
  got=$("$dir/treerig" --version)
  [ "$got" = "$version" ] || fail "$label: installed treerig reports $got, expected $version"
  if [ ! -L "$dir/trig" ] || [ "$(readlink "$dir/trig")" != treerig ]; then
    cat "$work/install.log" >&2
    fail "$label: $dir/trig is not a symlink to treerig"
  fi
  [ "$("$dir/trig" --version)" = "$version" ] || fail "$label: trig does not report $version"
  "$dir/trig" --help 2>&1 | grep -q 'trig' || fail "$label: trig --help does not say trig"
  echo "install.sh ($label): ok"
}

# check_alias_skipped <label> <dir> <what-install-prints> [env...]: install.sh
# into <dir> still installs treerig, exits 0, and says why it left trig out.
check_alias_skipped() {
  label=$1
  dir=$2
  said=$3
  shift 3
  env "$@" sh "$repo/install.sh" --dir "$dir" >"$work/install.log" 2>&1 ||
    { cat "$work/install.log" >&2; fail "$label: install.sh failed"; }
  [ "$("$dir/treerig" --version)" = "$version" ] || fail "$label: treerig was not installed"
  grep -q "$said" "$work/install.log" || { cat "$work/install.log" >&2; fail "$label: no message saying '$said'"; }
}

# The scenarios below expect the alias by default; one opted out in the
# caller's environment would make every installer run skip it.
unset TREERIG_NO_ALIAS TREERIG_NO_COMPLETIONS XDG_DATA_HOME XDG_CONFIG_HOME XDG_CACHE_HOME ZDOTDIR

# install.sh writes under HOME (shell completion, and .zshrc for a zsh login
# shell). Every scenario below runs in a scratch HOME so it never touches the
# real one; the completion scenarios give themselves a HOME of their own.
HOME="$work/home"
SHELL=/bin/sh
export HOME SHELL
mkdir -p "$HOME"

serve "$work/good"
check_install "latest, curl or wget" "$work/t1" PATH="$plain" TREERIG_LATEST_URL="$base/latest/checksums.txt" TREERIG_BASE_URL="$base"
check_install "pinned version" "$work/t2" PATH="$plain" TREERIG_VERSION="v$version" TREERIG_BASE_URL="$base"

# Without curl, and then without sha256sum: a directory holding only the tools
# the script needs.
tools="uname mktemp sed awk head tar gzip cp chmod mv rm mkdir cat tr dirname readlink ln wget shasum sha256sum env sh id grep"
if command -v wget >/dev/null 2>&1; then
  mkdir "$work/nocurl"
  for t in $tools; do
    p=$(command -v "$t" 2>/dev/null || true)
    case "$p" in /*) ln -s "$p" "$work/nocurl/$t" ;; esac
  done
  check_install "wget, no curl" "$work/t3" PATH="$work/nocurl" TREERIG_LATEST_URL="$base/latest/checksums.txt" TREERIG_BASE_URL="$base"
else
  echo "install.sh (wget, no curl): skipped, no wget on this machine"
fi
if command -v shasum >/dev/null 2>&1 && command -v sha256sum >/dev/null 2>&1; then
  mkdir "$work/nosha256sum"
  for t in $tools; do
    [ "$t" = sha256sum ] && continue
    p=$(command -v "$t" 2>/dev/null || true)
    case "$p" in /*) ln -s "$p" "$work/nosha256sum/$t" ;; esac
  done
  check_install "shasum, no sha256sum" "$work/t4" PATH="$work/nosha256sum" TREERIG_VERSION="$version" TREERIG_BASE_URL="$base"
else
  echo "install.sh (shasum, no sha256sum): skipped, both tools are not here"
fi

# The alias never takes a name that is not ours. Each case runs against the
# plain tool directory (plus the one thing in the way), so what is on this
# machine's PATH cannot change the outcome.
env PATH="$plain" TREERIG_VERSION="$version" TREERIG_BASE_URL="$base" sh "$repo/install.sh" --dir "$work/t1" >"$work/install.log" 2>&1 ||
  { cat "$work/install.log" >&2; fail "install.sh: a second install over the first failed"; }
grep -q 'already in place' "$work/install.log" || fail "install.sh: a second install did not leave its own trig alone"
[ "$(readlink "$work/t1/trig")" = treerig ] || fail "install.sh: a second install changed the trig link"

mkdir "$work/t6"
printf 'mine\n' >"$work/t6/trig"
check_alias_skipped "a file named trig" "$work/t6" 'skipped the trig alias' PATH="$plain" TREERIG_VERSION="$version" TREERIG_BASE_URL="$base"
if [ -L "$work/t6/trig" ] || [ "$(cat "$work/t6/trig")" != mine ]; then
  fail "install.sh replaced a trig file that was not its own"
fi

mkdir "$work/t7"
ln -s /nonexistent-elsewhere "$work/t7/trig"
check_alias_skipped "a trig symlink elsewhere" "$work/t7" 'skipped the trig alias' PATH="$plain" TREERIG_VERSION="$version" TREERIG_BASE_URL="$base"
[ "$(readlink "$work/t7/trig")" = /nonexistent-elsewhere ] || fail "install.sh repointed a trig symlink that was not its own"

mkdir "$work/otherbin"
printf '#!/bin/sh\necho not-treerig\n' >"$work/otherbin/trig"
chmod 755 "$work/otherbin/trig"
check_alias_skipped "a different trig on PATH" "$work/t8" 'different trig is already on your PATH' PATH="$work/otherbin:$plain" TREERIG_VERSION="$version" TREERIG_BASE_URL="$base"
[ ! -e "$work/t8/trig" ] || fail "install.sh linked trig beside a different trig on PATH"

# A broken symlink is still something in the way.
mkdir "$work/t10"
ln -s /nonexistent-broken-target "$work/t10/trig"
check_alias_skipped "a broken trig symlink" "$work/t10" 'skipped the trig alias' PATH="$plain" TREERIG_VERSION="$version" TREERIG_BASE_URL="$base"
[ "$(readlink "$work/t10/trig")" = /nonexistent-broken-target ] || fail "install.sh repointed a broken trig symlink"

# A trig on PATH that already links to a treerig is "provided", not a clash.
mkdir "$work/provided"
ln -s "$work/t1/treerig" "$work/provided/trig"
check_alias_skipped "a trig on PATH that links to treerig" "$work/t11" 'already provided by' PATH="$work/provided:$plain" TREERIG_VERSION="$version" TREERIG_BASE_URL="$base"
if [ -e "$work/t11/trig" ] || [ -L "$work/t11/trig" ]; then
  fail "install.sh linked trig where one is already provided on PATH"
fi
# Homebrew's layout: two hops, relative ones, bin/trig -> ../Cellar/.../bin/trig -> treerig.
mkdir -p "$work/brewlike/bin" "$work/brewlike/Cellar/treerig/1.0/bin"
cp "$work/t1/treerig" "$work/brewlike/Cellar/treerig/1.0/bin/treerig"
ln -s treerig "$work/brewlike/Cellar/treerig/1.0/bin/trig"
ln -s ../Cellar/treerig/1.0/bin/trig "$work/brewlike/bin/trig"
check_alias_skipped "a trig on PATH that links to treerig in two hops" "$work/t11b" 'already provided by' PATH="$work/brewlike/bin:$plain" TREERIG_VERSION="$version" TREERIG_BASE_URL="$base"
[ ! -e "$work/t11b/trig" ] || fail "install.sh linked trig where one is already provided on PATH in two hops"
# A trig already in the install directory that reaches its treerig in two hops
# is in place, and left as it is. (The completion scenarios below check that its
# completions are written.)
mkdir -p "$work/t11c"
ln -s trig-hop "$work/t11c/trig"
ln -s treerig "$work/t11c/trig-hop"
check_alias_skipped "a trig in the directory that links to treerig in two hops" "$work/t11c" 'alias is already in place' PATH="$plain" TREERIG_VERSION="$version" TREERIG_BASE_URL="$base" TREERIG_NO_COMPLETIONS=1
[ "$(readlink "$work/t11c/trig")" = trig-hop ] || fail "install.sh replaced a trig that reaches treerig in two hops"

env PATH="$plain" TREERIG_NO_ALIAS=1 TREERIG_VERSION="$version" TREERIG_BASE_URL="$base" sh "$repo/install.sh" --dir "$work/t9" >"$work/install.log" 2>&1 ||
  { cat "$work/install.log" >&2; fail "install.sh with TREERIG_NO_ALIAS failed"; }
[ -x "$work/t9/treerig" ] || fail "TREERIG_NO_ALIAS=1 skipped treerig too"
if [ -e "$work/t9/trig" ] || [ -L "$work/t9/trig" ]; then
  fail "TREERIG_NO_ALIAS=1 still linked trig"
fi
echo "install.sh (trig alias): linked; left alone beside a file, a foreign symlink and a different trig on PATH; skipped on TREERIG_NO_ALIAS"

# --- Shell completion ----------------------------------------------------------

# The scripts the archive carries, to compare the installed copies with.
mkdir "$work/archive-files"
tar xzf "$dist/$archive" -C "$work/archive-files" completions ||
  fail "could not read completions/ from $archive"

# mkstubs <name> <shell>...: a directory of empty stand-ins for shells. install.sh
# asks `command -v`, so a stub is enough to make a shell "present", and the one
# it runs, `zsh -f` for zsh's default fpath, gets an empty one from the stub; the
# PATH for a scenario is such a directory in front of the plain tool directory.
mkstubs() {
  d="$work/path-$1"
  mkdir -p "$d"
  shift
  for t in "$@"; do
    printf '#!/bin/sh\nexit 0\n' >"$d/$t"
    chmod 755 "$d/$t"
  done
}
mkstubs all fish bash zsh
mkstubs fish fish
mkstubs bash bash
mkstubs zsh zsh

extra_arg=
# run_install <label> <dir> [env...]: install.sh into <dir> against the good
# server, with the scenario's environment (and $extra_arg, if set); it must
# succeed. The output stays in $work/install.log.
run_install() {
  label=$1
  dir=$2
  shift 2
  env TREERIG_VERSION="$version" TREERIG_BASE_URL="$base" "$@" sh "$repo/install.sh" --dir "$dir" ${extra_arg:+"$extra_arg"} >"$work/install.log" 2>&1 ||
    { cat "$work/install.log" >&2; fail "$label: install.sh failed"; }
  [ "$("$dir/treerig" --version)" = "$version" ] || fail "$label: treerig was not installed"
}

# must_have <label> <file>, must_lack <label> <path>
must_have() { [ -f "$2" ] || { cat "$work/install.log" >&2; fail "$1: $2 was not written"; }; }
must_lack() { { [ ! -e "$2" ] && [ ! -L "$2" ]; } || { cat "$work/install.log" >&2; fail "$1: $2 should not exist"; }; }

# must_match <label> <installed> <archive-name>: the installed file is the archive's.
must_match() {
  cmp -s "$2" "$work/archive-files/completions/$3" || fail "$1: $2 is not completions/$3 from the archive"
}

# count_block <file>: how many start markers the file holds.
count_block() { grep -c '^# >>> treerig completion ' "$1" || true; }

# A machine with treerig's own package installed has system-wide completions
# that install.sh rightly leaves alone, which would change what these scenarios
# see; they only run where there are none.
system_wide=
for f in /usr/share/bash-completion/completions/treerig /usr/share/zsh/site-functions/_treerig /usr/share/zsh/vendor-completions/_treerig /usr/share/fish/vendor_completions.d/treerig.fish; do
  [ ! -e "$f" ] || system_wide=$f
done

if [ -n "$system_wide" ]; then
  [ -z "${CI:-}" ] || fail "install.sh (completion): $system_wide is installed system-wide, and CI does not skip these scenarios"
  echo "install.sh (completion): skipped, $system_wide is installed system-wide on this machine"
elif [ "$(id -u)" = 0 ]; then
  [ -z "${CI:-}" ] || fail "install.sh (completion): CI runs as root, and does not skip the write scenarios"

  # As root install.sh declines (HOME may be someone else's), so the scenarios
  # below, which need it to write, cannot run. Check that it says so.
  h="$work/h-root"
  run_install "completion as root" "$work/c0" HOME="$h" SHELL=/bin/zsh PATH="$work/path-all:$plain"
  grep -q 'running as root' "$work/install.log" || { cat "$work/install.log" >&2; fail "install.sh did not say it skipped completion as root"; }
  grep -qF 'https://github.com/sofired/treerig/blob/main/docs/install.md#shell-completion' "$work/install.log" || fail "the root message does not link the guide"
  must_lack "completion as root" "$h"
  echo "install.sh (completion): skipped as root, as designed; the write scenarios need a non-root user"
else
  # All three shells, zsh as the login shell, the alias made: every file, and
  # the zsh block once across two runs.
  h="$work/h1"
  run_install "completion, all shells" "$work/c1" HOME="$h" SHELL=/bin/zsh PATH="$work/path-all:$plain"
  for c in treerig trig; do
    must_have "all shells" "$h/.config/fish/completions/$c.fish"
    must_match "all shells" "$h/.config/fish/completions/$c.fish" "$c.fish"
    must_have "all shells" "$h/.local/share/bash-completion/completions/$c"
    must_match "all shells" "$h/.local/share/bash-completion/completions/$c" "$c.bash"
    must_have "all shells" "$h/.local/share/zsh/site-functions/_$c"
    must_match "all shells" "$h/.local/share/zsh/site-functions/_$c" "_$c"
  done
  grep -q 'Open a new terminal to use completion' "$work/install.log" || fail "all shells: no 'Open a new terminal' line"
  grep -q '^To undo: https://github.com/sofired/treerig/blob/main/docs/install.md#undoing-the-completion-setup$' "$work/install.log" || fail "all shells: no 'To undo' line"
  grep -q "zsh: wrote _treerig, _trig to $h/.local/share/zsh/site-functions" "$work/install.log" || fail "all shells: the report does not say what was written"
  grep -q "$h/.zshrc" "$work/install.log" || fail "all shells: the report does not name .zshrc"
  [ "$(count_block "$h/.zshrc")" = 1 ] || fail "all shells: .zshrc does not hold one block"
  grep -qxF "fpath=('$h/.local/share/zsh/site-functions' \$fpath)" "$h/.zshrc" || fail "all shells: .zshrc lacks the fpath line"
  grep -qF 'compdef _trig trig' "$h/.zshrc" || fail "all shells: the block does not cover trig"
  cp "$h/.zshrc" "$work/zshrc.first"
  run_install "completion, second run" "$work/c1" HOME="$h" SHELL=/bin/zsh PATH="$work/path-all:$plain"
  [ "$(count_block "$h/.zshrc")" = 1 ] || fail "a second run duplicated the .zshrc block"
  cmp -s "$h/.zshrc" "$work/zshrc.first" || fail "a second run changed .zshrc"
  must_match "second run" "$h/.config/fish/completions/treerig.fish" treerig.fish
  echo "install.sh (completion): fish, bash and zsh files written for treerig and trig; the zsh block added once across two runs"

  # A shell that is not on PATH gets no files and no directories.
  h="$work/h2"
  run_install "completion, bash only" "$work/c2" HOME="$h" SHELL=/bin/bash PATH="$work/path-bash:$plain"
  must_have "bash only" "$h/.local/share/bash-completion/completions/treerig"
  must_lack "bash only" "$h/.config"
  must_lack "bash only" "$h/.local/share/zsh"
  must_lack "bash only" "$h/.zshrc"
  h="$work/h3"
  run_install "completion, fish only" "$work/c3" HOME="$h" SHELL=/usr/bin/fish PATH="$work/path-fish:$plain"
  must_have "fish only" "$h/.config/fish/completions/treerig.fish"
  must_lack "fish only" "$h/.local/share"
  h="$work/h4"
  run_install "completion, zsh only, login shell bash" "$work/c4" HOME="$h" SHELL=/bin/bash PATH="$work/path-zsh:$plain"
  must_have "zsh only" "$h/.local/share/zsh/site-functions/_treerig"
  must_lack "zsh, login shell bash" "$h/.zshrc"
  grep -q 'to use them in zsh, add the fpath lines from https://github.com/sofired/treerig/blob/main/docs/install.md#shell-completion' "$work/install.log" || fail "zsh, login shell bash: the report does not say how to use the zsh files"
  must_lack "zsh only" "$h/.config"
  h="$work/h5"
  run_install "completion, no shell on PATH" "$work/c5" HOME="$h" SHELL=/bin/zsh PATH="$plain"
  must_lack "no shells" "$h"
  grep -q 'No bash, zsh or fish' "$work/install.log" || fail "no shells: install.sh did not say so"
  echo "install.sh (completion): only the shells on PATH get files; .zshrc only for a zsh login shell"

  # XDG_DATA_HOME and XDG_CONFIG_HOME are honored.
  h="$work/h6"
  run_install "completion, XDG dirs" "$work/c6" HOME="$h" XDG_DATA_HOME="$work/xdg-data" XDG_CONFIG_HOME="$work/xdg-config" SHELL=/bin/sh PATH="$work/path-all:$plain"
  must_have "XDG" "$work/xdg-config/fish/completions/treerig.fish"
  must_have "XDG" "$work/xdg-data/bash-completion/completions/treerig"
  must_have "XDG" "$work/xdg-data/zsh/site-functions/_treerig"
  must_lack "XDG" "$h/.config"

  # A symlinked .zshrc is not edited; the lines to add are printed.
  h="$work/h7"
  mkdir -p "$h" "$work/dotfiles"
  printf '# managed elsewhere\n' >"$work/dotfiles/zshrc"
  ln -s "$work/dotfiles/zshrc" "$h/.zshrc"
  run_install "completion, symlinked .zshrc" "$work/c7" HOME="$h" SHELL=/bin/zsh PATH="$work/path-zsh:$plain"
  [ "$(cat "$work/dotfiles/zshrc")" = '# managed elsewhere' ] || fail "install.sh edited a symlinked .zshrc"
  [ -L "$h/.zshrc" ] || fail "install.sh replaced a symlinked .zshrc"
  grep -q 'did not edit' "$work/install.log" || { cat "$work/install.log" >&2; fail "no message about the symlinked .zshrc"; }
  grep -qxF "fpath=('$h/.local/share/zsh/site-functions' \$fpath)" "$work/install.log" || fail "the symlinked .zshrc case did not print the lines to add, unindented"
  grep -qx 'fi' "$work/install.log" || fail "the symlinked .zshrc case did not print the whole block"
  if grep -q 'Open a new terminal' "$work/install.log"; then fail "the symlinked .zshrc case says to open a new terminal"; fi
  grep -q 'After adding those lines' "$work/install.log" || fail "the symlinked .zshrc case does not say what to do after adding the lines"
  must_have "symlinked .zshrc" "$h/.local/share/zsh/site-functions/_treerig"
  echo "install.sh (completion): a symlinked .zshrc is not edited"

  # An existing .zshrc without a final newline keeps its last line intact.
  h="$work/h8"
  mkdir -p "$h"
  printf 'export A=1' >"$h/.zshrc"
  run_install "completion, .zshrc without newline" "$work/c8" HOME="$h" SHELL=/bin/zsh PATH="$work/path-zsh:$plain"
  [ "$(sed -n 1p "$h/.zshrc")" = 'export A=1' ] || fail "the block joined the last line of .zshrc"
  [ "$(count_block "$h/.zshrc")" = 1 ] || fail ".zshrc does not hold one block"

  # A begin marker with no end marker is damage: reported, not edited, and not
  # called "already has the block".
  h="$work/h8b"
  mkdir -p "$h"
  # shellcheck disable=SC2016 # zsh text, nothing to expand
  printf 'export A=1\n# >>> treerig completion (added by install.sh; delete this block to undo) >>>\nfpath=(x $fpath)\n' >"$h/.zshrc"
  cp "$h/.zshrc" "$work/zshrc.damaged"
  run_install "completion, unfinished block" "$work/c8b" HOME="$h" SHELL=/bin/zsh PATH="$work/path-zsh:$plain"
  cmp -s "$h/.zshrc" "$work/zshrc.damaged" || fail "install.sh edited a .zshrc with an unfinished block"
  grep -q 'unfinished treerig block' "$work/install.log" || { cat "$work/install.log" >&2; fail "no message about the unfinished block"; }
  if grep -q 'already has the treerig completion block' "$work/install.log"; then fail "an unfinished block was called complete"; fi
  if grep -q 'Open a new terminal' "$work/install.log"; then fail "an unfinished block still says to open a new terminal"; fi

  # Directory modes: with a group-writable umask the directories it creates are
  # still 755, and zsh's compaudit finds nothing wrong with them.
  h="$work/h8c"
  (umask 002 && run_install "completion, umask 002" "$work/c8c" HOME="$h" SHELL=/bin/sh PATH="$work/path-all:$plain")
  bad=$(find "$h" -type d -perm -020)
  [ -z "$bad" ] || fail "install.sh made group-writable directories under umask 002: $bad"
  bad=$(find "$h" -type f -perm -020)
  [ -z "$bad" ] || fail "install.sh made group-writable files under umask 002: $bad"
  if command -v zsh >/dev/null 2>&1; then
    mkdir -p "$work/zdot-audit"
    audit=$(env HOME="$h" ZDOTDIR="$work/zdot-audit" zsh -d -f -c "fpath=('$h/.local/share/zsh/site-functions' \$fpath); autoload -U compaudit; compaudit" 2>&1 </dev/null) || true
    if printf '%s\n' "$audit" | grep -qF "$h"; then fail "compaudit flags our directories under umask 002: $audit"; fi
  fi
  echo "install.sh (completion): an unfinished .zshrc block is reported, not edited; directories are 755 under umask 002"

  # A relative HOME or ZDOTDIR skips completion with a message.
  run_install "completion, relative HOME" "$work/c8d" HOME=relative-home SHELL=/bin/zsh PATH="$work/path-all:$plain"
  grep -q 'HOME is not set to an absolute path' "$work/install.log" || fail "a relative HOME was not reported"
  must_lack "relative HOME" "$PWD/relative-home"
  h="$work/h8e"
  run_install "completion, relative ZDOTDIR" "$work/c8e" HOME="$h" ZDOTDIR=zdot SHELL=/bin/zsh PATH="$work/path-all:$plain"
  grep -q 'ZDOTDIR is not an absolute path' "$work/install.log" || fail "a relative ZDOTDIR was not reported"
  must_lack "relative ZDOTDIR" "$h"

  # The archive's own faults: a script that is a symlink is skipped alone, and a
  # command whose scripts are missing is skipped without the other.
  for odd in 9.9.9 9.9.8; do
    mkdir -p "$work/good/v$odd" "$work/odd-$odd"
    cp -R "$work/archive-files/completions" "$work/odd-$odd/completions"
    tar xzf "$dist/$archive" -C "$work/odd-$odd" treerig
  done
  rm "$work/odd-9.9.9/completions/_trig"
  ln -s _treerig "$work/odd-9.9.9/completions/_trig"
  rm "$work/odd-9.9.8/completions/trig.fish"
  for odd in 9.9.9 9.9.8; do
    odd_archive="treerig_${odd}_${os}_${arch}.tar.gz"
    tar czf "$work/good/v$odd/$odd_archive" -C "$work/odd-$odd" treerig completions
    printf '%s  %s\n' "$(sha256_stdin <"$work/good/v$odd/$odd_archive")" "$odd_archive" >"$work/good/v$odd/checksums.txt"
  done
  h="$work/h8f"
  run_install "completion, symlink in the archive" "$work/c8f" HOME="$h" SHELL=/bin/sh PATH="$work/path-all:$plain" TREERIG_VERSION=9.9.9
  must_have "symlink in the archive" "$h/.local/share/zsh/site-functions/_treerig"
  must_lack "symlink in the archive" "$h/.local/share/zsh/site-functions/_trig"
  must_have "symlink in the archive" "$h/.config/fish/completions/trig.fish"
  grep -q 'not a regular file' "$work/install.log" || fail "a symlinked script in the archive was not reported"
  h="$work/h8g"
  run_install "completion, a command's script missing" "$work/c8g" HOME="$h" SHELL=/bin/sh PATH="$work/path-all:$plain" TREERIG_VERSION=9.9.8
  must_have "missing trig.fish" "$h/.config/fish/completions/treerig.fish"
  must_lack "missing trig.fish" "$h/.config/fish/completions/trig.fish"
  must_lack "missing trig.fish" "$h/.local/share/zsh/site-functions/_trig"
  grep -q 'could not extract the completion scripts for trig from the archive (an older release may not include them)' "$work/install.log" || fail "a missing script was not reported"
  echo "install.sh (completion): a symlink or a missing script in the archive skips only that script or command"

  # A system-wide copy (Homebrew's prefix here, laid out as Homebrew links a
  # formula's completions: bash under etc/bash_completion.d) is kept: no
  # user-level copy for that command and shell, which would shadow it. The zsh
  # block still goes in.
  mkdir -p "$work/brewprefix/share/zsh/site-functions" "$work/brewprefix/share/fish/vendor_completions.d" "$work/brewprefix/etc/bash_completion.d" "$work/path-brew"
  : >"$work/brewprefix/share/zsh/site-functions/_treerig"
  : >"$work/brewprefix/share/fish/vendor_completions.d/treerig.fish"
  : >"$work/brewprefix/etc/bash_completion.d/treerig"
  printf '#!/bin/sh\necho "%s"\n' "$work/brewprefix" >"$work/path-brew/brew"
  # A zsh whose default fpath holds Homebrew's directory, so its copy counts.
  printf '#!/bin/sh\necho "%s"\n' "$work/brewprefix/share/zsh/site-functions" >"$work/path-brew/zsh"
  chmod 755 "$work/path-brew/brew" "$work/path-brew/zsh"
  h="$work/h8h"
  run_install "completion, system-wide copy" "$work/c8h" HOME="$h" SHELL=/bin/zsh PATH="$work/path-brew:$work/path-all:$plain"
  must_lack "system-wide copy" "$h/.config/fish/completions/treerig.fish"
  must_lack "system-wide copy" "$h/.local/share/bash-completion/completions/treerig"
  must_lack "system-wide copy" "$h/.local/share/zsh/site-functions/_treerig"
  must_have "system-wide copy, trig" "$h/.config/fish/completions/trig.fish"
  grep -q "kept the system-wide $work/brewprefix/share/zsh/site-functions/_treerig" "$work/install.log" || fail "the kept system-wide copy was not reported"
  grep -qF 'compdef _treerig treerig' "$h/.zshrc" || fail "the zsh block is missing when the system-wide copy was kept"
  # The same Homebrew copy with a zsh whose fpath lacks its directory, as on
  # Apple silicon with macOS's own zsh: zsh would never load it, so the user-level
  # copy is written. bash and fish still keep theirs.
  mkdir -p "$work/path-brew-nofpath"
  cp "$work/path-brew/brew" "$work/path-brew-nofpath/brew"
  h="$work/h8i"
  run_install "completion, Homebrew zsh copy off fpath" "$work/c8i" HOME="$h" SHELL=/bin/zsh PATH="$work/path-brew-nofpath:$work/path-all:$plain"
  must_match "Homebrew zsh copy off fpath" "$h/.local/share/zsh/site-functions/_treerig" _treerig
  must_lack "Homebrew zsh copy off fpath" "$h/.config/fish/completions/treerig.fish"
  must_lack "Homebrew zsh copy off fpath" "$h/.local/share/bash-completion/completions/treerig"
  # A zsh copy in share/zsh/vendor-completions, where the .deb puts it, counts
  # too when that directory is on zsh's fpath, as on Debian and Ubuntu.
  mkdir -p "$work/vcprefix/share/zsh/vendor-completions" "$work/path-vc"
  : >"$work/vcprefix/share/zsh/vendor-completions/_treerig"
  printf '#!/bin/sh\necho "%s"\n' "$work/vcprefix" >"$work/path-vc/brew"
  printf '#!/bin/sh\necho "%s"\n' "$work/vcprefix/share/zsh/vendor-completions" >"$work/path-vc/zsh"
  chmod 755 "$work/path-vc/brew" "$work/path-vc/zsh"
  h="$work/h8j"
  run_install "completion, vendor-completions copy" "$work/c8j" HOME="$h" SHELL=/bin/zsh PATH="$work/path-vc:$work/path-all:$plain"
  must_lack "vendor-completions copy" "$h/.local/share/zsh/site-functions/_treerig"
  must_have "vendor-completions copy, trig" "$h/.local/share/zsh/site-functions/_trig"
  echo "install.sh (completion): a system-wide copy is kept in place of a user-level one; for zsh only when its directory (site-functions or vendor-completions) is on zsh's fpath"

  # Opt-outs write nothing, not even a directory.
  h="$work/h9"
  extra_arg=--no-completions
  run_install "--no-completions" "$work/c9" HOME="$h" SHELL=/bin/zsh PATH="$work/path-all:$plain"
  must_lack "--no-completions" "$h"
  run_install "--no-completions beats TREERIG_NO_COMPLETIONS=0" "$work/c9" HOME="$h" SHELL=/bin/zsh TREERIG_NO_COMPLETIONS=0 PATH="$work/path-all:$plain"
  must_lack "--no-completions with =0" "$h"
  extra_arg=
  run_install "TREERIG_NO_COMPLETIONS=1" "$work/c9" HOME="$h" SHELL=/bin/zsh TREERIG_NO_COMPLETIONS=1 PATH="$work/path-all:$plain"
  must_lack "TREERIG_NO_COMPLETIONS=1" "$h"
  run_install "TREERIG_NO_COMPLETIONS=0" "$work/c9" HOME="$h" SHELL=/bin/zsh TREERIG_NO_COMPLETIONS=0 PATH="$work/path-all:$plain"
  must_have "TREERIG_NO_COMPLETIONS=0" "$h/.config/fish/completions/treerig.fish"
  echo "install.sh (completion): --no-completions and TREERIG_NO_COMPLETIONS=1 write nothing; 0 does not opt out"

  # trig files follow the alias: none when it was skipped for a different trig
  # or by TREERIG_NO_ALIAS; present when it is provided elsewhere by a link to
  # treerig.
  h="$work/h10"
  run_install "completion, different trig" "$work/c10" HOME="$h" SHELL=/bin/zsh PATH="$work/otherbin:$work/path-all:$plain"
  must_have "different trig" "$h/.config/fish/completions/treerig.fish"
  must_lack "different trig" "$work/c10/trig"
  must_lack "different trig" "$h/.config/fish/completions/trig.fish"
  must_lack "different trig" "$h/.local/share/bash-completion/completions/trig"
  must_lack "different trig" "$h/.local/share/zsh/site-functions/_trig"
  if grep -q '_trig' "$h/.zshrc"; then fail "different trig: the .zshrc block covers trig"; fi
  h="$work/h11"
  run_install "completion, TREERIG_NO_ALIAS" "$work/c11" HOME="$h" SHELL=/bin/zsh TREERIG_NO_ALIAS=1 PATH="$work/path-all:$plain"
  must_have "TREERIG_NO_ALIAS" "$h/.local/share/zsh/site-functions/_treerig"
  must_lack "TREERIG_NO_ALIAS" "$h/.config/fish/completions/trig.fish"
  must_lack "TREERIG_NO_ALIAS" "$h/.local/share/zsh/site-functions/_trig"
  h="$work/h12"
  run_install "completion, trig provided elsewhere" "$work/c12" HOME="$h" SHELL=/bin/sh PATH="$work/provided:$work/path-all:$plain"
  grep -q 'already provided by' "$work/install.log" || fail "provided trig: the alias was not reported as provided"
  must_have "provided trig" "$h/.config/fish/completions/trig.fish"
  must_have "provided trig" "$h/.local/share/zsh/site-functions/_trig"
  # A trig already in the install directory, reaching its treerig in two hops,
  # is in place: its completions are written, the archive's own.
  mkdir -p "$work/c12b"
  ln -s trig-hop "$work/c12b/trig"
  ln -s treerig "$work/c12b/trig-hop"
  h="$work/h12b"
  run_install "completion, trig in place in two hops" "$work/c12b" HOME="$h" SHELL=/bin/zsh PATH="$work/path-all:$plain"
  grep -q 'alias is already in place' "$work/install.log" || fail "trig in two hops: the alias was not reported as in place"
  [ "$(readlink "$work/c12b/trig")" = trig-hop ] || fail "trig in two hops: install.sh replaced the link"
  must_match "trig in two hops" "$h/.config/fish/completions/trig.fish" trig.fish
  must_match "trig in two hops" "$h/.local/share/bash-completion/completions/trig" trig.bash
  must_match "trig in two hops" "$h/.local/share/zsh/site-functions/_trig" _trig
  grep -qF 'compdef _trig trig' "$h/.zshrc" || fail "trig in two hops: the zsh block does not cover trig"
  echo "install.sh (completion): trig files only with the alias in place (not beside a different trig, not under TREERIG_NO_ALIAS)"

  # A HOME with a space in it.
  h="$work/h space"
  run_install "completion, HOME with a space" "$work/c13" HOME="$h" SHELL=/bin/zsh PATH="$work/path-all:$plain"
  must_have "HOME with a space" "$h/.local/share/zsh/site-functions/_treerig"
  must_have "HOME with a space" "$h/.config/fish/completions/trig.fish"
  grep -qxF "fpath=('$h/.local/share/zsh/site-functions' \$fpath)" "$h/.zshrc" ||
    { cat "$h/.zshrc" >&2; fail "HOME with a space: the fpath line is not safely quoted"; }
  echo "install.sh (completion): a HOME with a space works"

  # With a real zsh on this machine: a new interactive shell registers the
  # completions whether or not the .zshrc ran compinit first, and the block does
  # not run compinit again when it had.
  if command -v zsh >/dev/null 2>&1; then
    # A .zshenv for these test shells only: it drops the system fpath
    # directories compaudit calls insecure (on GitHub's runner images some
    # under /usr/local/share are group- or world-writable), since compinit with
    # no terminal aborts on them instead of asking, which would hide what this
    # checks. Our own directories are not exempt: one compaudit flags is dropped
    # too, and the check below then fails.
    # shellcheck disable=SC2016 # zsh expands these
    zenv='autoload -Uz compaudit
_tr_bad=( $(compaudit 2>/dev/null) )
_tr_keep=()
for _tr_d in $fpath; do
  (( ${_tr_bad[(Ie)$_tr_d]} || ${_tr_bad[(Ie)${_tr_d:h}]} )) || _tr_keep+=( $_tr_d )
done
fpath=( $_tr_keep )
unset _tr_bad _tr_keep _tr_d'
    printf '%s\n' "$zenv" >"$h/.zshenv"
    # (a) no compinit earlier in the .zshrc
    # shellcheck disable=SC2016 # zsh expands these
    got=$(env HOME="$h" ZDOTDIR="$h" TERM=dumb zsh -d -i -c 'print -r -- "$_comps[treerig] $_comps[trig]"' 2>"$work/zsh.err" </dev/null) || true
    [ "$got" = "_treerig _trig" ] || { cat "$work/zsh.err" >&2; fail "zsh: with no earlier compinit, got '$got', expected '_treerig _trig'"; }
    # (b) compinit already ran. Afterwards compinit is replaced by a counter, so
    # a second call from the block would show as N=1.
    zd="$work/zdot b"
    mkdir -p "$zd"
    printf '%s\n' "$zenv" >"$zd/.zshenv"
    printf 'autoload -Uz compinit && compinit\ntypeset -gi N=0\ncompinit() { (( N++ )) }\n' >"$zd/.zshrc"
    sed -n '/^# >>> treerig/,/^# <<< treerig/p' "$h/.zshrc" >>"$zd/.zshrc"
    # shellcheck disable=SC2016 # zsh expands these
    got=$(env HOME="$h" ZDOTDIR="$zd" TERM=dumb zsh -d -i -c 'print -r -- "$_comps[treerig] $_comps[trig] $N"' 2>"$work/zsh.err" </dev/null) || true
    [ "$got" = "_treerig _trig 0" ] || { cat "$work/zsh.err" >&2; fail "zsh: with an earlier compinit, got '$got', expected '_treerig _trig 0'"; }
    echo "install.sh (completion): a real zsh registers the completions with and without an earlier compinit"
  elif [ -n "${CI:-}" ]; then
    fail "install.sh (completion): zsh is not on this machine, and CI does not skip the real-zsh check"
  else
    echo "install.sh (completion): real-zsh load check skipped, no zsh on this machine"
  fi
fi
stop_server

serve "$work/bad"
if TREERIG_VERSION="$version" TREERIG_BASE_URL="$base" sh "$repo/install.sh" --dir "$work/t5" >"$work/install.log" 2>&1; then
  fail "install.sh installed an archive whose checksum does not match"
fi
grep -q 'checksum mismatch' "$work/install.log" || { cat "$work/install.log" >&2; fail "no checksum mismatch message"; }
[ ! -e "$work/t5/treerig" ] || fail "a mismatched archive left a binary behind"
echo "install.sh (tampered archive): refused"
stop_server
