package commands_test

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/commands/commandstest"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/worktree"
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

		if _, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{}); err != nil {
			t.Fatal(err)
		}
		// The exact row: filtered, still legible, still padded into its column.
		_, printed := tc.Logger.Snapshot()
		if !slices.Contains(printed, "  api�[2K�x  not created") {
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
		if _, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := commands.UrlsWithContext(ctx, tc.CommandContext, commands.OutputOptions{}); err != nil {
			t.Fatal(err)
		}
		_, printed := tc.Logger.Snapshot()
		if printed[0] != "feature-login.acme�[2K�x.localhost   stopped" {
			t.Fatalf("first line = %q", printed[0])
		}
		if !slices.Contains(printed, "api   http://feature-login.acme�[2K�x.localhost") {
			t.Fatalf("printed = %q", printed)
		}
	})

	// #19: the base loader accepts a
	// control character in [service.index] label or purpose; only `up`,
	// `config validate` and `doctor` refuse one. `status` only reads an
	// already-running worktree's config, so it loads and proceeds.
	t.Run("proceeds when a service index label or purpose carries a control character", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)
		tc.Loaded.Config.Service[0].Index = &config.ServiceIndex{Label: "a\tb", Purpose: "the \u202eAPI"}

		if _, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{}); err != nil {
			t.Fatalf("StatusWithContext: %v", err)
		}
	})

	t.Run("says, as a notice, that a worktree never brought up is not registered", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		statuses, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{})
		if err != nil || statuses != nil {
			t.Fatalf("statuses = %v, err = %v", statuses, err)
		}
		lines, printed := tc.Logger.Snapshot()
		if len(printed) != 0 || !slices.Contains(lines, "info: Not registered — run 'treerig up' to create this worktree's environment.") {
			t.Fatalf("lines = %q, printed = %q", lines, printed)
		}
		scan, err := registry.ScanRegistrations(w.ControlHome)
		if err != nil {
			t.Fatalf("ScanRegistrations: %v", err)
		}
		if len(scan.Records) != 0 {
			t.Fatalf("expected no registry records, got %+v", scan.Records)
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
		if err == nil || !regexp.MustCompile(regexp.QuoteMeta("This worktree is not registered. Run 'treerig up' first.")).MatchString(err.Error()) {
			t.Fatalf("err = %v", err)
		}
		scan, err := registry.ScanRegistrations(w.ControlHome)
		if err != nil {
			t.Fatalf("ScanRegistrations: %v", err)
		}
		if len(scan.Records) != 0 {
			t.Fatalf("expected no registry records, got %+v", scan.Records)
		}
	})

	// #19: `down` only stops what is
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

// `status --json` is one document on stdout whether or not the worktree is
// registered; the not-registered notice stays on stderr.
func TestWorktreeStatusJSON(t *testing.T) {
	ctx := context.Background()

	t.Run("prints the host and every service", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace)
		w.Docker.Services = []string{"api"}
		tc := w.Context("", acme, nil)

		if _, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		_, printed := tc.Logger.Snapshot()
		if len(printed) != 1 {
			t.Fatalf("stdout = %q, want one document", printed)
		}
		var got commands.StatusReport
		if err := json.Unmarshal([]byte(printed[0]), &got); err != nil {
			t.Fatalf("stdout is not one JSON document: %v\n%q", err, printed)
		}
		if !got.Registered || got.Host == nil || *got.Host != "feature-login."+acme || got.State == nil || *got.State != worktree.StateStopped {
			t.Fatalf("report = %+v", got)
		}
		var spelled struct {
			State    string `json:"state"`
			Services []struct {
				State string `json:"state"`
			} `json:"services"`
		}
		if err := json.Unmarshal([]byte(printed[0]), &spelled); err != nil || spelled.State != "stopped" || spelled.Services[0].State != "not_created" {
			t.Fatalf("the document spells its states %+v (%v): %s", spelled, err, printed[0])
		}
		if len(got.Services) != 1 || got.Services[0].Service != "api" || got.Services[0].State != worktree.NotCreated {
			t.Fatalf("services = %+v", got.Services)
		}
		var raw struct {
			Services []map[string]any `json:"services"`
		}
		if err := json.Unmarshal([]byte(printed[0]), &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw.Services[0]["service"]; !ok {
			t.Fatalf("a service entry must be keyed \"service\": %v", raw.Services[0])
		}
	})

	// runStatusJSON runs `status --json` for a registered worktree whose
	// `docker compose ps` answers per ps (nil answers as the world does).
	runStatusJSON := func(t *testing.T, services []string, ps func(spec run.Spec) (run.Result, bool)) (*commandstest.TestContext, []commands.StatusService, error) {
		t.Helper()
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace)
		w.Docker.Services = services
		inner := w.Runner.Handler
		w.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
			if ps != nil && spec.Command == "docker" && slices.Contains(spec.Args, "ps") && slices.Contains(spec.Args, "compose") {
				if result, ok := ps(spec); ok {
					return run.Finish(spec, result)
				}
			}
			return inner(ctx, spec)
		}
		tc := w.Context("", acme, nil)
		_, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{JSON: true})
		if err != nil {
			return tc, nil, err
		}
		_, printed := tc.Logger.Snapshot()
		if len(printed) != 1 {
			t.Fatalf("stdout = %q, want one document", printed)
		}
		var got commands.StatusReport
		if err := json.Unmarshal([]byte(printed[0]), &got); err != nil {
			t.Fatal(err)
		}
		return tc, got.Services, nil
	}

	t.Run("reports a state Docker names that treerig does not as unknown, with Docker's word as the detail", func(t *testing.T) {
		_, services, err := runStatusJSON(t, []string{"api"}, func(run.Spec) (run.Result, bool) {
			return run.Result{Stdout: `{"Service":"api","State":"weird","Health":""}`}, true
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(services) != 1 || services[0].State != worktree.Unknown || services[0].Detail != "weird" {
			t.Fatalf("services = %+v", services)
		}
	})

	t.Run("prints an empty services array for a registered worktree that declares none", func(t *testing.T) {
		_, services, err := runStatusJSON(t, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(services) != 0 {
			t.Fatalf("services = %+v", services)
		}
	})

	t.Run("prints the document with an unknown state, and fails, when Compose cannot list the services", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace)
		w.Docker.Services = []string{"api"}
		inner := w.Runner.Handler
		w.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
			if spec.Command == "docker" && slices.Contains(spec.Args, "ps") && slices.Contains(spec.Args, "compose") {
				return run.Finish(spec, run.Result{ExitCode: 1, Stderr: "daemon is down"})
			}
			return inner(ctx, spec)
		}
		tc := w.Context("", acme, nil)
		_, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{JSON: true})
		if err == nil || !strings.Contains(err.Error(), "Compose could not list services") {
			t.Fatalf("err = %v", err)
		}
		_, printed := tc.Logger.Snapshot()
		if len(printed) != 1 {
			t.Fatalf("stdout = %q, want one document", printed)
		}
		var got commands.StatusReport
		if err := json.Unmarshal([]byte(printed[0]), &got); err != nil {
			t.Fatal(err)
		}
		if got.State == nil || *got.State != worktree.StateUnknown || !strings.HasPrefix(got.Hint, "Compose couldn't read this worktree: ") || len(got.Services) != 0 {
			t.Fatalf("report = %+v", got)
		}
	})

	t.Run("escapes a service name that is not ASCII", func(t *testing.T) {
		tc, services, err := runStatusJSON(t, []string{"caf\u00e9"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(services) != 1 || services[0].Service != "caf\u00e9" {
			t.Fatalf("services = %+v", services)
		}
		if _, printed := tc.Logger.Snapshot(); !strings.Contains(printed[0], `caf\u00e9`) || strings.ContainsRune(printed[0], '\u00e9') {
			t.Fatalf("stdout = %q", printed)
		}
	})

	t.Run("prints an unregistered report and keeps the notice on stderr", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)

		if _, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		lines, printed := tc.Logger.Snapshot()
		if got := strings.Join(printed, "\n"); got != "{\n  \"registered\": false,\n  \"services\": []\n}" {
			t.Fatalf("stdout = %q", got)
		}
		if !slices.Contains(lines, "info: Not registered — run 'treerig up' to create this worktree's environment.") {
			t.Fatalf("lines = %q", lines)
		}
	})
}
