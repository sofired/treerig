#!/bin/sh
# The post-release smoke test (#147; release-smoke.yml calls it, and
# docs/design/teardown.md, "#147", is the design): checks what a published
# release is, and what its own binary does at the teardown pin and `worktree
# remove`'s gate (#146), on the machine it runs on.
#
#   release-smoke.sh fetch  --tag vX.Y.Z --dir DIR [--base-url URL]
#                           [--no-signature-check] [--no-packages]
#   release-smoke.sh legs   --binary BIN --version X.Y.Z
#                           [--pin release|file] [--dev-build] [--keep]
#                           [--ci-snapshot]
#   release-smoke.sh latest --tag vX.Y.Z --binary BIN --dir DIR [--base-url URL]
#   release-smoke.sh help
#
# fetch   Downloads checksums.txt, its Sigstore bundle and this platform's
#         archive from <base>/download/<tag>/, confirms that checksums.txt was
#         signed by release.yml at that tag (cosign verify-blob, the identity and
#         issuer in docs/design/release.md), that it lists every asset the
#         contract names, and that the archive matches its line; extracts
#         DIR/bin/treerig. On Linux it also downloads this architecture's .deb,
#         .rpm and .apk and confirms that each one's /usr/bin/treerig is the
#         archive's (packaging/verify-snapshot.sh does the same for a snapshot;
#         it is tied to dist/, so this is the small equivalent). Needs curl, tar,
#         cosign, and on Linux dpkg-deb, rpm2cpio and cpio.
# legs    Runs the three legs against BIN, each in its own scratch TREERIG_HOME
#         and TREERIG_CONTROL_HOME, in a scratch git checkout with a minimal
#         treerig.toml, with no terminal:
#           pin      `teardown pin`, `teardown status`, then `worktree remove`
#                    must say both pinned-binary lines;
#           no-pin   `worktree remove` must not meet the gate, and `teardown
#                    status --json` must say removal runs this build;
#           corrupt  a pinned binary that was overwritten must be refused.
#         Every `worktree remove` stops after the gate: the checkout is not a
#         registered worktree, so it ends at "not registered; nothing to
#         remove" (or at a Docker failure) and removes nothing.
#         --pin file pins BIN itself with `--file --sha256` instead of
#         downloading it: for a binary no release has, such as a snapshot.
#         --dev-build says BIN is a development build (a snapshot's version
#         reads as one), so the no-pin leg must see the gate's refusal instead.
#         --keep leaves the scratch directory for inspection.
# latest  At a stable tag that is the latest release, confirms that
#         <base>/latest/download/checksums.txt is the tag's, and that
#         `teardown pin --latest` pins that release. A prerelease, or a tag that
#         is not (any longer) the latest, is skipped with a notice.
#
# --base-url (default https://github.com/sofired/treerig/releases),
# --no-signature-check, --no-packages, --pin file and --dev-build exist to try
# this script before a release exists: against a local server, or a GoReleaser
# snapshot. They are for local testing only. When GITHUB_ACTIONS is true the
# script refuses them, except that --ci-snapshot allows --pin file and
# --dev-build, for go.yml's release-snapshot job, which runs the legs against
# the snapshot on every pull request. release-smoke.yml uses none of them
# (internal/style checks), so the release run verifies the real signature, the
# real URLs and the real pin. `latest` cannot point `teardown pin --latest` at a
# local server, so it skips that half under --base-url.
#
# The package-reading code (the deb, rpm and apk /usr/bin/treerig hashes) is
# the small equivalent of packaging/verify-snapshot.sh's, kept separate on
# purpose (that script is tied to dist/ and a snapshot's file names); a fix to
# one belongs in the other.
#
# Nothing here starts or stops the router, touches a registered worktree, or
# reads ~/.treerig: every treerig runs under the scratch homes, any TREERIG_*
# variable in the caller's environment is dropped, and the scratch directory is
# removed on exit.
set -eu
umask 077

here=$(cd "$(dirname "$0")" && pwd)
default_base=https://github.com/sofired/treerig/releases
# docs/design/release.md, "Who signed it".
identity_prefix=https://github.com/sofired/treerig/.github/workflows/release.yml@refs/tags/
issuer=https://token.actions.githubusercontent.com

# annotate <message>: a GitHub Actions error annotation, in Actions only.
annotate() {
  if [ -n "${GITHUB_ACTIONS:-}" ]; then printf '::error::%s\n' "$1" >&2; fi
}

die() {
  printf 'release-smoke: %s\n' "$*" >&2
  annotate "$*"
  exit 1
}

print_usage() {
  cat <<'USAGE'
usage: release-smoke.sh fetch  --tag vX.Y.Z --dir DIR [--base-url URL] [--no-signature-check] [--no-packages]
       release-smoke.sh legs   --binary BIN --version X.Y.Z [--pin release|file] [--dev-build] [--keep] [--ci-snapshot]
       release-smoke.sh latest --tag vX.Y.Z --binary BIN --dir DIR [--base-url URL]
       release-smoke.sh help
(the header of this script says what each does)
USAGE
}

# usage_error <message>: the message above the usage, on stderr, exit 2.
usage_error() {
  printf 'release-smoke: %s\n' "$*" >&2
  print_usage >&2
  exit 2
}

# need_value <option> [value...]: usage_error unless the option has a value.
need_value() {
  if [ $# -lt 2 ]; then usage_error "$1 needs a value"; fi
}

# local_only <option>: refuse a local-testing option in GitHub Actions.
local_only() {
  if [ "${GITHUB_ACTIONS:-}" = true ]; then die "$1 is for local testing only"; fi
}

# The scratch directories this run made, removed on exit. $keep=1 keeps $root.
work=''
root=''
keep=0
cleanup() {
  if [ -n "$work" ]; then rm -rf "$work"; fi
  if [ -n "$root" ] && [ "$keep" != 1 ]; then rm -rf "$root"; fi
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{ print $1 }'
  else
    shasum -a 256 "$1" | awk '{ print $1 }'
  fi
}

is_sha256() {
  printf '%s' "$1" | grep -Eq '^[0-9a-f]{64}$'
}

# validate_tag <tag>: the release workflow's own rule (validate-tag.sh).
validate_tag() {
  GITHUB_REF_NAME=$1 "$here/validate-tag.sh" || die "refusing the tag '$1'"
}

# curl_get <url> <curl options...>: https only (also across redirects), except
# for the local http server a local-testing --base-url names.
curl_get() {
  target_url=$1
  shift
  case $target_url in
    https://*) curl --proto '=https' --proto-redir '=https' "$@" "$target_url" ;;
    *) curl "$@" "$target_url" ;;
  esac
}

# download <url> <file>
download() {
  printf 'GET %s\n' "$1"
  curl_get "$1" -fsSL --retry 3 --retry-all-errors --retry-delay 5 --connect-timeout 20 --max-time 300 -o "$2" ||
    die "could not download $1 (is the release published, and its assets uploaded? A failed run of this job can be re-run.)"
}

# platform sets os, arch and rpm_arch (the rpm and apk spelling) for this machine.
platform() {
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) die "unsupported OS $(uname -s)" ;;
  esac
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 rpm_arch=x86_64 ;;
    aarch64 | arm64) arch=arm64 rpm_arch=aarch64 ;;
    *) die "unsupported architecture $(uname -m)" ;;
  esac
}

# versions <version> sets the version as each package format spells it
# (.goreleaser.yaml's nfpms file_name_template is {{ .ConventionalFileName }}):
#   deb_ver  the first '-' is '~' (0.1.0~rc.2): the name in checksums.txt, and
#            the file name here;
#   dl_ver   deb_ver with '~' as '.' (0.1.0.rc.2): GitHub rewrites a '~' in an
#            asset's name when it is uploaded, so this is the download URL's;
#   apk_ver  the first '-' is '_' (0.1.0_rc.2): the same in both places.
# A final release (no '-') has the same version everywhere. See
# docs/design/release.md, "The assets of release".
versions() {
  case $1 in
    *-*)
      deb_ver=${1%%-*}~${1#*-}
      apk_ver=${1%%-*}_${1#*-}
      ;;
    *)
      deb_ver=$1
      apk_ver=$1
      ;;
  esac
  dl_ver=$(printf '%s' "$deb_ver" | tr '~' '.')
}

# ---------------------------------------------------------------- fetch

# package_binary_sha <package file>: the sha256 of its /usr/bin/treerig. The
# tools' stderr goes to a file in $work, and the last of it is in the failure.
package_binary_sha() {
  rm -f "$work/pkg-treerig" "$work/pkg.cpio"
  : >"$work/pkg.err"
  case "$1" in
    *.deb)
      { dpkg-deb --fsys-tarfile "$1" | tar xOf - ./usr/bin/treerig >"$work/pkg-treerig"; } 2>"$work/pkg.err" || true
      ;;
    *.rpm)
      # Ubuntu's rpm2cpio (rpm 4.18) can exit 1, silently, after writing the
      # whole payload of a GoReleaser rpm, so its status is not the test: a cut
      # payload is caught by the missing file or the hash. The member is spelled
      # with or without a leading ./ or /, by version.
      rpm2cpio "$1" >"$work/pkg.cpio" 2>"$work/pkg.err" || true
      cpio -i --to-stdout ./usr/bin/treerig /usr/bin/treerig usr/bin/treerig <"$work/pkg.cpio" >"$work/pkg-treerig" 2>>"$work/pkg.err" || true
      ;;
    *.apk)
      # Concatenated gzip streams of tar, which tar reads through.
      tar xzOf "$1" usr/bin/treerig >"$work/pkg-treerig" 2>"$work/pkg.err" || true
      ;;
  esac
  if [ ! -s "$work/pkg-treerig" ]; then
    die "no /usr/bin/treerig in $(basename "$1"); the tools said: $(tail -n 5 "$work/pkg.err" | tr '\n' ' ')"
  fi
  sha256_of "$work/pkg-treerig"
}

cmd_fetch() {
  tag='' dir='' base=$default_base check_signature=1 check_packages=1
  while [ $# -gt 0 ]; do
    case $1 in
      --tag) need_value "$@"; tag=$2; shift 2 ;;
      --dir) need_value "$@"; dir=$2; shift 2 ;;
      --base-url) need_value "$@"; local_only --base-url; base=$2; shift 2 ;;
      --no-signature-check) local_only --no-signature-check; check_signature=0; shift ;;
      --no-packages) local_only --no-packages; check_packages=0; shift ;;
      -h | --help) print_usage; exit 0 ;;
      *) usage_error "unknown option '$1'" ;;
    esac
  done
  if [ -z "$tag" ] || [ -z "$dir" ]; then usage_error "fetch needs --tag and --dir"; fi
  validate_tag "$tag"
  version=${tag#v}
  versions "$version"
  platform
  command -v curl >/dev/null 2>&1 || die "curl is required"
  if [ "$check_signature" = 1 ]; then
    command -v cosign >/dev/null 2>&1 || die "cosign is required (sigstore/cosign-installer)"
    cosign version 2>&1 | sed -n 's/^GitVersion: */cosign /p'
  else
    printf 'release-smoke: WARNING: --no-signature-check; the signature of checksums.txt is NOT verified (local testing only)\n' >&2
  fi

  mkdir -p "$dir/dl" "$dir/bin"
  work=$(mktemp -d "${TMPDIR:-/tmp}/release-smoke-fetch.XXXXXX")
  url=$base/download/$tag
  archive=treerig_${version}_${os}_${arch}.tar.gz

  download "$url/checksums.txt" "$dir/dl/checksums.txt"
  download "$url/checksums.txt.sigstore.json" "$dir/dl/checksums.txt.sigstore.json"
  download "$url/$archive" "$dir/dl/$archive"

  if [ "$check_signature" = 1 ]; then
    cosign verify-blob --bundle "$dir/dl/checksums.txt.sigstore.json" \
      --certificate-identity "$identity_prefix$tag" \
      --certificate-oidc-issuer "$issuer" \
      "$dir/dl/checksums.txt" ||
      die "checksums.txt is not signed by $identity_prefix$tag (issuer $issuer); do not trust this release"
    echo "signature: ok (signed by release.yml at $tag)"
  fi

  # Every asset docs/design/release.md names for this version, as
  # checksums.txt spells it (a prerelease's deb and rpm carry a '~', its apk a '_').
  missing=''
  for name in \
    "treerig_${version}_linux_amd64.tar.gz" "treerig_${version}_linux_arm64.tar.gz" \
    "treerig_${version}_darwin_amd64.tar.gz" "treerig_${version}_darwin_arm64.tar.gz" \
    "treerig-${version}.tar.gz" \
    "treerig_${deb_ver}_amd64.deb" "treerig_${deb_ver}_arm64.deb" \
    "treerig-${deb_ver}-1.x86_64.rpm" "treerig-${deb_ver}-1.aarch64.rpm" \
    "treerig_${apk_ver}_x86_64.apk" "treerig_${apk_ver}_aarch64.apk"; do
    awk -v f="$name" '$2 == f { found = 1 } END { exit !found }' "$dir/dl/checksums.txt" || missing="$missing $name"
  done
  [ -z "$missing" ] || die "checksums.txt does not list:$missing"
  echo "checksums.txt: lists every asset of $tag"

  archive_sha=$(sha256_of "$dir/dl/$archive")
  listed=$(awk -v f="$archive" '$2 == f { print $1; exit }' "$dir/dl/checksums.txt")
  is_sha256 "$listed" || die "checksums.txt has no valid sha256 for $archive"
  [ "$archive_sha" = "$listed" ] ||
    die "$archive has sha256 $archive_sha, but checksums.txt lists $listed"
  echo "archive: ok ($archive matches checksums.txt)"

  tar xzf "$dir/dl/$archive" -C "$dir/bin" treerig || die "no treerig at the root of $archive"
  chmod 755 "$dir/bin/treerig"
  binary_sha=$(sha256_of "$dir/bin/treerig")

  if [ "$os" = linux ] && [ "$check_packages" = 1 ]; then
    for tool in dpkg-deb rpm2cpio cpio; do
      command -v "$tool" >/dev/null 2>&1 ||
        die "$tool is required to read the packages (apt-get install dpkg rpm2cpio cpio)"
    done
    # name: as checksums.txt and the local file spell it; url name: as the
    # download URL spells it.
    for pair in \
      "treerig_${deb_ver}_${arch}.deb treerig_${dl_ver}_${arch}.deb" \
      "treerig-${deb_ver}-1.${rpm_arch}.rpm treerig-${dl_ver}-1.${rpm_arch}.rpm" \
      "treerig_${apk_ver}_${rpm_arch}.apk treerig_${apk_ver}_${rpm_arch}.apk"; do
      # shellcheck disable=SC2086 # two words, by construction
      set -- $pair
      package=$1
      download "$url/$2" "$dir/dl/$package"
      listed=$(awk -v f="$package" '$2 == f { print $1; exit }' "$dir/dl/checksums.txt")
      [ "$(sha256_of "$dir/dl/$package")" = "$listed" ] ||
        die "$package does not match its line in checksums.txt"
      found=$(package_binary_sha "$dir/dl/$package")
      [ "$found" = "$binary_sha" ] ||
        die "/usr/bin/treerig in $package (sha256 $found) is not the treerig in $archive ($binary_sha)"
      echo "package: ok (/usr/bin/treerig in $package is the archive's treerig)"
    done
  fi
  echo "binary: $dir/bin/treerig sha256 $binary_sha"
}

# ----------------------------------------------------------------- legs

# fail_leg <message>: what failed, with the leg's name; the output of each
# command ("[leg] $ command", then its out| and err| lines) is above it in the log.
fail_leg() {
  printf 'release-smoke: FAILED (%s leg): %s\n' "$leg_name" "$*" >&2
  annotate "FAILED ($leg_name leg): $*"
  exit 1
}

# new_leg <name>: a scratch TREERIG_HOME, TREERIG_CONTROL_HOME and git checkout
# of its own, and the environment treerig runs in. The caller runs in a
# subshell, so none of this leaks into the next leg.
new_leg() {
  leg_name=$1
  leg=$root/$1
  mkdir -p "$leg/home" "$leg/control" "$leg/checkout"
  for name in $(env | sed -n 's/^\(TREERIG_[A-Za-z0-9_]*\)=.*/\1/p'); do unset "$name"; done
  TREERIG_HOME=$leg/home
  TREERIG_CONTROL_HOME=$leg/control
  export TREERIG_HOME TREERIG_CONTROL_HOME
  cd "$leg/checkout"
  git -c init.defaultBranch=main init -q .
  printf '[project]\nname = "smoke"\ndomain = "smoke.localhost"\n\n[compose]\nfile = "compose.yaml"\n' >treerig.toml
  printf 'services: {}\n' >compose.yaml
  git add treerig.toml compose.yaml
  git -c user.name=release-smoke -c user.email=release-smoke@invalid commit -q -m 'release smoke'
}

# run <label> <command...>: with no terminal (stdin is /dev/null, stdout and
# stderr are files); leaves $rc and $leg/<label>.{stdout,stderr}, and shows them.
run() {
  label=$1
  shift
  printf '\n[%s] $ %s\n' "$leg_name" "$*"
  rc=0
  "$@" >"$leg/$label.stdout" 2>"$leg/$label.stderr" </dev/null || rc=$?
  sed 's/^/    out| /' "$leg/$label.stdout"
  sed 's/^/    err| /' "$leg/$label.stderr"
  printf '    exit %s\n' "$rc"
}

# has <label> stdout|stderr <fixed text>
has() { grep -qF -e "$3" "$leg/$1.$2"; }

# expect_rc <want>; expect_nonzero.
expect_rc() { [ "$rc" = "$1" ] || fail_leg "exit $rc, want $1"; }
expect_nonzero() { [ "$rc" != 0 ] || fail_leg "exit 0, want a refusal or failure"; }

# pin_binary: the pin, from the release (the real cosign) or from BIN itself.
pin_binary() {
  case $pin_mode in
    release) run pin "$binary" teardown pin --release "$version" --yes ;;
    file) run pin "$binary" teardown pin --file "$binary" --sha256 "$digest" --release "$version" --yes ;;
  esac
  expect_rc 0
  [ -f "$TREERIG_CONTROL_HOME/teardown/$digest" ] ||
    fail_leg "the pin did not store the binary at $TREERIG_CONTROL_HOME/teardown/$digest"
}

# downstream_stop: `worktree remove` got past the gate and stopped for the only
# reason the smoke test accepts: this checkout is not a registered worktree
# (resolveRemoveTarget says so before Docker is asked anything). On macOS, where
# the runner has no Docker, "Docker is not available" is accepted too, in case
# that order changes.
downstream_stop() {
  expect_nonzero
  [ ! -s "$leg/remove.stdout" ] || fail_leg "worktree remove wrote to stdout"
  has remove stderr 'Refusing' && fail_leg "worktree remove was refused by the gate"
  if has remove stderr 'not registered; nothing to remove'; then :
  elif [ "$(uname -s)" = Darwin ] && has remove stderr 'Docker is not available'; then :
  else
    fail_leg "worktree remove did not end at 'not registered; nothing to remove'"
  fi
  [ -f "$leg/checkout/treerig.toml" ] || fail_leg "the checkout is gone"
}

leg_pin() {
  new_leg pin
  pin_binary
  run status "$binary" teardown status
  expect_rc 0
  has status stdout "release $version" || fail_leg "teardown status does not name release $version"
  has status stdout 'worktree remove: will run this binary' || fail_leg "teardown status does not say worktree remove will run the pinned binary"
  run remove "$binary" worktree remove
  parent="/teardown/$digest (release $version) to remove this worktree."
  grep -F 'Running the pinned teardown binary ' "$leg/remove.stderr" | grep -qF -e "$parent" ||
    fail_leg "the parent did not say 'Running the pinned teardown binary …$parent'"
  has remove stderr "Running as the pinned teardown binary (version $version)." ||
    fail_leg "the child did not say 'Running as the pinned teardown binary (version $version).'"
  # The parent's line comes first: the child only exists because of it.
  first=$(grep -nF 'Running the pinned teardown binary ' "$leg/remove.stderr" | head -n 1 | cut -d: -f1)
  second=$(grep -nF 'Running as the pinned teardown binary' "$leg/remove.stderr" | head -n 1 | cut -d: -f1)
  [ "$first" -lt "$second" ] || fail_leg "the pinned-binary lines are out of order"
  downstream_stop
  echo "leg pin: ok"
}

leg_nopin() {
  new_leg no-pin
  run remove "$binary" worktree remove
  has remove stderr 'Running the pinned teardown binary' && fail_leg "a pinned-binary line appeared with no pin"
  has remove stderr 'Running as the pinned teardown binary' && fail_leg "a pinned-binary line appeared with no pin"
  if [ "$dev_build" = 1 ]; then
    expect_rc 1
    has remove stderr 'Refusing to remove this worktree: this is a development build' ||
      fail_leg "a development build with no pin was not refused for being one"
    run status "$binary" teardown status --json
    expect_rc 1
    grep -Eq '"removal": *"refuse"' "$leg/status.stdout" || fail_leg 'teardown status --json does not say "removal": "refuse"'
  else
    downstream_stop
    run status "$binary" teardown status --json
    expect_rc 0
    grep -Eq '"removal": *"this-build"' "$leg/status.stdout" || fail_leg 'teardown status --json does not say "removal": "this-build"'
  fi
  echo "leg no-pin: ok"
}

leg_corrupt() {
  new_leg corrupt
  pin_binary
  stored=$TREERIG_CONTROL_HOME/teardown/$digest
  chmod u+w "$stored"
  printf 'overwritten by release-smoke\n' >"$stored"
  run remove "$binary" worktree remove
  expect_rc 1
  grep -Eq "has sha256 [0-9a-f]{64}, not the pinned $digest" "$leg/remove.stderr" ||
    fail_leg "the gate did not refuse with 'has sha256 …, not the pinned $digest'"
  has remove stderr 'Running as the pinned teardown binary' && fail_leg "the overwritten binary ran"
  [ -f "$leg/checkout/treerig.toml" ] || fail_leg "the checkout is gone"
  echo "leg corrupt: ok"
}

cmd_legs() {
  binary='' version='' pin_mode=release dev_build=0 ci_snapshot=0
  while [ $# -gt 0 ]; do
    case $1 in
      --binary) need_value "$@"; binary=$2; shift 2 ;;
      --version) need_value "$@"; version=$2; shift 2 ;;
      --pin) need_value "$@"; pin_mode=$2; shift 2 ;;
      --dev-build) dev_build=1; shift ;;
      --keep) keep=1; shift ;;
      --ci-snapshot) ci_snapshot=1; shift ;;
      -h | --help) print_usage; exit 0 ;;
      *) usage_error "unknown option '$1'" ;;
    esac
  done
  if [ -z "$binary" ] || [ -z "$version" ]; then usage_error "legs needs --binary and --version"; fi
  case $pin_mode in release | file) ;; *) usage_error "--pin must be release or file" ;; esac
  # The two options a snapshot needs are local-testing ones; the CI snapshot run
  # (go.yml) says so with --ci-snapshot. Nothing else is allowed in Actions.
  if [ "$ci_snapshot" != 1 ]; then
    if [ "$pin_mode" = file ]; then local_only "--pin file"; fi
    if [ "$dev_build" = 1 ]; then local_only --dev-build; fi
  fi
  # The release's version already passed validate-tag.sh in the workflow. Only
  # the characters matter here, since it is matched as text; a snapshot's
  # version (0.0.0-dev+<commit>) is not a valid tag and is allowed.
  case $version in
    '' | *[!A-Za-z0-9.+-]*) die "'$version' is not a version string" ;;
  esac
  if [ ! -f "$binary" ] || [ ! -x "$binary" ]; then die "$binary is not an executable file"; fi
  binary=$(cd "$(dirname "$binary")" && pwd)/$(basename "$binary")
  command -v git >/dev/null 2>&1 || die "git is required"
  if [ "$pin_mode" = release ]; then
    command -v cosign >/dev/null 2>&1 || die "cosign is required to pin a release (sigstore/cosign-installer)"
  fi
  reported=$("$binary" --version </dev/null) || die "$binary --version failed"
  [ "$reported" = "$version" ] || die "$binary reports version '$reported', not '$version'"
  digest=$(sha256_of "$binary")

  root=$(mktemp -d "${TMPDIR:-/tmp}/release-smoke.XXXXXX")
  printf 'scratch directory: %s\nbinary: %s (version %s, sha256 %s)\n' "$root" "$binary" "$version" "$digest"
  if [ "$dev_build" = 1 ]; then echo "mode: development build (the no-pin leg expects the gate's refusal)"; fi

  # A subshell each: the legs set TREERIG_HOME and the like for themselves and
  # leave the next one a clean environment.
  (leg_pin)
  (leg_nopin)
  (leg_corrupt)
  echo "release-smoke: all legs passed"
}

# --------------------------------------------------------------- latest

cmd_latest() {
  tag='' binary='' dir='' base=$default_base
  while [ $# -gt 0 ]; do
    case $1 in
      --tag) need_value "$@"; tag=$2; shift 2 ;;
      --binary) need_value "$@"; binary=$2; shift 2 ;;
      --dir) need_value "$@"; dir=$2; shift 2 ;;
      --base-url) need_value "$@"; local_only --base-url; base=$2; shift 2 ;;
      -h | --help) print_usage; exit 0 ;;
      *) usage_error "unknown option '$1'" ;;
    esac
  done
  if [ -z "$tag" ] || [ -z "$binary" ] || [ -z "$dir" ]; then usage_error "latest needs --tag, --binary and --dir"; fi
  validate_tag "$tag"
  version=${tag#v}
  case $version in
    *-*)
      echo "::notice::$tag is a prerelease, which is never \"latest\"; skipping the latest checks"
      return 0
      ;;
  esac
  # The latest release is where /releases/latest redirects. An empty answer is
  # a failure (an HTTP error, a rate limit, or no release marked latest), never
  # a reason to skip: only a different tag is.
  target=$(curl_get "$base/latest" -fsS --retry 3 --retry-all-errors --connect-timeout 20 --max-time 60 -o /dev/null -w '%{redirect_url}') ||
    die "could not resolve $base/latest"
  [ -n "$target" ] || die "$base/latest did not redirect to a release (is a release marked latest?)"
  latest=${target##*/}
  [ -n "$latest" ] || die "$base/latest redirected to $target, which names no release"
  if [ "$latest" != "$tag" ]; then
    echo "::notice::$tag is not the latest release (that is '$latest'); skipping the latest checks"
    return 0
  fi
  work=$(mktemp -d "${TMPDIR:-/tmp}/release-smoke-latest.XXXXXX")
  download "$base/latest/download/checksums.txt" "$work/checksums.txt"
  cmp -s "$work/checksums.txt" "$dir/dl/checksums.txt" ||
    die "$base/latest/download/checksums.txt is not the checksums.txt of $tag"
  echo "latest: ok (releases/latest/download/checksums.txt is $tag's)"

  if [ "$base" != "$default_base" ]; then
    echo "release-smoke: --base-url is set; teardown pin --latest always downloads from GitHub, so it is skipped"
    return 0
  fi
  command -v cosign >/dev/null 2>&1 || die "cosign is required to pin a release (sigstore/cosign-installer)"
  binary=$(cd "$(dirname "$binary")" && pwd)/$(basename "$binary")
  root=$(mktemp -d "${TMPDIR:-/tmp}/release-smoke.XXXXXX")
  (
    new_leg latest
    run pin "$binary" teardown pin --latest --yes
    expect_rc 0
    run status "$binary" teardown status
    expect_rc 0
    has status stdout "release $version" || fail_leg "teardown pin --latest did not pin release $version"
    echo "leg latest: ok"
  )
}

# EXIT cleans up; the signals only exit (the EXIT trap then runs), so a Ctrl-C
# or a runner cancel ends the script instead of carrying on after the cleanup.
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

if [ $# -lt 1 ]; then usage_error "no command given"; fi
sub=$1
shift
case $sub in
  fetch) cmd_fetch "$@" ;;
  legs) cmd_legs "$@" ;;
  latest) cmd_latest "$@" ;;
  help | -h | --help) print_usage ;;
  *) usage_error "unknown command '$sub'" ;;
esac
