#!/bin/sh
# Validates the release tag in GITHUB_REF_NAME (release.yml's first step): it
# has to be a version a published treerig reads as a release, so that a release
# can never be taken for a development build. The rule is the one
# internal/semver.IsPublishedRelease applies to brand.Version, and
# internal/semver's TestValidateTagAgreesWithIsPublishedRelease runs this script
# against that function, so the two cannot drift.
#
# The tag, with its leading v stripped, becomes brand.Version through
# GoReleaser's -ldflags -X, so `--version` matches what the tag says.
#
# usage: GITHUB_REF_NAME=v1.2.3 .github/scripts/validate-tag.sh
set -eu

tag="${GITHUB_REF_NAME:-}"

# A release tag is `v` plus the version: release.yml only fires on `v*`, and the
# download URLs (release-smoke.yml's dispatch input included) are built from it.
case "$tag" in
  v*) ;;
  *)
    echo "::error::tag '${tag}' must start with v, as in v0.1.0" >&2
    exit 1
    ;;
esac
version="${tag#v}"

# Goes straight into -ldflags -X; a tag name with a quote or space in it would
# otherwise break the build step's quoting. A bare `v` tag leaves nothing to
# print for `--version`.
case "$version" in
  '')
    echo "::error::tag '${tag}' has no version after its leading v" >&2
    exit 1
    ;;
  *[!A-Za-z0-9.+-]*)
    echo "::error::tag '${tag}' isn't a safe version string (only letters, digits, '.', '+', '-')" >&2
    exit 1
    ;;
esac

# Strict Semantic Versioning 2.0.0, without build metadata: no leading zeros in a
# numeric part or a numeric pre-release identifier, no empty identifier.
ident='(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
if ! printf '%s' "$version" | grep -Eq "^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-${ident}(\.${ident})*)?\$"; then
  echo "::error::tag '${tag}' isn't a semantic version (vMAJOR.MINOR.PATCH, optionally with -prerelease; no leading zeros, no empty parts, no +build)" >&2
  exit 1
fi

# A published release has to read as one to treerig's own check: `worktree
# remove` treats a build whose version passes it as a release, which removes
# without a pin, and anything else as a development build. So a tag that check
# would reject (0.0.0 in any form, or the tail `git describe` adds past a tag)
# cannot be released. Build metadata (+...) is already out above.
if printf '%s' "$version" | grep -Eq '^0\.0\.0($|-)'; then
  echo "::error::tag '${tag}' is 0.0.0, the version of development builds" >&2
  exit 1
fi
if printf '%s' "$version" | grep -Eq -e '-[0-9]+-g[0-9a-f]{7,}$' -e '-(dirty|broken)$'; then
  echo "::error::tag '${tag}' ends like a git describe version (-N-gHASH, -dirty, -broken)" >&2
  exit 1
fi
