#!/bin/sh
# Splits the Docker integration run across CI jobs (the docker job in
# .github/workflows/go.yml). internal/commands is nearly all of its time, in
# serial Docker tests, and internal/router and internal/cli are the next
# largest, so the packages run as five shards:
#
#   1, 2   internal/commands: the slow tests named below, selected by an
#          anchored -run
#   3      internal/commands: every other test, selected by -skip of the names
#          in 1 and 2, so a test added later lands here with no edit
#   slow   the packages named in slowpkgs below
#   rest   every other package, listed by `go list` each time, so a new
#          package lands here with no edit
#
# usage: .github/scripts/commands-shards.sh run <1|2|3|slow|rest> [go test flags...]
#        .github/scripts/commands-shards.sh check [go test flags...]
#
# run starts `go test` with the flags given plus the shard's selection.
# check fails unless every top-level test in internal/commands is selected by
# exactly one of shards 1 to 3 (asked of `go test -list`), every name listed
# below still exists, and every package `go list` finds is in exactly one of
# commands, slow and rest, with every named package still existing. Run it
# from the repository root, with the same -race flag as the run it guards, so
# the test binary is the cached one. It starts no test, so it needs no Docker.
#
# Moving a test between shards, or naming a new slow one or package, is an edit
# to the lists below and nothing else. The names are exact: each is anchored in the
# pattern, so TestUp never selects TestUpX. Per-test times they were balanced
# on are in the docker job's comment in go.yml.
set -eu

pkg=./internal/commands
module=github.com/sofired/treerig
pkgpath=$module/internal/commands

# slowpkgs are the packages, besides internal/commands, that get a shard of
# their own, by path under the module.
slowpkgs='
internal/router
internal/cli
'

shard1='
TestHostRouteAnswersAtTheWorktreeHostnameOnARealDaemon
TestRenameOnARealDaemon
TestSharedAnonymousVolumesOnARealDaemon
TestSharedServicesOnARealDaemon
TestUpOverAnAlreadyRegisteredWorktreeOnARealDaemon
TestUpUsesTheSettingsFilePortOnARealDaemon
'

shard2='
TestConcurrentFirstUpsStartOneSharedServiceAcrossProcesses
TestGoneSharedContainerKeepsA502RouteUntilSharedDownOnARealDaemon
TestHostRouteToAnIPv6OnlyListenerOnARealDaemon
TestLoopbackPortsOnARealDaemon
TestRouterEnsureRepairsStaleRouteFilesOnARealDaemon
TestSharedWebUIThroughTheProxyOnARealDaemon
TestTwoWorktreesOfOneStackOnARealDaemon
'

# names prints a list without its blank lines.
names() {
	printf '%s\n' "$1" | sed '/^$/d'
}

# anchored turns a list of names into the pattern ^(a|b|c)$.
anchored() {
	printf '^(%s)$' "$(names "$1" | paste -sd'|' -)"
}

# fail prints a message to stderr, as a workflow annotation on Actions, and exits.
fail() {
	if [ -n "${GITHUB_ACTIONS:-}" ]; then
		echo "::error::$*" >&2
	else
		echo "commands-shards: $*" >&2
	fi
	exit 1
}

# modpkgs prints every package under the module, as go list finds them. -tags
# integration lists a package whose only Go files are integration test files.
modpkgs() {
	go list -tags integration -f '{{.ImportPath}}' ./... || fail "go list failed"
}

# pkglist prints the packages of shard $1, slow or rest.
pkglist() {
	# Captured first, so a go list failure stops the run instead of shortening it.
	all=$(modpkgs) || exit 1
	all=$(printf '%s\n' "$all" | grep -vFx -- "$pkgpath")
	slow=$(names "$slowpkgs" | sed "s#^#$module/#")
	case "$1" in
	slow) printf '%s\n' "$all" | grep -Fx -- "$slow" || true ;;
	rest) printf '%s\n' "$all" | grep -vFx -- "$slow" || true ;;
	esac
}

# listed prints the top-level tests, examples and fuzz targets of the package
# that a -list pattern matches, as Go itself evaluates the pattern. -list
# ignores -run and -skip, which is why the patterns are asked for one by one.
listed() {
	pattern=$1
	shift
	out=$(go test "$@" -tags integration -list "$pattern" "$pkg") || fail "go test -list failed:
$out"
	printf '%s\n' "$out" | grep -E '^(Test|Example|Fuzz)' || true
}

# count prints how many lines of $2 equal the line $1, whole.
count() {
	printf '%s\n' "$2" | grep -cFx -- "$1" || true
}

case "${1:-}" in
run)
	[ $# -ge 2 ] || fail "usage: run <1|2|3|slow|rest> [go test flags...]"
	shard=$2
	shift 2
	case "$shard" in
	1) exec go test "$@" -run "$(anchored "$shard1")" "$pkg" ;;
	2) exec go test "$@" -run "$(anchored "$shard2")" "$pkg" ;;
	# One level on purpose: a single-element -skip matches only top-level
	# names, so a subtest named like a listed test still runs. A trailing /
	# would skip the listed tests' subtests instead, and run their bodies here.
	3) exec go test "$@" -skip "$(anchored "$shard1
$shard2")" "$pkg" ;;
	slow | rest)
		pkgs=$(pkglist "$shard")
		[ -n "$pkgs" ] || fail "no packages in shard $shard"
		# shellcheck disable=SC2086 # one word per package
		exec go test "$@" $pkgs
		;;
	*) fail "unknown shard '$shard': want 1, 2, 3, slow or rest" ;;
	esac
	;;
check)
	shift
	all=$(listed . "$@")
	[ -n "$all" ] || fail "no tests listed in $pkg"
	dup=$(printf '%s\n%s\n' "$(names "$shard1")" "$(names "$shard2")" | sort | uniq -d)
	[ -z "$dup" ] || fail "named in more than one shard list: $dup"
	in1=$(listed "$(anchored "$shard1")" "$@")
	in2=$(listed "$(anchored "$shard2")" "$@")
	skipped=$(listed "$(anchored "$shard1
$shard2")" "$@")
	bad=0
	for name in $(names "$shard1") $(names "$shard2"); do
		if [ "$(count "$name" "$all")" -ne 1 ]; then
			echo "commands-shards: $name is named in a shard list but is not a test in $pkg" >&2
			bad=1
		fi
	done
	n3=0
	for name in $all; do
		n=0
		[ "$(count "$name" "$in1")" -eq 0 ] || n=$((n + 1))
		[ "$(count "$name" "$in2")" -eq 0 ] || n=$((n + 1))
		if [ "$(count "$name" "$skipped")" -eq 0 ]; then
			n=$((n + 1))
			n3=$((n3 + 1))
		fi
		if [ "$n" -ne 1 ]; then
			echo "commands-shards: $name is selected by $n shards, want exactly 1" >&2
			bad=1
		fi
	done
	allpkgs=$(modpkgs)
	slowp=$(pkglist slow)
	restp=$(pkglist rest)
	for name in $(names "$slowpkgs"); do
		full=$module/$name
		if [ "$full" = "$pkgpath" ]; then
			echo "commands-shards: $name has the command shards of its own, so it cannot be named in slowpkgs" >&2
			bad=1
		elif [ "$(count "$full" "$allpkgs")" -ne 1 ]; then
			echo "commands-shards: $name is named in slowpkgs but is not a package" >&2
			bad=1
		fi
	done
	dup=$(names "$slowpkgs" | sort | uniq -d)
	[ -z "$dup" ] || { echo "commands-shards: named twice in slowpkgs: $dup" >&2; bad=1; }
	for name in $allpkgs; do
		n=0
		[ "$name" != "$pkgpath" ] || n=$((n + 1))
		[ "$(count "$name" "$slowp")" -eq 0 ] || n=$((n + 1))
		[ "$(count "$name" "$restp")" -eq 0 ] || n=$((n + 1))
		if [ "$n" -ne 1 ]; then
			echo "commands-shards: package $name is in $n shards, want exactly 1" >&2
			bad=1
		fi
	done
	if [ -z "$slowp" ] || [ -z "$restp" ]; then
		echo "commands-shards: the slow or rest package shard is empty" >&2
		bad=1
	fi
	[ "$bad" -eq 0 ] || fail "the shards in .github/scripts/commands-shards.sh do not cover the tests of $pkg and the packages of the module exactly once"
	echo "package shards: slow $(printf '%s\n' "$slowp" | grep -c .), rest $(printf '%s\n' "$restp" | grep -c .), plus internal/commands"
	echo "commands shards cover all $(printf '%s\n' "$all" | wc -l | tr -d ' ') tests exactly once:" \
		"shard 1 $(printf '%s\n' "$in1" | grep -c .)," \
		"shard 2 $(printf '%s\n' "$in2" | grep -c .)," \
		"shard 3 (everything else) $n3"
	;;
*)
	fail "usage: $0 run <1|2|3|slow|rest> [go test flags...] | check [go test flags...]"
	;;
esac
