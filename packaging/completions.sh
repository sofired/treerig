#!/bin/sh
# Writes treerig's bash, zsh and fish completion scripts into completions/,
# for GoReleaser's before hook (.goreleaser.yaml) to put in the archives and
# the Linux packages.
#
# The scripts depend only on the command tree, not on the platform, so one
# build for the host is enough even when the release cross-compiles. treerig is
# built under the name `treerig` in a scratch directory because cobra writes the
# invoked program's name into the scripts it generates.
set -eu

out=completions
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$out"
CGO_ENABLED=0 go build -trimpath -o "$tmp/treerig" ./cmd/treerig

"$tmp/treerig" completion bash >"$out/treerig.bash"
"$tmp/treerig" completion zsh >"$out/_treerig"
"$tmp/treerig" completion fish >"$out/treerig.fish"
