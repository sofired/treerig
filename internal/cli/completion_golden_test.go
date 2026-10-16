package cli_test

// Golden corpus for shell completion of registered values: worktree slugs,
// domains, `up`'s service names (#98) and `run`'s command names (#94). These share TestGolden's own
// fixture format and update mechanism (testdata/golden/<name>/{stdout,
// stderr,exit}, rewritten by `-update`), but need a registry and a
// treerig.toml on disk that the plain, purely-literal goldenCases table
// cannot build for itself — see newCompletionFixture below — so they run
// through their own TestGoldenCompletion instead. Every case's args name
// the fixture's own --config file explicitly, so none of them depend on
// this test binary's working directory.
//
// The fixture is built once, from fixed, hand-picked UUIDs, slugs, domains
// and paths — never from real Git or a generated identity — so its output
// is exactly as deterministic as the rest of this package's goldens.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/checkoutindex/card"
	"github.com/sofired/treerig/internal/cli"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
)

// completionFixture is one hermetic, read-only world for the completion
// goldens: a treerig.toml with two declared services, and a control home
// with three registered worktrees across two domains (one with no
// checkout-index card yet) plus one recoverable-but-unregistered worktree
// lock.
type completionFixture struct {
	// configPath is the fixture's own treerig.toml, passed to every case as
	// --config so none of them depend on this test binary's cwd.
	configPath string
	// noCommandsConfigPath is a treerig.toml with services but no [commands].
	noCommandsConfigPath string
	// controlHome is TREERIG_CONTROL_HOME for the populated registry cases.
	controlHome string
	// missingControlHome names a control home nothing ever created.
	missingControlHome string
	// emptyControlHome exists, with no repository registered under it.
	emptyControlHome string
	// unreadableControlHome exists, but its own repositories directory is
	// a plain file rather than a directory, so scanning it fails.
	unreadableControlHome string
}

// Fixed, valid-shaped identifiers for the completion fixture: every one
// matches the registry's own UUID pattern (a version nibble of 1-8, a
// variant nibble of 8/9/a/b), and every slug the registry's own slug
// pattern, chosen once, by hand, so the goldens below never depend on a
// generated identity.
const (
	completionRepoAcme1      = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	completionWorktreeA      = "11111111-1111-4111-8111-111111111111"
	completionWorktreeB      = "22222222-2222-4222-8222-222222222222"
	completionRepoAcme2      = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	completionWorktreeC      = "33333333-3333-4333-8333-333333333333"
	completionOrphanRepo     = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	completionOrphanWorktree = "44444444-4444-4444-8444-444444444444"
	completionRepoOdd        = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	completionWorktreeOdd    = "55555555-5555-4555-8555-555555555555"

	completionAcmeDomain    = "acme.localhost"
	completionWidgetsDomain = "widgets.localhost"
	// completionOddDomain carries a raw tab: neither the registry record's
	// Domain nor the checkout-index card's Root is revalidated on read (see
	// describeWorktreeCandidate), so a record written by an older build, or
	// a branch name that made it into a path, could carry one — and an
	// unneutralised tab or newline in a completion's value or description
	// breaks cobra's line-based, tab-delimited value/description protocol.
	completionOddDomain = "odd\t.localhost"
	completionOddRoot   = "/checkouts/odd\nworktree"
)

// newCompletionFixture builds completionFixture under one fresh, physical
// temp directory (registrytest.TempDir, so its path survives a comparison
// on macOS): a treerig.toml declaring four services — "api" (no subdomain,
// path or label, so it completes as "host root"), "web" (a subdomain),
// "assets" (a path only) and "docs" (a [service.index] label carrying a
// raw tab and a raw newline, pinning serviceCompletionDescription's own
// text.Printable call — "docs" also has its own path, only so its route
// does not collide with "api"'s host root; the label wins the description
// regardless) — so all four of serviceCompletionDescription's branches are
// exercised, a control home with completionWorktreeA and
// completionWorktreeB registered under
// completionAcmeDomain (only A has a checkout-index card, so B completes
// with no known path) and completionWorktreeC registered under
// completionWidgetsDomain, plus a lock file for completionOrphanWorktree
// under completionOrphanRepo with no registration at all — and the three
// broken-registry variants the "missing/empty/unreadable" goldens need.
func newCompletionFixture(t *testing.T) completionFixture {
	t.Helper()
	base := registrytest.TempDir(t)

	checkout := filepath.Join(base, "checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatalf("mkdir checkout: %v", err)
	}
	configPath := filepath.Join(checkout, brand.ConfigFile)
	const treerigToml = `[project]
name = "fixture"
domain = "` + completionAcmeDomain + `"

[compose]
file = "compose.yaml"

[service.api]

[service.web]
subdomain = "web"

[service.assets]
path = "/latest"

[service.docs]
path = "/docs"
[service.docs.index]
label = "Docs\tGuide\nSection"

[shared]
file = "compose.shared.yaml"
[shared.service.mailpit]
used-by = ["api", "web"]
[shared.service.objects]
[shared.service.queue]
used-by = ["api"]

[commands]
plan = "./scripts/keycloak.sh plan"
destroy = "./scripts/keycloak.sh\tdestroy"

[commands.apply]
command = "./scripts/keycloak.sh apply --with-an-argument-list-long-enough-to-be-clamped"
kill-after = "60s"
`
	if err := os.WriteFile(configPath, []byte(treerigToml), 0o644); err != nil {
		t.Fatalf("write treerig.toml: %v", err)
	}

	bare := filepath.Join(base, "bare")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatalf("mkdir bare checkout: %v", err)
	}
	noCommandsConfigPath := filepath.Join(bare, brand.ConfigFile)
	const bareToml = `[project]
name = "fixture"
domain = "` + completionAcmeDomain + `"

[compose]
file = "compose.yaml"

[service.api]
`
	if err := os.WriteFile(noCommandsConfigPath, []byte(bareToml), 0o644); err != nil {
		t.Fatalf("write bare treerig.toml: %v", err)
	}

	controlHome := filepath.Join(base, "control")
	writeCompletionRecord(t, controlHome, completionRepoAcme1, completionWorktreeA, "feature-login", completionAcmeDomain)
	writeCompletionCard(t, controlHome, completionRepoAcme1, completionWorktreeA, "feature-login", completionAcmeDomain, "/checkouts/feature-login")
	writeCompletionRecord(t, controlHome, completionRepoAcme1, completionWorktreeB, "feature-payments", completionAcmeDomain)
	// completionWorktreeB gets no card: its own row completes with no known path.
	writeCompletionRecord(t, controlHome, completionRepoAcme2, completionWorktreeC, "widgets-main", completionWidgetsDomain)
	writeCompletionCard(t, controlHome, completionRepoAcme2, completionWorktreeC, "widgets-main", completionWidgetsDomain, "/checkouts/widgets-main")
	// completionOddDomain/completionOddRoot pin describeWorktreeCandidate's
	// and domainCompletionFunc's own text.Printable calls: written straight
	// to disk (registrytest.WriteRecord/card.WriteIndexCard bypass the
	// schema checks a real `up` would apply to a domain or a path), the
	// same way a pre-existing record from an older build could carry one.
	writeCompletionRecord(t, controlHome, completionRepoOdd, completionWorktreeOdd, "odd-worktree", completionOddDomain)
	writeCompletionCard(t, controlHome, completionRepoOdd, completionWorktreeOdd, "odd-worktree", completionOddDomain, completionOddRoot)
	writeCompletionOrphanLock(t, controlHome, completionOrphanRepo, completionOrphanWorktree)

	missingControlHome := filepath.Join(base, "missing-control")

	emptyControlHome := filepath.Join(base, "empty-control")
	if err := os.MkdirAll(layout.ControlLayout(emptyControlHome).RepositoriesDir, 0o755); err != nil {
		t.Fatalf("mkdir empty control home: %v", err)
	}

	unreadableControlHome := filepath.Join(base, "unreadable-control")
	if err := os.MkdirAll(unreadableControlHome, 0o755); err != nil {
		t.Fatalf("mkdir unreadable control home: %v", err)
	}
	// A plain file where ScanRegistrations expects a directory: os.ReadDir
	// fails with "not a directory" regardless of who runs this suite,
	// deterministically simulating an unreadable registry without relying
	// on a permission bit a root-run suite would ignore.
	repositories := layout.ControlLayout(unreadableControlHome).RepositoriesDir
	if err := os.WriteFile(repositories, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write unreadable registry stand-in: %v", err)
	}

	return completionFixture{
		configPath:            configPath,
		noCommandsConfigPath:  noCommandsConfigPath,
		controlHome:           controlHome,
		missingControlHome:    missingControlHome,
		emptyControlHome:      emptyControlHome,
		unreadableControlHome: unreadableControlHome,
	}
}

// writeCompletionRecord writes one fixture registry record directly to
// disk (registrytest.WriteRecord), bypassing the registry lock the same
// way every other registry fixture in this build does.
func writeCompletionRecord(t *testing.T, controlHome, repositoryID, worktreeID, slug, domain string) {
	t.Helper()
	_, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
		Version: 1, RepositoryID: repositoryID, WorktreeID: worktreeID,
		Slug: slug, Domain: domain, Initialization: string(registry.Complete),
	})
	if err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
}

// writeCompletionCard writes one fixture checkout-index card, creating its
// worktree home directory first the way `up` would have.
func writeCompletionCard(t *testing.T, controlHome, repositoryID, worktreeID, slug, domain, root string) {
	t.Helper()
	home := layout.WorktreeLayout(controlHome, repositoryID, worktreeID).Home
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir worktree home: %v", err)
	}
	host := layout.WorktreeHost(slug, domain)
	err := card.WriteIndexCard(controlHome, repositoryID, worktreeID, card.IndexCard{
		Project: "fixture", Domain: domain, Slug: slug, Host: host,
		Root: root, Branch: "main", LabelNamespace: brand.LabelNamespace,
		UpdatedAt: "2026-01-01T00:00:00.000Z",
	})
	if err != nil {
		t.Fatalf("WriteIndexCard: %v", err)
	}
}

// writeCompletionOrphanLock leaves a bare worktree-operation lock file
// under repositoryID/worktreeID with no registration behind it: what a
// worktree whose first `up` died before it registered leaves in place,
// the same shape findOrphanWorktree/OrphanWorktrees scan for. Its
// contents are never read by that scan, only its name.
func writeCompletionOrphanLock(t *testing.T, controlHome, repositoryID, worktreeID string) {
	t.Helper()
	dir := filepath.Join(layout.ControlLayout(controlHome).LocksDir, "worktrees", repositoryID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir orphan lock dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, worktreeID), nil, 0o644); err != nil {
		t.Fatalf("write orphan lock file: %v", err)
	}
}

// getenvWithControlHome is emptyGetenv with TREERIG_CONTROL_HOME overridden:
// every other lookup (HOME, TREERIG_HOME, TREERIG_DEBUG) stays exactly as
// unset as every other golden case in this package sees it, so checkedHome
// resolves TREERIG_HOME the same harmless default way it always does here,
// and only the control home this completion actually reads is redirected
// to the fixture.
func getenvWithControlHome(controlHome string) layout.Getenv {
	return func(key string) (string, bool) {
		if key == brand.ControlHomeEnv {
			return controlHome, true
		}
		return emptyGetenv(key)
	}
}

// completionGoldenCase is one TestGoldenCompletion case: args, and the
// control home TREERIG_CONTROL_HOME should resolve to while cli.Main runs
// it — completionFixture's populated one unless the case says otherwise.
type completionGoldenCase struct {
	name        string
	args        []string
	controlHome func(completionFixture) string
}

// populated is every completionGoldenCase's default controlHome selector:
// the fixture's one populated, readable control home.
func populated(f completionFixture) string { return f.controlHome }

// TestGoldenCompletion replays every completionGoldenCases entry against
// cli.Main, over one shared completionFixture, and compares its stdout,
// stderr and exit code byte for byte against the committed fixture under
// testdata/golden/<name>/ — see TestGolden's own doc comment for the
// -update flag this shares.
func TestGoldenCompletion(t *testing.T) {
	fixture := newCompletionFixture(t)

	cases := []completionGoldenCase{
		{
			// Shared services in file order, each described by what uses it.
			name: "complete-shared-up-names",
			args: []string{"__complete", "shared", "up", "--config", fixture.configPath, ""},
		},
		{
			name: "complete-shared-up-names-prefix-and-given",
			args: []string{"__complete", "shared", "up", "--config", fixture.configPath, "mailpit", "q"},
		},
		{
			name: "complete-shared-down-names",
			args: []string{"__complete", "shared", "down", "--config", fixture.configPath, ""},
		},
		{
			// A treerig.toml without [shared] has nothing to offer.
			name: "complete-shared-up-none-declared",
			args: []string{"__complete", "shared", "up", "--config", fixture.noCommandsConfigPath, ""},
		},
		{
			name: "complete-worktree-remove-identifier",
			args: []string{"__complete", "worktree", "remove", "--config", fixture.configPath, ""},
		},
		{
			name: "complete-worktree-remove-identifier-domain",
			args: []string{"__complete", "worktree", "remove", "--config", fixture.configPath, "--domain", completionAcmeDomain, ""},
		},
		{
			name: "complete-worktree-remove-identifier-extra-arg",
			args: []string{"__complete", "worktree", "remove", "--config", fixture.configPath, "feature-login", ""},
		},
		{
			// Pins describeWorktreeCandidate's text.Printable calls: narrowed
			// to completionOddDomain (its own raw tab, matched exactly
			// against the record's own unvalidated Domain) so the golden
			// shows only the one candidate whose description would
			// otherwise embed a second, protocol-breaking tab and a stray
			// completion line.
			name: "complete-worktree-remove-identifier-odd-characters",
			args: []string{"__complete", "worktree", "remove", "--config", fixture.configPath, "--domain", completionOddDomain, ""},
		},
		{
			name: "complete-worktree-remove-domain-flag",
			args: []string{"__complete", "worktree", "remove", "--config", fixture.configPath, "--domain", ""},
		},
		{
			name:        "complete-worktree-remove-registry-missing",
			args:        []string{"__complete", "worktree", "remove", "--config", fixture.configPath, ""},
			controlHome: func(f completionFixture) string { return f.missingControlHome },
		},
		{
			name:        "complete-worktree-remove-registry-empty",
			args:        []string{"__complete", "worktree", "remove", "--config", fixture.configPath, ""},
			controlHome: func(f completionFixture) string { return f.emptyControlHome },
		},
		{
			name:        "complete-worktree-remove-registry-unreadable",
			args:        []string{"__complete", "worktree", "remove", "--config", fixture.configPath, ""},
			controlHome: func(f completionFixture) string { return f.unreadableControlHome },
		},
		{
			name: "complete-worktree-unlock-identifier",
			args: []string{"__complete", "worktree", "unlock", "--config", fixture.configPath, ""},
		},
		{
			name: "complete-worktree-unlock-identifier-domain",
			args: []string{"__complete", "worktree", "unlock", "--config", fixture.configPath, "--domain", completionWidgetsDomain, ""},
		},
		{
			name: "complete-worktree-unlock-domain-flag",
			args: []string{"__complete", "worktree", "unlock", "--config", fixture.configPath, "--domain", ""},
		},
		{
			name:        "complete-worktree-unlock-registry-missing",
			args:        []string{"__complete", "worktree", "unlock", "--config", fixture.configPath, ""},
			controlHome: func(f completionFixture) string { return f.missingControlHome },
		},
		{
			name: "complete-up-services",
			args: []string{"__complete", "up", "--config", fixture.configPath, ""},
		},
		{
			name: "complete-up-services-excludes-given",
			args: []string{"__complete", "up", "--config", fixture.configPath, "api", ""},
		},
		{
			name: "complete-up-services-prefix",
			args: []string{"__complete", "up", "--config", fixture.configPath, "w"},
		},
		// `run`'s [commands] (#94): file order, not sorted (plan, destroy,
		// apply), each described by its command line clamped to one line —
		// destroy's holds a raw tab, apply's is longer than the clamp.
		{
			name: "complete-run-commands",
			args: []string{"__complete", "run", "--config", fixture.configPath, ""},
		},
		{
			name: "complete-run-commands-prefix",
			args: []string{"__complete", "run", "--config", fixture.configPath, "a"},
		},
		// `run` with no name or an unknown one lists the commands the
		// config defines, or says it defines none; nothing is registered or
		// started, so these need only the config.
		{
			name: "run-no-name",
			args: []string{"run", "--config", fixture.configPath},
		},
		{
			name: "run-unknown-name",
			args: []string{"run", "--config", fixture.configPath, "nope"},
		},
		{
			name: "run-no-name-after-dash",
			args: []string{"run", "--config", fixture.configPath, "--", "plan"},
		},
		{
			name: "run-no-name-no-commands",
			args: []string{"run", "--config", fixture.noCommandsConfigPath},
		},
		{
			name: "run-unknown-name-no-commands",
			args: []string{"run", "--config", fixture.noCommandsConfigPath, "nope"},
		},
		{
			name: "complete-run-commands-none-configured",
			args: []string{"__complete", "run", "--config", fixture.noCommandsConfigPath, ""},
		},
		{
			name: "complete-run-commands-after-name",
			args: []string{"__complete", "run", "--config", fixture.configPath, "plan", ""},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			controlHome := populated
			if tc.controlHome != nil {
				controlHome = tc.controlHome
			}
			getenv := getenvWithControlHome(controlHome(fixture))

			dir := filepath.Join("testdata", "golden", tc.name)
			var stdout, stderr bytes.Buffer
			args := append([]string{"treerig"}, tc.args...)
			code := cli.Main(args, strings.NewReader(""), &stdout, &stderr, getenv)

			if *update {
				writeFixture(t, dir, stdout.String(), stderr.String(), code)
				return
			}

			wantOut := readFixtureFile(t, filepath.Join(dir, "stdout"))
			wantErr := readFixtureFile(t, filepath.Join(dir, "stderr"))
			wantCode := readFixtureCode(t, filepath.Join(dir, "exit"))

			if stdout.String() != wantOut {
				t.Errorf("stdout mismatch\n--- want ---\n%s\n--- got ---\n%s", wantOut, stdout.String())
			}
			if stderr.String() != wantErr {
				t.Errorf("stderr mismatch\n--- want ---\n%s\n--- got ---\n%s", wantErr, stderr.String())
			}
			if code != wantCode {
				t.Errorf("exit code = %d, want %d", code, wantCode)
			}
		})
	}
}
