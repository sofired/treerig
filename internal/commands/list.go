package commands

import (
	"context"
	"sort"
	"strings"

	"github.com/sofired/berth/internal/checkoutindex/card"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/text"
)

// Read-only. Telling a registration from a checkout that is gone needs Git,
// so it is only offered for the repository the command runs in; records
// from other repositories are listed as the registry knows them.

// WorktreeState is a row's state. "missing" is the index's word for the same
// thing: registered, but the checkout is gone.
type WorktreeState string

const (
	StateReady   WorktreeState = "ready"
	StatePending WorktreeState = "pending"
	StateMissing WorktreeState = "missing"
)

// ListedRegistration is one row of `worktree list`. Field order is the JSON
// key order `--json` prints, matching the TypeScript build's object-spread
// order.
type ListedRegistration struct {
	RepositoryID   string        `json:"repositoryId"`
	WorktreeID     string        `json:"worktreeId"`
	Slug           string        `json:"slug"`
	Domain         string        `json:"domain"`
	LabelNamespace *string       `json:"labelNamespace,omitempty"`
	Host           string        `json:"host"`
	State          WorktreeState `json:"state"`
	// Root and Branch are live from Git for this repository's worktrees;
	// otherwise as last recorded by `up`, via its index card.
	Root    *string `json:"root,omitempty"`
	Branch  *string `json:"branch,omitempty"`
	Current bool    `json:"current"`
}

// List is `worktree list`.
func List(ctx context.Context, env *Env, options ListOptions) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	_, err = ListWithContext(ctx, cctx, options)
	return err
}

// ListWithContext is list.ts's `list`, operating directly on a
// CommandContext so it can be tested without a config file on disk.
func ListWithContext(ctx context.Context, cctx *CommandContext, options ListOptions) ([]ListedRegistration, error) {
	reg := cctx.Registry
	repositoryID, present, err := reg.Identities.RepositoryID(ctx, reg.Identities.Root)
	if err != nil {
		return nil, err
	}
	var current *registry.WorktreeIdentity
	if present {
		current, err = reg.Identities.Inspect(ctx, reg.Identities.Root, registry.InspectOptions{})
		if err != nil {
			return nil, err
		}
	}
	active := map[string]registry.WorktreeIdentity{}
	if present {
		activeIdentities, err := reg.Identities.ActiveIdentities(ctx)
		if err != nil {
			return nil, err
		}
		for _, identity := range activeIdentities {
			active[identity.WorktreeID] = identity
		}
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

	rows := make([]ListedRegistration, 0, len(records))
	for _, record := range records {
		local := present && record.RepositoryID == repositoryID
		var identity *registry.WorktreeIdentity
		if local {
			if found, ok := active[record.WorktreeID]; ok {
				found := found
				identity = &found
			}
		}
		initialization, err := reg.Store(record.RepositoryID).Initialization(record.WorktreeID, record.Slug)
		if err != nil {
			return nil, err
		}

		var seenRoot, seenBranch *string
		if identity != nil {
			seenRoot, seenBranch = &identity.Root, &identity.Branch
		} else if seen := card.ReadIndexCard(cctx.ControlHome, record.RepositoryID, record.WorktreeID); seen != nil {
			seenRoot, seenBranch = &seen.Root, &seen.Branch
		}

		state := StateReady
		switch {
		case local && identity == nil:
			state = StateMissing
		case initialization == registry.Pending:
			state = StatePending
		}

		rows = append(rows, ListedRegistration{
			RepositoryID: record.RepositoryID, WorktreeID: record.WorktreeID,
			Slug: record.Slug, Domain: record.Domain, LabelNamespace: record.LabelNamespace,
			Host: layout.WorktreeHost(record.Slug, record.Domain), State: state,
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
		out, err := text.SafeJSON(rows)
		if err != nil {
			return nil, err
		}
		cctx.Logger.Output(out)
	} else if len(rows) == 0 {
		// Prose, not data: stdout stays empty, the way `status` answers.
		// `--json` is the scriptable form and says `[]`.
		cctx.Logger.Info(EmptyTable(options.All))
	} else {
		for _, line := range FormatTable(rows, options.All) {
			cctx.Logger.Output(line)
		}
	}
	return rows, nil
}

// EmptyTable is what there is to say when there is no table.
func EmptyTable(all bool) string {
	if all {
		return "No worktrees are registered on this machine."
	}
	return "No worktrees are registered for this repository."
}

// describeBranchText is the table's word for a branch: a narrow port of
// checkout-index/snapshot.ts's describeBranch, whose `.text` is the only
// part a plain-text table has a use for.
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
		{"STATE", func(r ListedRegistration) string { return string(r.State) }},
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
