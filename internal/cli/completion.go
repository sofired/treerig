package cli

import (
	"context"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/text"
)

// completionContext is what every read in this file runs with: cobra never
// threads the root command's own context into a ValidArgsFunction or a
// flag's own completion function — finding the command completion is asked
// about (getCompletions' own Find, unlike ExecuteC) never sets its ctx, so
// cmd.Context() here would only ever be context.Background() regardless —
// and nothing this file reads is cancellable I/O, only local files.
func completionContext() context.Context { return context.Background() }

// noCompletions is what every completion function in this file answers on
// any read failure — a registry it cannot resolve, a berth.toml it cannot
// find — the same ShellCompDirectiveNoFileComp, no candidates, addCompletion's
// own default already answers for a command that declares nothing else:
// shell completion must never fail loudly, so an error and "nothing to
// offer" look identical to the shell.
func noCompletions() ([]cobra.Completion, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// withPrefix keeps only the completions whose own value — the part before
// a cobra.CompletionWithDesc's tab, or the whole string for one with no
// description — starts with toComplete. Cobra calls every completion
// function in this file with the word still being typed, but, unlike its
// own filtering of a plain ValidArgs list, never filters a function's
// answer by it (see cobra's completions.go, getCompletions): every
// function here has to.
func withPrefix(comps []cobra.Completion, toComplete string) []cobra.Completion {
	var kept []cobra.Completion
	for _, comp := range comps {
		value, _, _ := strings.Cut(comp, "\t")
		if strings.HasPrefix(value, toComplete) {
			kept = append(kept, comp)
		}
	}
	return kept
}

// worktreeCandidates resolves cmd's own commands.Env and CommandContext —
// the same p.env/commands.Context sequence every RunE in this tree
// builds — and reads every registered worktree through it: the boilerplate
// identifierCompletionFunc and domainCompletionFunc both need before they
// can do anything else. Any failure along the way (no config found, an
// unsafe or unresolvable home, an unreadable registry) is returned
// unchanged, for the caller to turn into noCompletions() — this helper
// never itself decides what "no completions" looks like.
func worktreeCandidates(p *program, cmd *cobra.Command) (*commands.CommandContext, []commands.RegisteredWorktreeCandidate, error) {
	env, err := p.env(cmd)
	if err != nil {
		return nil, nil, err
	}
	cctx, err := commands.Context(env)
	if err != nil {
		return nil, nil, err
	}
	candidates, err := commands.RegisteredWorktreeCandidates(completionContext(), cctx)
	if err != nil {
		return nil, nil, err
	}
	return cctx, candidates, nil
}

// describeWorktreeCandidate is a registered worktree's own completion
// description: its domain, and its last-known path when one is on record.
// Neither the registry record's Domain nor the checkout-index card's Root
// is revalidated on read (a record written by an older build, or a branch
// name that made it into a path, could carry a control character), and a
// completion's description is otherwise printed by the shell verbatim, so
// both go through text.Printable the same way `worktree list` prints them
// (internal/commands/list.go) — which also turns a tab, newline or
// carriage return into a plain space, keeping the description from ever
// injecting one into cobra's line-based, tab-delimited value/description
// protocol.
func describeWorktreeCandidate(c commands.RegisteredWorktreeCandidate) string {
	domain := text.Printable(c.Domain)
	if c.Root == "" {
		return domain
	}
	return domain + " — " + text.Printable(c.Root)
}

// identifierCompletionFunc is `worktree remove`'s and `worktree unlock`'s
// own ValidArgsFunction: every worktree registered anywhere on this
// machine — registry.Registry.FindGlobal's own search space, not just the
// repository the command runs in — sorted by slug and each described by
// its domain and its last-known path, narrowed to --domain once one is
// already given. Both commands take at most one identifier, so nothing is
// offered once one is already there. unlock's own (includeOrphans true)
// also offers, after every registered slug and sorted by UUID, a worktree
// resolveUnlockTarget can still recover a stale lock for even though it
// was never registered (or is no longer), named only by its own UUID — the
// one handle it has for one. Orphans carry no domain, so once --domain is
// given they can never match it and none are offered.
func identifierCompletionFunc(p *program, includeOrphans bool) func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return noCompletions()
		}
		cctx, candidates, err := worktreeCandidates(p, cmd)
		if err != nil {
			return noCompletions()
		}
		domain, _ := cmd.Flags().GetString("domain")
		// candidates is already sorted by slug (RegisteredWorktreeCandidates),
		// so appending in order keeps that as this list's own primary order.
		registered := make(map[string]bool, len(candidates))
		var comps []cobra.Completion
		for _, c := range candidates {
			registered[c.WorktreeID] = true
			if domain != "" && c.Domain != domain {
				continue
			}
			comps = append(comps, cobra.CompletionWithDesc(c.Slug, describeWorktreeCandidate(c)))
		}
		if includeOrphans && domain == "" {
			if orphans, err := commands.OrphanWorktrees(cctx.ControlHome); err == nil {
				sort.Slice(orphans, func(i, j int) bool { return orphans[i].WorktreeID < orphans[j].WorktreeID })
				for _, orphan := range orphans {
					if registered[orphan.WorktreeID] {
						continue
					}
					comps = append(comps, cobra.CompletionWithDesc(orphan.WorktreeID, "recoverable, not registered"))
				}
			}
		}
		// Not sorted again here: sorting the combined value\tdescription
		// strings would interleave orphan UUIDs among slugs instead of
		// listing every registered worktree first, each list in its own order.
		// KeepOrder asks bash and zsh not to re-sort it either.
		return withPrefix(comps, toComplete), cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveKeepOrder
	}
}

// domainCompletionFunc is `worktree remove --domain`'s and `worktree
// unlock --domain`'s own flag completion function: every domain a worktree
// is registered under anywhere on this machine, deduplicated and sorted,
// leaving out any that carries a control character.
func domainCompletionFunc(p *program) func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		_, candidates, err := worktreeCandidates(p, cmd)
		if err != nil {
			return noCompletions()
		}
		seen := map[string]bool{}
		var comps []cobra.Completion
		for _, c := range candidates {
			// A domain Printable would change can't be offered: the shell
			// would insert the cleaned text, which no registered worktree
			// carries, so the command could never resolve it.
			if text.Printable(c.Domain) != c.Domain || seen[c.Domain] {
				continue
			}
			seen[c.Domain] = true
			comps = append(comps, c.Domain)
		}
		sort.Strings(comps)
		return withPrefix(comps, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
}

// serviceCompletionDescription is one declared service's own completion
// description, from berth.toml alone: its [service.index] label when the
// config gave it one, otherwise the shape of its own route — its subdomain,
// its path, or "host root" for neither — so a completion candidate still
// says something useful without ever reading the registry. Subdomain and
// path are both schema-constrained (subdomainPattern, urlPathPattern in
// internal/config/schema.go), so, unlike the label, neither needs
// text.Printable: schema.go's own comment on [service.index] says plainly
// that neither label nor purpose refuses a control character, so this base
// stays lenient for every command that only reads an already-running
// worktree's config — a completion candidate is exactly such a reader.
func serviceCompletionDescription(svc config.Service) string {
	if svc.Index != nil && svc.Index.Label != "" {
		return text.Printable(svc.Index.Label)
	}
	if svc.Subdomain != "" {
		return "subdomain " + svc.Subdomain
	}
	if svc.Path != nil && *svc.Path != "" {
		return "path " + *svc.Path
	}
	return "host root"
}

// serviceCompletionFunc is `up`'s own ValidArgsFunction: the service names
// this worktree's own berth.toml declares (Config.Service, loaded directly
// with internal/config's own loader — a service name is a config-file
// question, nothing a CommandContext's registry or control home has any
// part in), each described by serviceCompletionDescription, minus whichever
// the command line already named, since `up` takes any number of them.
func serviceCompletionFunc(p *program) func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		loaded, err := completionConfig(p, cmd)
		if err != nil {
			return noCompletions()
		}
		given := make(map[string]bool, len(args))
		for _, a := range args {
			given[a] = true
		}
		var comps []cobra.Completion
		for _, svc := range loaded.Config.Service {
			if given[svc.Name] {
				continue
			}
			comps = append(comps, cobra.CompletionWithDesc(svc.Name, serviceCompletionDescription(svc)))
		}
		sort.Strings(comps)
		return withPrefix(comps, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
}

// completionConfig loads the berth.toml cmd's own --config, or the nearest
// one above the working directory, names: a config-file question, nothing a
// CommandContext's registry or control home has any part in. Any failure is
// returned for the caller to turn into noCompletions().
func completionConfig(p *program, cmd *cobra.Command) (*config.Loaded, error) {
	env, err := p.env(cmd)
	if err != nil {
		return nil, err
	}
	if env.ConfigPath != "" {
		return config.Load(env.ConfigPath)
	}
	return config.LoadNearest(env.Cwd)
}

// commandCompletionFunc is `run`'s own ValidArgsFunction: the command names
// this worktree's berth.toml declares under [commands], in file order, each
// described by its command line (one line, clamped). Only the name is
// completed: nothing is offered once one is given, and so nothing after
// "plan --", where the arguments are the command's own. (Cobra appends a
// "--" of its own when it parses a completion request, so ArgsLenAtDash
// cannot tell a user's "--" apart here.) A berth.toml that cannot be loaded
// completes nothing.
func commandCompletionFunc(p *program) func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return noCompletions()
		}
		loaded, err := completionConfig(p, cmd)
		if err != nil {
			return noCompletions()
		}
		var comps []cobra.Completion
		for _, entry := range loaded.Config.Commands {
			comps = append(comps, cobra.CompletionWithDesc(entry.Name, text.OneLine(entry.Command.Command, commandDescriptionMax)))
		}
		// Not sorted: file order is the order `run` itself lists them in.
		return withPrefix(comps, toComplete), cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveKeepOrder
	}
}

// commandDescriptionMax clamps a command's completion description, which a
// shell prints beside the name on one line.
const commandDescriptionMax = 60
