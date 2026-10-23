package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/checkoutindex/card"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/text"
	"github.com/sofired/treerig/internal/worktree"
	"github.com/sofired/treerig/internal/worktreestate"
)

// statusSubject is the worktree a status reports on, with a context whose
// Loaded is its own repository's treerig.toml.
type statusSubject struct {
	cctx   *CommandContext
	record registry.RegisteredWorktree
	// root is the directory the worktree was last seen in, "" when unknown.
	root string
	// here is true when the command runs in the worktree itself, so its
	// directory is known to be there.
	here bool
}

// resolveStatusSubject finds the worktree to report on: the one the command
// runs in, or the one options.Worktree names. It answers nil, nil for the
// current worktree when it is not registered.
func resolveStatusSubject(ctx context.Context, cctx *CommandContext, options StatusOptions) (*statusSubject, error) {
	if options.Worktree == "" {
		registration, err := cctx.Registry.Current(ctx)
		if err != nil || registration == nil {
			return nil, err
		}
		return &statusSubject{cctx: cctx, record: registration.RegisteredWorktree, root: registration.Root, here: true}, nil
	}
	// Read-only, so a path inside a worktree's directory names it too.
	resolved, err := cctx.Registry.Resolve(ctx, options.Worktree, cctx.nameOptions("", nil, true))
	if err != nil {
		return nil, err
	}
	return &statusSubject{cctx: cctx, record: resolved.Record, root: resolved.Root}, nil
}

// statusStop is why status cannot read a named worktree's services, and what it
// says then: directory is what the state is classified with, unknown the cause
// of the unknown state it is then in ("" when the directory decides), and note
// what stderr says.
type statusStop struct {
	directory worktree.Directory
	unknown   string
	note      string
}

// checkDirectory looks at the directory of a worktree the command does not run
// in, and loads the treerig.toml it holds into s.cctx. A nil stop means the
// services can be read. It fails when the directory is there and its
// treerig.toml, or the Compose file beside it, is not a file treerig will read.
//
// The directory is judged by the evidence the listing uses
// (worktreestate.Gatherer.RecordedDirectory), and the treerig.toml is read only
// from a directory that Git, when it can say, puts in the worktree's own
// repository: a directory a card recorded and something else has since taken
// is not read. The filesystem calls here take no context, so a hung mount holds
// them; the Git question does.
func (s *statusSubject) checkDirectory(ctx context.Context, gatherer *worktreestate.Gatherer, host string) (*statusStop, error) {
	cctx := s.cctx
	if s.root == "" {
		return &statusStop{
			directory: worktree.DirectoryPresent,
			unknown:   "treerig has no record of where its directory is.",
			note: fmt.Sprintf("treerig has no record of where %s lives, so its services can't be listed. Run '%s up' in its directory.",
				host, cctx.CommandName),
		}, nil
	}
	switch directory, hint := gatherer.RecordedDirectory(s.root); directory {
	case worktree.DirectoryGone:
		return &statusStop{
			directory: worktree.DirectoryGone,
			note: fmt.Sprintf("The directory of %s is missing, so its services can't be listed. If a drive is not connected, connect it. "+
				"Otherwise run '%s worktree remove %s'. Directory: %s", host, cctx.CommandName, host, s.root),
		}, nil
	case worktree.DirectoryUndetermined:
		return &statusStop{
			directory: worktree.DirectoryPresent,
			unknown:   hint,
			note:      fmt.Sprintf("%s is unknown. %s Directory: %s", host, hint, s.root),
		}, nil
	}
	if cctx.Loaded != nil && sameDirectory(s.root, cctx.Loaded.Root) {
		return nil, nil
	}
	// Another repository's worktree, or this one with a treerig.toml that was
	// set aside: its own directory says what it declares, provided it is still
	// the worktree's.
	if id, present, inside, err := registry.NewIdentityService(s.root, cctx.Runner).Repository(ctx, s.root); err == nil && inside && present && id != s.record.RepositoryID {
		reason := "The directory recorded for it now belongs to another repository."
		return &statusStop{
			directory: worktree.DirectoryPresent,
			unknown:   reason,
			note:      fmt.Sprintf("%s is unknown, and its services can't be listed. %s Directory: %s", host, reason, s.root),
		}, nil
	}
	loaded, err := config.Load(filepath.Join(s.root, brand.ConfigFile))
	if err != nil {
		return nil, err
	}
	// Compose reads the Compose file, not treerig, so it is looked at before it
	// is handed over: a missing one is Compose's to report.
	if err := config.CheckRegularFile(composeFilePath(loaded), config.MaxFileSize); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	derived := *cctx
	derived.Loaded = loaded
	s.cctx = &derived
	return nil, nil
}

// sameDirectory reports whether a and b are one directory, by file identity,
// so a second spelling of it (a symbolic link, a case-insensitive filesystem)
// is the same.
func sameDirectory(a, b string) bool {
	first, err := os.Stat(a)
	if err != nil {
		return false
	}
	second, err := os.Stat(b)
	return err == nil && os.SameFile(first, second)
}

// printStoppedStatus prints the header (or the JSON document) of a worktree
// whose services status cannot list, and says why on stderr. It is a report,
// not a failure: status exits 0 for any state.
func printStoppedStatus(cctx *CommandContext, options StatusOptions, host string, result worktreestate.Result, note string) error {
	if note != "" {
		cctx.Logger.Info(text.Printable(note))
	}
	if options.JSON {
		return printJSON(cctx, StatusReport{Registered: true, Host: &host, State: &result.State, Hint: result.Hint, Services: []StatusService{}})
	}
	cctx.Logger.Output(text.Printable(fmt.Sprintf("%s   %s", host, result.State)))
	return nil
}

// unregisteredName is what a worktree that is not registered would be called:
// the slug `up` would give it from its branch, or from its directory's name
// when Git has not been asked about a branch.
func unregisteredName(ctx context.Context, cctx *CommandContext) string {
	identity, err := cctx.Registry.Identities.Inspect(ctx, cctx.Registry.Identities.Root, registry.InspectOptions{})
	if err == nil && identity != nil {
		return registry.BranchSlug(identity.Branch, identity.Root)
	}
	return registry.BranchSlug("", cctx.Registry.Identities.Root)
}

// recordedOr is the label namespace the worktree's resources carry: the
// registration's, then the index card's (card.RecordedLabelNamespace), and
// fallback, the treerig.toml's, when neither recorded one. The order `remove`
// reads them in.
func recordedOr(controlHome string, record registry.RegisteredWorktree, fallback string) string {
	recorded := card.RecordedLabelNamespace(controlHome, card.Registration{
		RepositoryID: record.RepositoryID, WorktreeID: record.WorktreeID, LabelNamespace: record.LabelNamespace,
	})
	if recorded != nil {
		return *recorded
	}
	return fallback
}
