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
# anything) when another `trig` is in the way, and skips it when asked.
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
-rw-r--r-- /usr/share/zsh/site-functions/_treerig
-rw-r--r-- /usr/share/zsh/site-functions/_trig"
  got_deb=$(dpkg-deb -c "$deb" | awk '$1 !~ /^d/ { sub(/^\./, "", $6); print $1, $6 }' | LC_ALL=C sort -k2)
  [ "$got_deb" = "$want_deb" ] || {
    printf '%s\n' "$got_deb" >&2
    fail "$(basename "$deb") does not hold exactly the expected files and modes"
  }
  # The alias is a relative symlink to the binary beside it.
  deb_listing=$(dpkg-deb -c "$deb") || fail "dpkg-deb could not list $(basename "$deb")"
  printf '%s\n' "$deb_listing" | grep -q ' \./usr/bin/trig -> treerig$' ||
    fail "$(basename "$deb") does not link /usr/bin/trig to treerig"
  echo "deb contents: ok (with the trig symlink)"
else
  not_checked "deb contents" dpkg-deb
fi

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
  rm -f "$work/rpm.cpio" "$work/rpm-treerig"
  package_binary_matches rpm "$rpm" "$rpm_sha"
  echo "rpm binary: ok (/usr/bin/treerig is the archive's treerig, /usr/bin/trig links to it)"
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
echo "apk contents: ok (with the trig symlink)"

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
for t in uname mktemp sed awk head tar gzip cp chmod mv rm mkdir cat tr dirname readlink ln wget curl shasum sha256sum env sh; do
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

serve "$work/good"
check_install "latest, curl or wget" "$work/t1" PATH="$plain" TREERIG_LATEST_URL="$base/latest/checksums.txt" TREERIG_BASE_URL="$base"
check_install "pinned version" "$work/t2" PATH="$plain" TREERIG_VERSION="v$version" TREERIG_BASE_URL="$base"

# Without curl, and then without sha256sum: a directory holding only the tools
# the script needs.
tools="uname mktemp sed awk head tar gzip cp chmod mv rm mkdir cat tr dirname readlink ln wget shasum sha256sum env sh"
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

env PATH="$plain" TREERIG_NO_ALIAS=1 TREERIG_VERSION="$version" TREERIG_BASE_URL="$base" sh "$repo/install.sh" --dir "$work/t9" >"$work/install.log" 2>&1 ||
  { cat "$work/install.log" >&2; fail "install.sh with TREERIG_NO_ALIAS failed"; }
[ -x "$work/t9/treerig" ] || fail "TREERIG_NO_ALIAS=1 skipped treerig too"
if [ -e "$work/t9/trig" ] || [ -L "$work/t9/trig" ]; then
  fail "TREERIG_NO_ALIAS=1 still linked trig"
fi
echo "install.sh (trig alias): linked; left alone beside a file, a foreign symlink and a different trig on PATH; skipped on TREERIG_NO_ALIAS"
stop_server

serve "$work/bad"
if TREERIG_VERSION="$version" TREERIG_BASE_URL="$base" sh "$repo/install.sh" --dir "$work/t5" >"$work/install.log" 2>&1; then
  fail "install.sh installed an archive whose checksum does not match"
fi
grep -q 'checksum mismatch' "$work/install.log" || { cat "$work/install.log" >&2; fail "no checksum mismatch message"; }
[ ! -e "$work/t5/treerig" ] || fail "a mismatched archive left a binary behind"
echo "install.sh (tampered archive): refused"
stop_server
