//go:build linux

package run

import "fmt"

// shebangMaxDepth is the deepest classifyFormat may go and still treat a
// "#!" line found there as ordinary, rather than refusing outright — how
// many further interpreter indirections a real Linux exec(2) tolerates
// before giving up. fs/exec.c's exec_binprm (Linux v6.6) bounds this with a
// plain loop counter, not a named constant:
//
//	/* This allows 4 levels of binfmt rewrites before failing hard. */
//	for (depth = 0;; depth++) {
//		if (depth > 5)
//			return -ELOOP;
//		...
//	}
//
// six binary-handler attempts in total — the hook's own script, plus up to
// five interpreter indirections — matching the "4 levels of ... rewrites"
// the kernel's own comment describes (the sixth attempt, depth 5, is the
// last one still allowed; depth 6 is refused). Older kernels (e.g. v5.4)
// carried the identical "> 5" check directly in search_binary_handler, as
// bprm->recursion_depth, rather than this loop. classifyFormat's own depth
// counts attempts the same way — 0 for the hook's own script — so this is 5.
const shebangMaxDepth = 5

// execMaxDepth is the deepest attempt Linux makes at all: the "depth > 5"
// check above runs before the file at that depth is even looked at, so a
// native binary sitting one level past it is refused with ELOOP just like
// another script would be.
const execMaxDepth = 5

// shebangDepthError is the StartError a real Linux exec(2) gives once an
// interpreter chain goes deeper than shebangMaxDepth: -ELOOP. osrun's own
// classifyExecError has no dedicated ELOOP branch — a real run hits it too
// rarely to deserve one — so a real ELOOP there falls to that function's
// generic errno case, an ordinary 126; this prediction matches that
// fallback exactly rather than inventing a code of its own.
func shebangDepthError(interpreter string) *StartError {
	return &StartError{Code: "ELOOP", Detail: fmt.Sprintf(
		"its #! interpreter `%s` starts a chain of #! interpreters too many levels deep (ELOOP).", interpreter), ExitCode: 126}
}
