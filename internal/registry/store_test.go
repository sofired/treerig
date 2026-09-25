package registry_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/registry/registrytest"
)

const (
	repoA = "a08b4892-5111-49f1-a47a-5ab988c7f676"
	wtA   = "2b41d9d4-ed88-49c1-be9c-5439bdf6a431"
	repoB = "b1c2d3e4-1111-4222-8333-444455556666"
	wtB   = "c2d3e4f5-2222-4333-8444-555566667777"
)

// writeMinimalRecord writes a version-1 fixture record under controlHome,
// failing the test if the write fails.
func writeMinimalRecord(t *testing.T, controlHome, repositoryID, worktreeID, slug, domain, initialization string) {
	t.Helper()
	if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
		Version: 1, RepositoryID: repositoryID, WorktreeID: worktreeID,
		Slug: slug, Domain: domain, Initialization: initialization,
	}); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
}

func TestStoreFindAndList(t *testing.T) {
	t.Run("finds a written record and reports its initialization state", func(t *testing.T) {
		controlHome := t.TempDir()
		writeMinimalRecord(t, controlHome, repoA, wtA, "feature-login", acmeDomain, "pending")
		store := registry.NewStore(controlHome, repoA)

		record, err := store.Find(wtA)
		if err != nil {
			t.Fatalf("Find: %v", err)
		}
		if record == nil || record.Slug != "feature-login" || record.Domain != acmeDomain {
			t.Fatalf("unexpected record: %+v", record)
		}
		state, err := store.Initialization(wtA, "feature-login")
		if err != nil || state != registry.Pending {
			t.Fatalf("Initialization: %v, %v", state, err)
		}
	})

	t.Run("answers nil, nil for a record that does not exist", func(t *testing.T) {
		store := registry.NewStore(t.TempDir(), repoA)
		record, err := store.Find(wtA)
		if err != nil || record != nil {
			t.Fatalf("expected (nil, nil), got (%+v, %v)", record, err)
		}
	})

	t.Run("initialization refuses a record the caller no longer owns", func(t *testing.T) {
		controlHome := t.TempDir()
		writeMinimalRecord(t, controlHome, repoA, wtA, "feature-login", acmeDomain, "complete")
		store := registry.NewStore(controlHome, repoA)
		_, err := store.Initialization(wtA, "renamed-since")
		requireErrorContainsRegistry(t, err, "no longer owns registration")
	})

	t.Run("lists records sorted by slug", func(t *testing.T) {
		controlHome := t.TempDir()
		writeMinimalRecord(t, controlHome, repoA, wtA, "zebra", acmeDomain, "complete")
		writeMinimalRecord(t, controlHome, repoA, wtB, "alpha", acmeDomain, "complete")
		store := registry.NewStore(controlHome, repoA)

		list, err := store.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 2 || list[0].Slug != "alpha" || list[1].Slug != "zebra" {
			t.Fatalf("unexpected order: %+v", list)
		}
	})

	t.Run("records the label namespace when present, and omits it when absent", func(t *testing.T) {
		controlHome := t.TempDir()
		namespace := "dev.predecessor"
		if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
			Version: 1, RepositoryID: repoA, WorktreeID: wtA, Slug: "feature-login", Domain: acmeDomain,
			LabelNamespace: &namespace, Initialization: "complete",
		}); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
		writeMinimalRecord(t, controlHome, repoA, wtB, "other", acmeDomain, "complete")
		store := registry.NewStore(controlHome, repoA)

		withNamespace, err := store.Find(wtA)
		if err != nil || withNamespace == nil || withNamespace.LabelNamespace == nil || *withNamespace.LabelNamespace != namespace {
			t.Fatalf("expected labelNamespace to round-trip, got %+v, %v", withNamespace, err)
		}
		without, err := store.Find(wtB)
		if err != nil || without == nil || without.LabelNamespace != nil {
			t.Fatalf("expected no labelNamespace, got %+v, %v", without, err)
		}
	})
}

func TestStoreScanProblems(t *testing.T) {
	t.Run("a directory that skips non-UUID and non-.json entries", func(t *testing.T) {
		controlHome := t.TempDir()
		writeMinimalRecord(t, controlHome, repoA, wtA, "feature-login", acmeDomain, "complete")
		dir := registry.RegistryDirectory(controlHome, repoA)
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("not a record"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "subdir.json"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		records, problems, err := registry.NewStore(controlHome, repoA).Scan()
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(records) != 1 || len(problems) != 0 {
			t.Fatalf("expected exactly the one real record and no problems, got records=%+v problems=%+v", records, problems)
		}
	})

	t.Run("reports a file not named by a worktree UUID", func(t *testing.T) {
		controlHome := t.TempDir()
		dir := registry.RegistryDirectory(controlHome, repoA)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "not-a-uuid.json"), []byte("{}"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		_, problems, err := registry.NewStore(controlHome, repoA).Scan()
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(problems) != 1 || !contains(problems[0].Reason, "is not named by a worktree UUID") {
			t.Fatalf("unexpected problems: %+v", problems)
		}
	})

	t.Run("reports a symlinked record instead of following it", func(t *testing.T) {
		controlHome := t.TempDir()
		elsewhere := t.TempDir()
		target := filepath.Join(elsewhere, "elsewhere.json")
		if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		dir := registry.RegistryDirectory(controlHome, repoA)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Symlink(target, filepath.Join(dir, wtA+".json")); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		_, problems, err := registry.NewStore(controlHome, repoA).Scan()
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(problems) != 1 || !contains(problems[0].Reason, "not a symbolic link") {
			t.Fatalf("unexpected problems: %+v", problems)
		}
	})

	t.Run("reports invalid JSON, and a well-formed shape that fails validation, distinctly", func(t *testing.T) {
		controlHome := t.TempDir()
		dir := registry.RegistryDirectory(controlHome, repoA)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, wtA+".json"), []byte("{ not json"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		_, problems, err := registry.NewStore(controlHome, repoA).Scan()
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(problems) != 1 || !contains(problems[0].Reason, "not valid JSON") {
			t.Fatalf("unexpected problems: %+v", problems)
		}

		// An unknown key is `.strict()`'s job in the TypeScript schema; here
		// DisallowUnknownFields plays the same role.
		if err := os.WriteFile(filepath.Join(dir, wtB+".json"), []byte(`{"version":1,"repositoryId":"`+repoA+`","worktreeId":"`+wtB+`","slug":"x","domain":"d","initialization":"complete","extra":true}`), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, problems, err = registry.NewStore(controlHome, repoA).Scan()
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		var shapeProblems int
		for _, p := range problems {
			if contains(p.Reason, "unrecognized version or shape") {
				shapeProblems++
			}
		}
		if shapeProblems != 1 {
			t.Fatalf("expected exactly one shape problem, got %+v", problems)
		}
	})

	t.Run("reports identity that does not match its path", func(t *testing.T) {
		controlHome := t.TempDir()
		dir := registry.RegistryDirectory(controlHome, repoA)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// Filed under wtA, but the record itself claims wtB.
		if err := os.WriteFile(filepath.Join(dir, wtA+".json"), []byte(`{"version":1,"repositoryId":"`+repoA+`","worktreeId":"`+wtB+`","slug":"x","domain":"d","initialization":"complete"}`), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		_, problems, err := registry.NewStore(controlHome, repoA).Scan()
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(problems) != 1 || !contains(problems[0].Reason, "identity does not match its path") {
			t.Fatalf("unexpected problems: %+v", problems)
		}
	})
}

func TestScanRegistrationsConflictsAndDomains(t *testing.T) {
	t.Run("reports a duplicate hostname found while scanning without blocking unrelated repositories", func(t *testing.T) {
		controlHome := t.TempDir()
		writeMinimalRecord(t, controlHome, repoA, wtA, "feature-shared", acmeDomain, "complete")
		writeMinimalRecord(t, controlHome, repoB, wtB, "feature-shared", acmeDomain, "complete")

		scan, err := registry.ScanRegistrations(controlHome)
		if err != nil {
			t.Fatalf("ScanRegistrations: %v", err)
		}
		host := "feature-shared." + acmeDomain
		if claimants, ok := scan.Conflicts[host]; !ok || len(claimants) != 2 {
			t.Fatalf("expected a conflict at %s, got %+v", host, scan.Conflicts)
		}
		if len(scan.Records) != 2 {
			t.Fatalf("both conflicting records are still reported: %+v", scan.Records)
		}
	})

	t.Run("tolerates the same slug under two domains", func(t *testing.T) {
		controlHome := t.TempDir()
		const widgetsDomain = "widgets.localhost"
		writeMinimalRecord(t, controlHome, repoA, wtA, "feature-shared", acmeDomain, "complete")
		writeMinimalRecord(t, controlHome, repoB, wtB, "feature-shared", widgetsDomain, "complete")

		scan, err := registry.ScanRegistrations(controlHome)
		if err != nil {
			t.Fatalf("ScanRegistrations: %v", err)
		}
		if len(scan.Conflicts) != 0 {
			t.Fatalf("expected no conflicts, got %+v", scan.Conflicts)
		}
		if len(scan.Records) != 2 {
			t.Fatalf("expected both records, got %+v", scan.Records)
		}
	})

	t.Run("an absent repositories directory scans as empty", func(t *testing.T) {
		scan, err := registry.ScanRegistrations(filepath.Join(t.TempDir(), "does-not-exist"))
		if err != nil {
			t.Fatalf("ScanRegistrations: %v", err)
		}
		if len(scan.Records) != 0 || len(scan.Problems) != 0 || len(scan.Conflicts) != 0 {
			t.Fatalf("expected an empty scan, got %+v", scan)
		}
	})
}

// TestListSortMatchesLocale documents the accepted difference: Store.List
// and the index sort by plain Go string comparison (sort.Strings) rather
// than `localeCompare(…, 'en')`. testdata/locale-sort.json, generated by
// tools/oracle/locale-sort.ts, is the TypeScript build's own
// localeCompare('en') order for a corpus of slug- and domain-shaped strings
// ([a-z0-9.-], the only alphabet Store.List or the checkout index ever sorts
// within); this test checks Go's byte order reproduces it exactly, so no
// collation package (x/text/collate) is needed for this schema.
func TestListSortMatchesLocale(t *testing.T) {
	data, err := os.ReadFile("testdata/locale-sort.json")
	if err != nil {
		t.Fatalf("reading oracle data: %v", err)
	}
	var oracle struct {
		Input  []string `json:"input"`
		Sorted []string `json:"sorted"`
	}
	if err := json.Unmarshal(data, &oracle); err != nil {
		t.Fatalf("parsing oracle data: %v", err)
	}
	if len(oracle.Input) == 0 {
		t.Fatalf("empty oracle corpus")
	}

	got := append([]string(nil), oracle.Input...)
	sort.Strings(got)

	if len(got) != len(oracle.Sorted) {
		t.Fatalf("length mismatch: got %d, want %d", len(got), len(oracle.Sorted))
	}
	for i := range oracle.Sorted {
		if got[i] != oracle.Sorted[i] {
			t.Fatalf("index %d: sort.Strings gives %q, localeCompare('en') gives %q (full: got=%v want=%v)",
				i, got[i], oracle.Sorted[i], got, oracle.Sorted)
		}
	}
}
