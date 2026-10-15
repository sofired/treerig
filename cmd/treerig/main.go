// Command treerig gives every Git worktree a durable identity, an isolated
// home, and a routed URL.
package main

import (
	"os"

	"github.com/sofired/treerig/internal/cli"
)

// main hands the process's argv, standard streams and environment lookup to
// cli.MainProcess and exits with the code it returns; it is the only place
// in the Go build that reads os.Args or os.Environ directly. MainProcess,
// not cli.Main, because this is the one process allowed to let `worktree
// remove` replace its own image with a standalone copy of itself (#7): a
// real treerig build, never a go test binary or anything else linking
// internal/cli in-process.
func main() {
	os.Exit(cli.MainProcess(os.Args, os.Stdin, os.Stdout, os.Stderr, os.LookupEnv))
}
