package commands_test

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/commands/commandstest"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/registry"
)

// What `status` puts on stdout, and what it refuses to put there: the service
// names come from the branch's own compose file, which no schema constrains.
func TestWorktreeStatus(t *testing.T) {
	ctx := context.Background()

	t.Run("will not let a compose file drive the terminal through a service row", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace)
		// A compose file naming a service this way is unusual, not impossible,
		// and nothing between it and the terminal validates it.
		w.Docker.Services = []string{"api\x1b[2K\u202ex"}
		tc := w.Context("", acme, nil)

		if _, err := commands.StatusWithContext(ctx, tc.CommandContext); err != nil {
			t.Fatal(err)
		}
		// The exact row: filtered, still legible, still padded into its column.
		_, printed := tc.Logger.Snapshot()
		if !slices.Contains(printed, "  api�[2K�x  absent") {
			t.Fatalf("printed = %q", printed)
		}
	})

	t.Run("cannot drive the terminal through a domain no schema constrains", func(t *testing.T) {
		// A registry record's domain is deliberately looser than the config's
		// hostname rule, because removal has to find a record it cannot parse.
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace)
		writeRecord(t, w, &registry.WorktreeIdentity{RepositoryID: ref.RepositoryID, WorktreeID: ref.WorktreeID},
			"acme\x1b[2K\u202ex.localhost", "feature-login", ns(brand.LabelNamespace))

		tc := w.Context("", acme, nil)
		if _, err := commands.StatusWithContext(ctx, tc.CommandContext); err != nil {
			t.Fatal(err)
		}
		if _, err := commands.UrlsWithContext(ctx, tc.CommandContext); err != nil {
			t.Fatal(err)
		}
		_, printed := tc.Logger.Snapshot()
		if printed[0] != "feature-login.acme�[2K�x.localhost" {
			t.Fatalf("first line = %q", printed[0])
		}
		if !slices.Contains(printed, "api   http://feature-login.acme�[2K�x.localhost") {
			t.Fatalf("printed = %q", printed)
		}
	})

	// #19, row 25 in docs/port/divergences.md: the base loader accepts a
	// control character in [service.index] label or purpose; only `up`,
	// `config validate` and `doctor` refuse one. `status` only reads an
	// already-running worktree's config, so it loads and proceeds.
	t.Run("proceeds when a service index label or purpose carries a control character", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)
		tc.Loaded.Config.Service[0].Index = &config.ServiceIndex{Label: "a\tb", Purpose: "the \u202eAPI"}

		if _, err := commands.StatusWithContext(ctx, tc.CommandContext); err != nil {
			t.Fatalf("StatusWithContext: %v", err)
		}
	})

	t.Run("says, as a notice, that a worktree never brought up is not registered", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		statuses, err := commands.StatusWithContext(ctx, tc.CommandContext)
		if err != nil || statuses != nil {
			t.Fatalf("statuses = %v, err = %v", statuses, err)
		}
		lines, printed := tc.Logger.Snapshot()
		if len(printed) != 0 || !slices.Contains(lines, "info: Not registered — run 'berth up' to create this worktree's environment.") {
			t.Fatalf("lines = %q, printed = %q", lines, printed)
		}
	})
}

func TestWorktreeDown(t *testing.T) {
	ctx := context.Background()

	t.Run("stops the services under the worktree lock, keeping the data", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)

		if err := commands.DownWithContext(ctx, tc.CommandContext); err != nil {
			t.Fatal(err)
		}
		// The container went; the named volume is data, and survives `down`.
		if left := w.Docker.Owned(ref, brand.LabelNamespace); len(left) != 1 || !strings.HasPrefix(left[0], "v") {
			t.Fatalf("owned after down = %v", left)
		}
		lines, _ := tc.Logger.Snapshot()
		want := []string{"info: Stopping feature-login.acme.localhost.", "info: feature-login.acme.localhost is down."}
		if !slices.Equal(lines, want) {
			t.Fatalf("lines = %q", lines)
		}
		// Released: a second `down` takes the lock again at once.
		if err := commands.DownWithContext(ctx, w.Context("", acme, nil).CommandContext); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("asks for an up first when the worktree is not registered", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		err := commands.DownWithContext(ctx, w.Context("", acme, nil).CommandContext)
		if err == nil || !regexp.MustCompile(regexp.QuoteMeta("This worktree is not registered. Run 'berth up' first.")).MatchString(err.Error()) {
			t.Fatalf("err = %v", err)
		}
	})

	// #19, row 25 in docs/port/divergences.md: `down` only stops what is
	// already running, so — unlike `up` — it loads and proceeds against a
	// [service.index] label or purpose that carries a control character.
	t.Run("proceeds when a service index label or purpose carries a control character", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)
		tc.Loaded.Config.Service[0].Index = &config.ServiceIndex{Label: "a\tb", Purpose: "the \u202eAPI"}

		if err := commands.DownWithContext(ctx, tc.CommandContext); err != nil {
			t.Fatalf("DownWithContext: %v", err)
		}
	})
}
