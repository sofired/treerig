package commands

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sofired/treerig/internal/checkoutindex/card"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/text"
	"github.com/sofired/treerig/internal/worktree"
	"github.com/sofired/treerig/internal/worktreestate"
)

// Read-only. Telling a worktree whose directory is gone needs Git, so it is
// only offered for the repository the command runs in; records from other
// repositories are listed as the registry knows them, with the state their
// containers and history give.

// ListedRegistration is one row of `worktree list`. Field order is the JSON
// key order `--json` prints.
type ListedRegistration struct {
	RepositoryID   string  `json:"repositoryId"`
	WorktreeID     string  `json:"worktreeId"`
	Slug           string  `json:"slug"`
	Domain         string  `json:"domain"`
	LabelNamespace *string `json:"labelNamespace,omitempty"`
	Host           string  `json:"host"`
	// State is the worktree's state in the words every command uses (JSON:
	// running, starting, partly_running, failed, stopped, not_started,
	// removing, directory_missing, unknown). Hint names the cause of an
	// unknown one.
	State worktree.State `json:"state"`
	Hint  string         `json:"hint,omitempty"`
	// Root and Branch are live from Git for this repository's worktrees;
	// otherwise as last recorded by `up`, via its index card.
	Root    *string `json:"root,omitempty"`
	Branch  *string `json:"branch,omitempty"`
	Current bool    `json:"current"`
}

// List is `worktree list`. It needs no treerig.toml (ContextWithoutConfig):
// outside a repository it lists the worktrees of every repository, as --all
// does.
func List(ctx context.Context, env *Env, options ListOptions) error {
	cctx, err := ContextWithoutConfig(env)
	if err != nil {
		return err
	}
	_, err = ListWithContext(ctx, cctx, options)
	return err
}

// ListWithContext is `list`, operating directly on a CommandContext so it
// can be tested without a config file on disk.
//
// Outside every Git work tree there is no "this repository", so the command
// lists every registered worktree, exactly as --all does. Inside one, with or
// without a usable treerig.toml, it lists that repository's worktrees; when
// there are none it says so and points at --all.
func ListWithContext(ctx context.Context, cctx *CommandContext, options ListOptions) ([]ListedRegistration, error) {
	reg := cctx.Registry
	repositoryID, present, inside, err := reg.Identities.Repository(ctx, reg.Identities.Root)
	if err != nil {
		return nil, err
	}
	if !inside {
		options.All = true
	}
	var current *registry.WorktreeIdentity
	if present {
		current, err = reg.Identities.Inspect(ctx, reg.Identities.Root, registry.InspectOptions{})
		if err != nil {
			return nil, err
		}
	}
	// Git is asked once for the repository the command runs in: its entries
	// say where each worktree is now, and what became of the directories of
	// the ones that are gone.
	active := map[string]registry.WorktreeEntry{}
	var repositories map[string]worktreestate.Repository
	if present {
		entries, entriesErr := reg.Identities.Entries(ctx)
		if entriesErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
		}
		for _, entry := range entries {
			if entry.Exists && entry.WorktreeID != "" && entry.RepositoryID == repositoryID {
				active[entry.WorktreeID] = entry
			}
		}
		repositories = map[string]worktreestate.Repository{repositoryID: worktreestate.Answered{Listed: entries, Err: entriesErr}}
	}

	var records []registry.RegisteredWorktree
	if options.All {
		records, err = reg.ListAll(ctx)
	} else {
		records, err = reg.List(ctx)
	}
	if err != nil {
		return nil, err
	}

	// The cards say which services a worktree declared and where it was last
	// seen; they are read one by one, which is as many files as there are rows.
	cards := make([]*card.IndexCard, len(records))
	cardErrs := make([]error, len(records))
	subjects := make([]worktreestate.Subject, len(records))
	for i, record := range records {
		cards[i], cardErrs[i] = card.ReadIndexCard(cctx.ControlHome, record.RepositoryID, record.WorktreeID)
		subjects[i] = worktreestate.SubjectOf(record, cards[i], cardErrs[i])
	}
	// Nothing to classify, nothing to ask Docker.
	report := worktreestate.Report{Docker: worktreestate.Inventory{Answered: true}}
	if len(records) > 0 {
		report, err = worktreestate.New(worktreestate.Options{
			ControlHome: cctx.ControlHome, Runner: cctx.Runner, InventoryTimeout: listDockerBudget,
		}).Gather(ctx, worktreestate.Request{
			Subjects: subjects, Namespaces: worktreestate.Namespaces(cctx.ControlHome, records, cards), Repositories: repositories,
		})
		if err != nil {
			return nil, err
		}
	}
	if report.GitError != "" {
		cctx.Logger.Warn(fmt.Sprintf("Git didn't answer, so directories are not checked: %s", text.OneLine(report.GitError, 300)))
	}

	rows := make([]ListedRegistration, 0, len(records))
	for i, record := range records {
		// Root and Branch are live from Git for this repository's worktrees;
		// otherwise as the last `up` recorded them. A card that could not be
		// read leaves them unset, as one that was never written does; the
		// state says the difference (unknown, with the reason).
		var seenRoot, seenBranch *string
		if entry, ok := active[record.WorktreeID]; ok && present && record.RepositoryID == repositoryID {
			seenRoot, seenBranch = &entry.Root, &entry.Branch
		} else if cardErrs[i] == nil && cards[i] != nil {
			seenRoot, seenBranch = &cards[i].Root, &cards[i].Branch
		}
		state := report.States[worktreestate.Key(record.RepositoryID, record.WorktreeID)]
		rows = append(rows, ListedRegistration{
			RepositoryID: record.RepositoryID, WorktreeID: record.WorktreeID,
			Slug: record.Slug, Domain: record.Domain, LabelNamespace: record.LabelNamespace,
			Host: layout.WorktreeHost(record.Slug, record.Domain), State: state.State, Hint: state.Hint,
			Root: seenRoot, Branch: seenBranch,
			Current: current != nil && current.WorktreeID == record.WorktreeID,
		})
	}
	// A fixed locale, as the index sorts with; see TestListSortMatchesLocale
	// in internal/registry for why plain string comparison agrees with it
	// for this schema's alphabet. Stable, matching modern JS engines'
	// Array.prototype.sort.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Domain != rows[j].Domain {
			return rows[i].Domain < rows[j].Domain
		}
		return rows[i].Slug < rows[j].Slug
	})

	if options.JSON {
		if err := printJSON(cctx, rows); err != nil {
			return nil, err
		}
	} else if len(rows) == 0 {
		// Prose, not data: stdout stays empty, the way `status` answers.
		// `--json` is the scriptable form and says `[]`.
		cctx.Logger.Info(EmptyTable(options.All))
		if !options.All {
			cctx.Logger.Info(fmt.Sprintf("Every repository: '%s worktree list --all'", cctx.CommandName))
		}
	} else {
		for _, line := range FormatTable(rows, options.All) {
			cctx.Logger.Output(line)
		}
		// Why a row is unknown, unless it is the cause already given once.
		dockerHint := worktreestate.DockerHint(report.Docker.Error)
		for _, row := range rows {
			if row.Hint != "" && row.Hint != dockerHint && row.Hint != report.GitHint {
				cctx.Logger.Info(text.Printable(fmt.Sprintf("%s is unknown. %s", row.Host, row.Hint)))
			}
		}
	}
	// The table is printed whatever happened; Docker not answering still
	// fails the command, as it does `status`, with the cause said once.
	if !report.Docker.Answered {
		return rows, clierr.New(worktreestate.DockerHint(report.Docker.Error))
	}
	return rows, nil
}

// listDockerBudget bounds the one `docker ps` per label namespace a listing
// makes: a listing is read, not waited for, so a daemon that is hung is
// reported rather than waited out.
const listDockerBudget = 5 * time.Second

// EmptyTable is what there is to say when there is no table.
func EmptyTable(all bool) string {
	if all {
		return "No worktrees are registered on this machine."
	}
	return "No worktrees are registered for this repository."
}

// describeBranchText is the table's word for a branch, the same wording
// checkoutindex.DescribeBranch gives, whose `.Text` is the only part a
// plain-text table has a use for.
func describeBranchText(branch string) string {
	if branch == "" {
		return "(detached)"
	}
	return branch
}

// FormatTable renders rows the way `worktree list` prints them.
func FormatTable(rows []ListedRegistration, all bool) []string {
	if len(rows) == 0 {
		return []string{EmptyTable(all)}
	}
	type column struct {
		title string
		value func(ListedRegistration) string
	}
	// printable, before the widths are measured: a branch or a path is
	// whatever a clone put there, and this table is printed to a terminal.
	columns := []column{
		{"HOST", func(r ListedRegistration) string { return text.Printable(r.Host) }},
		{"STATE", func(r ListedRegistration) string { return r.State.String() }},
		// The index's word for a detached HEAD, so one state has one name;
		// "-" for nothing recorded, which is this table's spelling and what
		// ROOT says beside it.
		{"BRANCH", func(r ListedRegistration) string {
			if r.Branch == nil {
				return "-"
			}
			return text.Printable(describeBranchText(*r.Branch))
		}},
		{"ROOT", func(r ListedRegistration) string {
			if r.Root == nil {
				return "-"
			}
			return text.Printable(*r.Root)
		}},
	}
	widths := make([]int, len(columns))
	for i, col := range columns {
		width := text.JSLen(col.title)
		for _, row := range rows {
			if l := text.JSLen(col.value(row)); l > width {
				width = l
			}
		}
		widths[i] = width
	}
	line := func(marker string, cells []string) string {
		padded := make([]string, len(cells))
		for i, cell := range cells {
			padded[i] = text.PadEnd(cell, widths[i])
		}
		return strings.TrimRight(marker+" "+strings.Join(padded, "  "), " ")
	}
	titles := make([]string, len(columns))
	for i, col := range columns {
		titles[i] = col.title
	}
	lines := make([]string, 0, len(rows)+1)
	lines = append(lines, line(" ", titles))
	for _, row := range rows {
		marker := " "
		if row.Current {
			marker = "*"
		}
		cells := make([]string, len(columns))
		for i, col := range columns {
			cells[i] = col.value(row)
		}
		lines = append(lines, line(marker, cells))
	}
	return lines
}
