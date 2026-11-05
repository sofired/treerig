package commands

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/overview"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/text"
	"github.com/sofired/treerig/internal/worktree"
	"github.com/sofired/treerig/internal/worktreestate"
)

// `list` is read-only. It takes no lock and writes nothing, except with
// --refresh, which rebuilds the overview page under the router's lock.

// ListReport is `list --json`'s document: what the overview page is built from,
// with the page's own address, the router's state and each worktree's URLs.
// Entries are in the page's order. DockerReachable false means no service's
// Available was checked.
type ListReport struct {
	// URL is where the overview page answers.
	URL         string `json:"url"`
	GeneratedAt string `json:"generatedAt"`
	// Router is the shared router's state; absent when there is no worktree to
	// show a URL of, and so it was not read.
	Router           *StatusRouter      `json:"router,omitempty"`
	DockerReachable  bool               `json:"dockerReachable"`
	DockerError      *string            `json:"dockerError,omitempty"`
	IdentityError    *ListIdentityError `json:"identityError,omitempty"`
	CardReadFailures int                `json:"cardReadFailures"`
	Entries          []ListEntry        `json:"entries"`
	// Shared are the routed web UIs of the repositories' shared services;
	// absent when there are none.
	Shared []ListShared `json:"shared,omitempty"`
}

// ListShared is one shared service's routed web UI.
type ListShared struct {
	RepositoryID string `json:"repositoryId"`
	Service      string `json:"service"`
	Project      string `json:"project"`
	Domain       string `json:"domain"`
	Host         string `json:"host"`
	URL          string `json:"url"`
	// Available is set only when it was checked: Docker answered. Absent
	// means unknown, never false.
	Available *bool `json:"available,omitempty"`
}

// ListIdentityError is why Git could not say which worktrees exist.
type ListIdentityError struct {
	Reason      string `json:"reason"`
	RowsUnknown bool   `json:"rowsUnknown"`
}

// ListEntry is one worktree. Branch is "" for a detached HEAD and absent when
// nothing knows; Root, the worktree's directory, is absent when unknown.
type ListEntry struct {
	RepositoryID string  `json:"repositoryId"`
	WorktreeID   string  `json:"worktreeId"`
	Project      string  `json:"project"`
	Domain       string  `json:"domain"`
	Slug         string  `json:"slug"`
	Host         string  `json:"host"`
	Branch       *string `json:"branch,omitempty"`
	Root         string  `json:"root,omitempty"`
	// Current marks the worktree the command ran in.
	Current bool `json:"current"`
	// State is the worktree's state in the words every command uses (JSON:
	// running, starting, partly_running, failed, stopped, not_started,
	// removing, directory_missing, unknown). Hint names the cause of an
	// unknown one.
	State worktree.State `json:"state"`
	Hint  string         `json:"hint,omitempty"`
	// URLs are the addresses that load now, in the order treerig.toml declares
	// the services: a running service's, while the router serves this root's
	// URLs. Services are the ones that opted into the overview, with the
	// address each routes on whether it runs or not.
	URLs     []ListURL     `json:"urls"`
	Services []ListService `json:"services"`
}

// ListURL is a routed service's URL.
type ListURL struct {
	Service string `json:"service"`
	URL     string `json:"url"`
}

// ListService is one declared service of a worktree that opted into the
// overview.
type ListService struct {
	Service string `json:"service"`
	Label   string `json:"label"`
	Purpose string `json:"purpose,omitempty"`
	URL     string `json:"url"`
	// Available is set only when it was checked: Docker answered and the
	// worktree's directory is not missing. Absent means unknown, never false.
	Available *bool `json:"available,omitempty"`
}

// List is `list`. It needs no treerig.toml (ContextWithoutConfig): outside a
// repository it lists the worktrees of every repository, as --all does.
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
func ListWithContext(ctx context.Context, cctx *CommandContext, options ListOptions) (ListReport, error) {
	reg := cctx.Registry
	repositoryID, present, inside, err := reg.Identities.Repository(ctx, reg.Identities.Root)
	if err != nil {
		return ListReport{}, err
	}
	all := options.All || !inside
	if options.Refresh && !all {
		return ListReport{}, clierr.New("--refresh rebuilds the overview, which lists every repository: add --all.")
	}
	var current *registry.WorktreeIdentity
	if present {
		current, err = reg.Identities.Inspect(ctx, reg.Identities.Root, registry.InspectOptions{})
		if err != nil {
			return ListReport{}, err
		}
	}

	// The router is asked while the rest is read, within the same overviewDockerBudget
	// as Docker, since a router that cannot be read shows no URL and nothing more.
	var side sync.WaitGroup
	defer side.Wait()
	var observed router.Observed
	routerCtx, stopRouter := context.WithTimeout(ctx, overviewDockerBudget)
	defer stopRouter()
	side.Go(func() { observed = readRouterState(routerCtx, cctx) })

	var snapshot overview.IndexSnapshot
	var index *overview.Service
	switch {
	case options.Refresh:
		if index, err = newOverview(cctx, listsWorktrees); err != nil {
			return ListReport{}, err
		}
		snapshot, err = index.Refresh(ctx)
	case all:
		index = overviewReader(cctx, listsWorktrees)
		snapshot, err = index.Snapshot(ctx)
	case present:
		index = overviewReader(cctx, listsWorktrees)
		snapshot, err = index.SnapshotOf(ctx, repositoryID)
	default:
		// A repository treerig has never registered a worktree in: nothing to
		// read, and so nothing to ask Docker.
		index = overviewReader(cctx, listsWorktrees)
		snapshot = index.EmptySnapshot()
	}
	if err != nil {
		return ListReport{}, err
	}
	routerAsked := len(snapshot.Entries) > 0
	if !routerAsked {
		// Nothing shows a URL, so the router has nothing to be asked for, and
		// its state is not part of the document.
		stopRouter()
	}
	side.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		// A cancelled read says "unknown" for the router: not an answer to print.
		return ListReport{}, ctxErr
	}

	serves := routerServesURLs(observed, cctx.RouterPort)
	report := listReport(snapshot, index.URL(), current, serves)
	if routerAsked {
		report.Router = routerReport(observed)
	}
	anyRunning := false
	for _, entry := range snapshot.Entries {
		for _, route := range entry.Routes {
			anyRunning = anyRunning || route.Running
		}
	}
	noticeAboutRouter(cctx, observed, serves, anyRunning, snapshot.DockerReachable)

	switch {
	case options.JSON:
		if err := printJSON(cctx, report); err != nil {
			return ListReport{}, err
		}
	case len(report.Entries) == 0:
		// Prose, not data: stdout stays empty, the way `status` answers.
		// `--json` is the scriptable form and has an empty list.
		if all {
			cctx.Logger.Info(EmptyTable(all))
		} else {
			cctx.Logger.Info(fmt.Sprintf("%s Run '%s up' to register this worktree.", EmptyTable(all), cctx.CommandName))
			cctx.Logger.Info(fmt.Sprintf("Every repository: '%s list --all'", cctx.CommandName))
		}
	default:
		for _, line := range FormatTable(report.Entries, all) {
			cctx.Logger.Output(line)
		}
		sayUnknownCauses(cctx, report.Entries, snapshot)
		if cctx.StderrIsTTY {
			listFooters(cctx, report.Entries, all, serves, index.URL())
		}
	}
	// The table is printed whatever happened; Docker not answering still
	// fails the command, as it does `status`, with the cause said once.
	if !snapshot.DockerReachable && snapshot.DockerError != nil {
		return report, clierr.New(worktreestate.DockerHint(*snapshot.DockerError))
	}
	return report, nil
}

// listReport maps a snapshot onto the document, so the output does not follow
// the overview package's internal types. current is the worktree the command
// ran in, if it is a registered one; serves says the router serves this root's
// URLs, which is what lets a running service's address load.
func listReport(snapshot overview.IndexSnapshot, url string, current *registry.WorktreeIdentity, serves bool) ListReport {
	report := ListReport{
		URL: url, GeneratedAt: snapshot.GeneratedAt,
		DockerReachable: snapshot.DockerReachable, DockerError: snapshot.DockerError,
		CardReadFailures: snapshot.CardReadFailures,
		Entries:          make([]ListEntry, 0, len(snapshot.Entries)),
	}
	if failure := snapshot.IdentityError; failure != nil {
		report.IdentityError = &ListIdentityError{Reason: failure.Reason, RowsUnknown: failure.RowsUnknown}
	}
	for _, entry := range snapshot.Entries {
		services := make([]ListService, 0, len(entry.Services))
		checked := snapshot.DockerReachable && entry.State != worktree.StateDirectoryMissing
		for _, service := range entry.Services {
			reported := ListService{Service: service.Name, Label: service.Label, Purpose: service.Purpose, URL: service.URL}
			if checked {
				available := service.Available
				reported.Available = &available
			}
			services = append(services, reported)
		}
		urls := make([]ListURL, 0, len(entry.Routes))
		for _, route := range entry.Routes {
			if serves && route.Running {
				urls = append(urls, ListURL{Service: route.Name, URL: route.URL})
			}
		}
		report.Entries = append(report.Entries, ListEntry{
			RepositoryID: entry.RepositoryID, WorktreeID: entry.WorktreeID, Project: entry.Project,
			Domain: entry.Domain, Slug: entry.Slug, Host: entry.Host, Branch: entry.Branch, Root: entry.Root,
			Current: current != nil && current.RepositoryID == entry.RepositoryID && current.WorktreeID == entry.WorktreeID,
			State:   entry.State, Hint: entry.Hint, URLs: urls, Services: services,
		})
	}
	for _, service := range snapshot.Shared {
		reported := ListShared{
			RepositoryID: service.RepositoryID, Service: service.Name, Project: service.Project,
			Domain: service.Domain, Host: service.Host, URL: service.URL,
		}
		if snapshot.DockerReachable {
			available := service.Available
			reported.Available = &available
		}
		report.Shared = append(report.Shared, reported)
	}
	return report
}

// sayUnknownCauses says on stderr why a row is unknown, unless the cause was
// given once already: Docker's is the error the command ends with, and a
// repository Git could not be asked about has said so in a warning. A cause
// that several rows share (every worktree of a repository that is offline) is
// said once, for the first of them.
func sayUnknownCauses(cctx *CommandContext, entries []ListEntry, snapshot overview.IndexSnapshot) {
	skip := map[string]bool{}
	if snapshot.DockerError != nil {
		skip[worktreestate.DockerHint(*snapshot.DockerError)] = true
	}
	for _, hint := range snapshot.GitHints {
		skip[hint] = true
	}
	var order []string
	hosts := map[string][]string{}
	for _, entry := range entries {
		if entry.Hint == "" || skip[entry.Hint] {
			continue
		}
		if _, seen := hosts[entry.Hint]; !seen {
			order = append(order, entry.Hint)
		}
		hosts[entry.Hint] = append(hosts[entry.Hint], entry.Host)
	}
	for _, hint := range order {
		if others := len(hosts[hint]) - 1; others > 0 {
			cctx.Logger.Info(text.Printable(fmt.Sprintf("%s and %d more are unknown. %s", hosts[hint][0], others, hint)))
			continue
		}
		cctx.Logger.Info(text.Printable(fmt.Sprintf("%s is unknown. %s", hosts[hint][0], hint)))
	}
}

// listFooters are the two lines for a person at a terminal: how many
// directories are gone, and where the rest of the machine is. They are not
// output: a script that captures stderr has no use for them. The second is
// left out when the listing already has every repository, and when the router
// does not serve this root's URLs, since the address would not load.
func listFooters(cctx *CommandContext, entries []ListEntry, all, serves bool, overviewURL string) {
	// A slug names a worktree only within its domain, so a listing of every
	// repository names the host, which names it anywhere.
	var gone []string
	for _, entry := range entries {
		if entry.State == worktree.StateDirectoryMissing {
			name := entry.Slug
			if all {
				name = entry.Host
			}
			gone = append(gone, name)
		}
	}
	// TODO(#211): `remove --gone` lists what it would remove and asks; until
	// it exists, point at the command that removes one worktree.
	switch len(gone) {
	case 0:
	case 1:
		cctx.Logger.Info(text.Printable(fmt.Sprintf("1 directory is gone: '%s worktree remove %s' removes its worktree.", cctx.CommandName, gone[0])))
	default:
		cctx.Logger.Info(fmt.Sprintf("%d directories are gone: '%s worktree remove <slug>' removes one worktree.", len(gone), cctx.CommandName))
	}
	if !all && serves {
		cctx.Logger.Info(fmt.Sprintf("Every repository: '%s list --all', or %s", cctx.CommandName, overviewURL))
	}
}

// EmptyTable is what there is to say when there is no table.
func EmptyTable(all bool) string {
	if all {
		return "No worktrees are registered on this machine."
	}
	return "No worktrees are registered for this repository."
}

// urlCell is the URL column of a worktree: the first address that loads, with
// how many others there are, and "-" when none does.
func urlCell(urls []ListURL) string {
	switch len(urls) {
	case 0:
		return "-"
	case 1:
		return urls[0].URL
	}
	return fmt.Sprintf("%s +%d more", urls[0].URL, len(urls)-1)
}

// FormatTable renders entries the way `list` prints them: a marker column that
// holds `*` for the worktree the command ran in, then WORKTREE, STATE, URL and
// DIRECTORY, with REPOSITORY before them when the listing spans repositories.
// Every cell is printable before its width is measured: a project name or a
// path is whatever a clone put there, and the table is printed to a terminal.
func FormatTable(entries []ListEntry, all bool) []string {
	if len(entries) == 0 {
		return []string{EmptyTable(all)}
	}
	type column struct {
		title string
		value func(ListEntry) string
	}
	var columns []column
	if all {
		columns = append(columns, column{"REPOSITORY", func(e ListEntry) string { return text.Printable(e.Project) }})
	}
	columns = append(columns,
		column{"WORKTREE", func(e ListEntry) string { return text.Printable(e.Slug) }},
		column{"STATE", func(e ListEntry) string { return e.State.String() }},
		column{"URL", func(e ListEntry) string { return text.Printable(urlCell(e.URLs)) }},
		// "-" for nothing recorded, the way URL says it.
		column{"DIRECTORY", func(e ListEntry) string {
			if e.Root == "" {
				return "-"
			}
			return text.Printable(e.Root)
		}},
	)
	widths := make([]int, len(columns))
	for i, col := range columns {
		widths[i] = text.JSLen(col.title)
		for _, entry := range entries {
			widths[i] = max(widths[i], text.JSLen(col.value(entry)))
		}
	}
	line := func(marker string, cells []string) string {
		padded := make([]string, len(cells))
		for i, cell := range cells {
			padded[i] = text.PadEnd(cell, widths[i])
		}
		return strings.TrimRight(marker+"  "+strings.Join(padded, "  "), " ")
	}
	titles := make([]string, len(columns))
	for i, col := range columns {
		titles[i] = col.title
	}
	lines := make([]string, 0, len(entries)+1)
	lines = append(lines, line(" ", titles))
	for _, entry := range entries {
		marker := " "
		if entry.Current {
			marker = "*"
		}
		cells := make([]string, len(columns))
		for i, col := range columns {
			cells[i] = col.value(entry)
		}
		lines = append(lines, line(marker, cells))
	}
	return lines
}
