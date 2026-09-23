// Command berth gives every Git worktree a durable identity, an isolated
// home, and a routed URL. This is the Go build; see docs/spike.
package main

import (
	"os"

	"github.com/sofired/berth/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args, os.Stdin, os.Stdout, os.Stderr, os.LookupEnv))
}
