#!/bin/sh
# Writes berth's bash, zsh and fish completion scripts into completions/,
# for GoReleaser's before hook (.goreleaser.yaml) to put in the archives and
# the Linux packages.
#
# The scripts depend only on the command tree, not on the platform, so one
# build for the host is enough even when the release cross-compiles. berth is
# built under the name `berth` in a scratch directory because cobra writes the
# invoked program's name into the scripts it generates.
set -eu

out=completions
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$out"
CGO_ENABLED=0 go build -trimpath -o "$tmp/berth" ./cmd/berth

"$tmp/berth" completion bash >"$out/berth.bash"
"$tmp/berth" completion zsh >"$out/_berth"
"$tmp/berth" completion fish >"$out/berth.fish"
