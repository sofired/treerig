package cli

// buildProgram assembles the command tree src/cli/main.ts defines, verb for
// verb, in the same order and with the same descriptions, arguments and
// options — so help.go's rendering and suggest.go's candidates come out
// identical to the TypeScript build's. See internal/cli's package comment
// for how this differs from actually depending on commander.
//
// A fresh tree is built on every call (rather than kept as a package-level
// singleton) because node.values is mutated during parsing: cli.Main may run
// many times in one process, in tests, and each run needs to start clean —
// the way a new `new Command()` per process does for the TypeScript build.

import (
	"fmt"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/commands"
)

func buildProgram(commandName, version string) *node {
	root := newCommand(commandName, "Give every Git worktree a durable identity, an isolated home, and a routed URL.")
	root.setVersion(version)
	root.addOption("--config <path>", fmt.Sprintf("path to %s (default: nearest above cwd)", brand.ConfigFile))

	config := newCommand("config", "inspect the project configuration")
	config.addChild(newCommand("validate", fmt.Sprintf("parse and validate %s", brand.ConfigFile)).
		setAction(func(rc *runContext) error { return commands.ConfigValidate(rc.ctx, rc.env) }))
	config.addChild(newCommand("show", "print the resolved configuration as JSON").
		setAction(func(rc *runContext) error { return commands.ConfigShow(rc.ctx, rc.env) }))
	root.addChild(config)

	root.addChild(newCommand("dev", "start this worktree's environment and follow its logs (not yet implemented)").
		setAction(func(rc *runContext) error {
			return clierr.WithCode(fmt.Sprintf("'%s dev' is not implemented yet; see the Roadmap in README.md.", rc.env.CommandName), 2)
		}))

	root.addChild(newCommand("up", "register this worktree, start its services, and run its hooks").
		addArgument("[services...]", "").
		setAction(func(rc *runContext) error { return commands.NotPorted(rc.env.CommandName, "up") }))

	root.addChild(newCommand("down", "stop this worktree's services; data is kept").
		setAction(func(rc *runContext) error { return commands.Down(rc.ctx, rc.env) }))

	root.addChild(newCommand("status", "show service status for this worktree").
		setAction(func(rc *runContext) error { return commands.Status(rc.ctx, rc.env) }))

	root.addChild(newCommand("urls", "print this worktree's routed URLs").
		setAction(func(rc *runContext) error { return commands.Urls(rc.ctx, rc.env) }))

	root.addChild(newCommand("index", "refresh the machine-wide checkout index and print it").
		setAction(func(rc *runContext) error { return commands.NotPorted(rc.env.CommandName, "index") }))

	root.addChild(newCommand("doctor", "check prerequisites, the registry, the router, and this checkout's verify hook").
		setAction(func(rc *runContext) error { return commands.NotPorted(rc.env.CommandName, "doctor") }))

	router := newCommand("router", "manage the shared local router")
	router.addChild(newCommand("up", "start the shared router and verify its hardened configuration").
		setAction(func(rc *runContext) error { return commands.RouterUp(rc.ctx, rc.env) }))
	router.addChild(newCommand("status", "report whether the shared router is running as expected").
		setAction(func(rc *runContext) error {
			failed, err := commands.RouterStatus(rc.ctx, rc.env)
			if err != nil {
				return err
			}
			if failed {
				rc.exit(1)
			}
			return nil
		}))
	router.addChild(newCommand("down", "stop the shared router; worktree services keep running but are no longer routed").
		setAction(func(rc *runContext) error { return commands.RouterDown(rc.ctx, rc.env) }))
	root.addChild(router)

	worktree := newCommand("worktree", "manage registered worktrees")
	list := newCommand("list", "list registered worktrees")
	list.addOption("--all", "every repository and domain on this machine")
	list.addOption("--json", "machine-readable output")
	list.setAction(func(rc *runContext) error {
		return commands.List(rc.ctx, rc.env, commands.ListOptions{All: rc.bool("--all"), JSON: rc.bool("--json")})
	})
	worktree.addChild(list)

	remove := newCommand("remove", "remove a worktree's environment: containers, volumes, networks, images, home, and registration")
	remove.addArgument("[identifier]", "")
	remove.addOption("--domain <domain>", "narrow a slug registered under more than one domain")
	remove.addOption("--label-namespace <namespace>", "supply the Docker label namespace when nothing on disk records it (a worktree of another repository)")
	remove.addOption("--yes", "skip the confirmation phrase")
	remove.setAction(func(rc *runContext) error {
		return commands.Remove(rc.ctx, rc.env, commands.RemoveOptions{
			Identifier:     rc.arg(0),
			Domain:         rc.string("--domain"),
			LabelNamespace: rc.string("--label-namespace"),
			Yes:            rc.bool("--yes"),
		})
	})
	worktree.addChild(remove)

	unlock := newCommand("unlock", "clear a dead holder's lock on a worktree, once the holder and every command it started on this machine are checked gone")
	unlock.addArgument("[identifier]", "")
	unlock.addOption("--domain <domain>", "narrow a slug registered under more than one domain")
	unlock.addOption("--holder-unverifiable", "accept a holder this machine cannot check (another machine or container, or no record), once you have made sure it is gone")
	unlock.setAction(func(rc *runContext) error { return commands.NotPorted(rc.env.CommandName, "worktree unlock") })
	worktree.addChild(unlock)
	root.addChild(worktree)

	return root
}
