// Command berth gives every Git worktree a durable identity, an isolated
// home, and a routed URL. This is the Go build; see
// docs/decisions/0001-port-to-go.md.
package main

import (
	"os"

	"github.com/sofired/berth/internal/cli"
)

// main hands the process's argv, standard streams and environment lookup to
// cli.Main and exits with the code it returns; it is the only place in the
// Go build that reads os.Args or os.Environ directly.
func main() {
	os.Exit(cli.Main(os.Args, os.Stdin, os.Stdout, os.Stderr, os.LookupEnv))
}
