package checkoutindex

import (
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/text"
)

// Ported from tests/unit/checkout-index.test.ts's `rendering` and
// `planIndexUpdate` describe blocks (parseContainerInventory and
// buildSnapshot are in snapshot_test.go), one t.Run per "it" title.
//
// Left out: "survives the round trip through the page as plain JavaScript"
// (in `describe('planIndexUpdate')`). It runs planIndexUpdate.toString()
// through node:vm's runInNewContext and liveUpdateScript() through
// node:vm's Script to prove the embedded text is valid, self-contained
// JavaScript — a property of the embedded asset
// (internal/checkoutindex/assets/script.js), not of this package's Go code,
// and not something a Go test can check.

func strPtr(s string) *string { return &s }

func countMatches(s string, re *regexp.Regexp) int {
	return len(re.FindAllString(s, -1))
}

func renderTestEntry() IndexEntry {
	return IndexEntry{
		RepositoryID: testRepo, WorktreeID: testWorktreeIDs[0], Project: "acme", Domain: "acme.localhost", Slug: "feature-a",
		Host: "feature-a.acme.localhost", Branch: strPtr("feature/<a>"), Root: "/Users/dev/worktrees/feature-a",
		Status: StatusPartial,
		Services: []IndexServiceLink{
			{Name: apiService.Name, Label: apiService.Label, Purpose: apiService.Purpose, URL: apiService.URL, Available: true},
			{Name: mailService.Name, Label: mailService.Label, Purpose: `Captured "email"`, URL: mailService.URL, Available: false},
		},
	}
}

func renderTestSnapshot() IndexSnapshot {
	return IndexSnapshot{Entries: []IndexEntry{renderTestEntry()}, GeneratedAt: "2026-09-21T12:00:00.000Z", DockerReachable: true}
}

const renderTestURL = "http://berth.localhost"

const dockerNoticeHTML = "Docker could not be asked about containers, so no service below has been checked. " +
	"Rows show Unknown unless Git reports the checkout gone. Run <code>berth index</code> again once it answers."

// pinHomeDir fixes what compactPath's production call sites (renderRow,
// formatIndexRow) resolve as the current user's home, for the rest of the
// test. Without this, an assertion that wants to check what the row-
// rendering code does with a path under home would have to call compactPath
// itself to build the expected value — the same call the code under test
// makes — which could never catch a regression in compactPath, since both
// sides would be wrong together.
func pinHomeDir(t *testing.T, home string) {
	t.Helper()
	original := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDir = original })
}

func TestRendering(t *testing.T) {
	// Fixed and deliberately unrelated to every fixture's root
	// (/Users/dev/worktrees/feature-a), so compactPath leaves those roots
	// untouched here regardless of the real machine's actual home
	// directory; the dedicated "compacts paths under the home directory
	// only" test below exercises the tilde branch itself.
	pinHomeDir(t, "/home/berth-test")

	t.Run("renders a link for available services, a disabled span otherwise, and escapes everything", func(t *testing.T) {
		html := RenderIndexHTML(renderTestSnapshot())
		for _, want := range []string{
			">acme · acme.localhost</h2>",
			"<strong>feature/&lt;a&gt;</strong>",
			`<a class="service-link" href="http://feature-a.acme.localhost"`,
			// WebKit drops a bulletless list's role unless it is asked for.
			`<ul role="list">`,
			`<span class="service-link service-disabled" title="http://mail.feature-a.acme.localhost"`,
			"Captured &quot;email&quot;",
			"status-partial",
		} {
			if !strings.Contains(html, want) {
				t.Errorf("html does not contain %q", want)
			}
		}
	})

	t.Run("says so when nothing is indexed, and points at the missing-checkout remedy", func(t *testing.T) {
		empty := IndexSnapshot{GeneratedAt: "now", DockerReachable: true}
		if !strings.Contains(RenderIndexHTML(empty), "No checkouts have been indexed yet. Run <code>berth up</code> in a worktree.") {
			t.Error("missing empty-state HTML")
		}
		if !strings.Contains(FormatIndexText(empty, renderTestURL), "No checkouts have been indexed yet. Run `berth up` in a worktree.") {
			t.Error("missing empty-state text")
		}
		gone := renderTestEntry()
		gone.Status = StatusMissing
		goneSnapshot := IndexSnapshot{Entries: []IndexEntry{gone}, GeneratedAt: "now", DockerReachable: true}
		if !strings.Contains(RenderIndexHTML(goneSnapshot), "berth worktree remove feature-a") {
			t.Error("missing remove guidance in HTML")
		}
		if !strings.Contains(FormatIndexText(goneSnapshot, renderTestURL), "    gone      berth worktree remove feature-a") {
			t.Error("missing remove guidance in text")
		}
	})

	t.Run("formats the text twin with the index URL, each purpose, the legend, and state markers", func(t *testing.T) {
		got := strings.Split(FormatIndexText(renderTestSnapshot(), renderTestURL), "\n")
		want := []string{
			"Checkout index  http://berth.localhost",
			"Updated         2026-09-21T12:00:00.000Z",
			"",
			"Running: every service answers. Partial: some do. Stopped: none do. Missing:",
			"the checkout is gone. Unknown: the state could not be read.",
			"Services: ! not answering, ? not checked, - checkout gone.",
			"",
			"acme · acme.localhost",
			"  [Partial]  feature/<a>",
			"    host      feature-a.acme.localhost",
			// Not under the pinned home, so compactPath leaves it whole; a
			// hardcoded literal, not a call to compactPath itself, so a
			// regression there can't hide behind this test calling the same
			// code it means to check.
			"    checkout  /Users/dev/worktrees/feature-a",
			"      API (REST surface)           http://feature-a.acme.localhost",
			`    ! Mailpit (Captured "email")   http://mail.feature-a.acme.localhost`,
		}
		if len(got) != len(want) {
			t.Fatalf("got %d lines, want %d\ngot: %v\nwant: %v", len(got), len(want), got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("line %d: got %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("keeps every text line inside eighty columns, however long the notice", func(t *testing.T) {
		snapshot := renderTestSnapshot()
		snapshot.DockerReachable = false
		snapshot.IdentityError = &IdentityFailure{Reason: "fatal: not a git repository (or any of the parent directories): .git", RowsUnknown: true}
		wordy := FormatIndexText(snapshot, renderTestURL)
		for _, line := range strings.Split(wordy, "\n") {
			if text.JSLen(line) > 80 {
				t.Errorf("line exceeds 80 columns: %q", line)
			}
		}
		if !strings.Contains(wordy, "not a git repository") {
			t.Error("missing the identity error's own text")
		}
	})

	t.Run("names a detached head and a branch nothing recorded, and cannot be mistaken for a real one", func(t *testing.T) {
		detached := renderTestEntry()
		detached.Branch = strPtr("")
		detachedSnapshot := IndexSnapshot{Entries: []IndexEntry{detached}, GeneratedAt: renderTestSnapshot().GeneratedAt, DockerReachable: true}
		if !strings.Contains(RenderIndexHTML(detachedSnapshot), `<th scope="row"><span class="muted">(detached)</span>`) {
			t.Error("missing detached-head markup")
		}

		nothingKnows := renderTestEntry()
		nothingKnows.Branch = nil
		blank := IndexSnapshot{Entries: []IndexEntry{nothingKnows}, GeneratedAt: renderTestSnapshot().GeneratedAt, DockerReachable: true}
		if !strings.Contains(RenderIndexHTML(blank), `<th scope="row"><span class="muted">(no branch)</span>`) {
			t.Error("missing no-branch markup")
		}
		if !strings.Contains(FormatIndexText(blank, renderTestURL), "  [Partial]  (no branch)") {
			t.Error("missing no-branch text")
		}

		namedDetached := renderTestEntry()
		namedDetached.Branch = strPtr("detached")
		namedSnapshot := IndexSnapshot{Entries: []IndexEntry{namedDetached}, GeneratedAt: renderTestSnapshot().GeneratedAt, DockerReachable: true}
		if !strings.Contains(FormatIndexText(namedSnapshot, renderTestURL), "  [Partial]  detached") {
			t.Error("a branch literally named 'detached' should print as itself")
		}
		// `detached` is a legal branch name: the parentheses are what tell
		// them apart.
		if got, want := DescribeBranch(strPtr("detached")), (DescribedBranch{Text: "detached", Muted: false}); got != want {
			t.Errorf("DescribeBranch(detached) = %+v, want %+v", got, want)
		}
	})

	t.Run("heads a project with its domain alone when no card has named the project", func(t *testing.T) {
		unnamedEntry := renderTestEntry()
		unnamedEntry.Project = "acme.localhost"
		unnamed := RenderIndexHTML(IndexSnapshot{Entries: []IndexEntry{unnamedEntry}, GeneratedAt: renderTestSnapshot().GeneratedAt, DockerReachable: true})
		if !strings.Contains(unnamed, ">acme.localhost</h2>") {
			t.Error("missing bare-domain heading")
		}
		if strings.Contains(unnamed, ">acme.localhost · ") {
			t.Error("should not join the domain with itself")
		}
	})

	t.Run("keeps one table per domain, whatever each row knows about its project", func(t *testing.T) {
		entry := renderTestEntry()
		second := renderTestEntry()
		second.Slug, second.Branch, second.Project = "feature-b", strPtr("feature/b"), "acme.localhost"
		other := renderTestEntry()
		other.Slug, other.Domain, other.Project, other.Branch = "main", "widgets.localhost", "widgets", strPtr("main")
		snapshot := IndexSnapshot{Entries: []IndexEntry{entry, second, other}, GeneratedAt: renderTestSnapshot().GeneratedAt, DockerReachable: true}
		html := RenderIndexHTML(snapshot)
		if got := countMatches(html, regexp.MustCompile(`<h2`)); got != 2 {
			t.Errorf("h2 count = %d, want 2", got)
		}
		if got := countMatches(html, regexp.MustCompile(`<th scope="row">`)); got != 3 {
			t.Errorf("row count = %d, want 3", got)
		}
		if !strings.Contains(html, ">acme · acme.localhost</h2>") {
			t.Error("missing acme heading")
		}
		sections := strings.Split(html, "<section>")
		acme, widgets := "", ""
		if len(sections) > 1 {
			acme = sections[1]
		}
		if len(sections) > 2 {
			widgets = sections[2]
		}
		if got := countMatches(acme, regexp.MustCompile(`<th scope="row">`)); got != 2 {
			t.Errorf("acme section rows = %d, want 2", got)
		}
		if got := countMatches(widgets, regexp.MustCompile(`<th scope="row">`)); got != 1 {
			t.Errorf("widgets section rows = %d, want 1", got)
		}
		textOut := FormatIndexText(snapshot, renderTestURL)
		if got := countMatches(textOut, regexp.MustCompile(`(?m)^acme · acme\.localhost$`)); got != 1 {
			t.Errorf("heading line count = %d, want 1", got)
		}
		if !strings.Contains(textOut, "feature/b") {
			t.Error("missing second row's branch")
		}
	})

	t.Run("keeps service links live when Docker was never asked, and says which state is unread", func(t *testing.T) {
		blind := renderTestSnapshot()
		blind.DockerReachable = false
		html := RenderIndexHTML(blind)
		if !strings.Contains(html, dockerNoticeHTML) {
			t.Error("missing the docker notice")
		}
		// Both services stay clickable: nothing checked them, so nothing
		// may grey them out.
		if got := countMatches(html, regexp.MustCompile(`<a class="service-link"`)); got != 2 {
			t.Errorf("clickable services = %d, want 2", got)
		}
		if !strings.Contains(html, `<span class="sr-only"> (state unknown)</span>`) {
			t.Error("missing state-unknown marker")
		}
		if strings.Contains(html, "(unavailable)") {
			t.Error("should not claim unavailable when unchecked")
		}
		textOut := FormatIndexText(blind, renderTestURL)
		if !strings.Contains(textOut, "    ? API (REST surface)") {
			t.Error("missing unchecked marker in text")
		}
		if strings.Contains(textOut, "! Mailpit") {
			t.Error("should not mark Mailpit unavailable when unchecked")
		}
		// A checkout Git reports gone is not in doubt, and nothing was
		// asked of its services: they are not "not answering", they are
		// not there.
		blindGoneEntry := renderTestEntry()
		blindGoneEntry.Status = StatusMissing
		blindGone := IndexSnapshot{Entries: []IndexEntry{blindGoneEntry}, GeneratedAt: blind.GeneratedAt, DockerReachable: false}
		gone := RenderIndexHTML(blindGone)
		if !strings.Contains(gone, `<span class="sr-only"> (checkout gone)</span>`) {
			t.Error("missing checkout-gone marker")
		}
		if strings.Contains(gone, "(state unknown)") {
			t.Error("a gone checkout should not also say state unknown")
		}
		if !strings.Contains(gone, "status-missing") {
			t.Error("missing status-missing class")
		}
		goneText := FormatIndexText(blindGone, renderTestURL)
		if strings.Contains(goneText, "! API") {
			t.Error("should not mark a gone checkout's service unavailable")
		}
		if !strings.Contains(goneText, "    - API (REST surface)") {
			t.Error("missing gone marker in text")
		}
	})

	t.Run("says nothing about Docker or Git when there are no rows for it to be about", func(t *testing.T) {
		nothing := IndexSnapshot{GeneratedAt: "now", DockerReachable: false, IdentityError: &IdentityFailure{Reason: "git exploded", RowsUnknown: true}}
		if strings.Contains(RenderIndexHTML(nothing), "Docker could not be asked") {
			t.Error("should say nothing about Docker with no rows")
		}
		if strings.Contains(RenderIndexHTML(nothing), "git exploded") {
			t.Error("should say nothing about Git with no rows")
		}
		if strings.Contains(FormatIndexText(nothing, renderTestURL), "Docker could not be asked") {
			t.Error("text twin should say nothing about Docker with no rows")
		}
		placeholder := RenderIndexHTML(PlaceholderSnapshot("now"))
		if !strings.Contains(placeholder, "No checkouts have been indexed yet") {
			t.Error("placeholder should show the empty state")
		}
		if strings.Contains(placeholder, `class="notice"`) {
			t.Error("placeholder should carry no notice")
		}
	})

	t.Run("tells a screen reader what the grey tells everyone else about a disabled service", func(t *testing.T) {
		html := RenderIndexHTML(renderTestSnapshot())
		if !strings.Contains(html, `<span class="sr-only"> (unavailable)</span>`) {
			t.Error("missing unavailable marker")
		}
		// Visually hidden, not merely small: the text must not land in the
		// layout.
		if !regexp.MustCompile(`\.sr-only \{[^}]*position: absolute[^}]*clip-path: inset\(50%\)`).MatchString(html) {
			t.Error("sr-only rule missing expected properties")
		}
	})

	t.Run("explains the status words once for the page and counts the rows in the title", func(t *testing.T) {
		html := RenderIndexHTML(renderTestSnapshot())
		if !strings.Contains(html, "<title>Checkouts (1)</title>") {
			t.Error("missing row count in title")
		}
		if !strings.Contains(html, `<p class="legend">Running: every service answers.`) {
			t.Error("missing legend")
		}
		if !strings.Contains(html, "Missing: the checkout is gone.") {
			t.Error("legend missing Missing entry")
		}
		second := renderTestEntry()
		second.Slug, second.Domain, second.Project = "main", "widgets.localhost", "widgets"
		twoProjects := RenderIndexHTML(IndexSnapshot{Entries: []IndexEntry{renderTestEntry(), second}, GeneratedAt: renderTestSnapshot().GeneratedAt, DockerReachable: true})
		if got := countMatches(twoProjects, regexp.MustCompile(`class="legend"`)); got != 1 {
			t.Errorf("legend count = %d, want 1", got)
		}
		if !strings.Contains(twoProjects, "<title>Checkouts (2)</title>") {
			t.Error("missing two-row title")
		}
		if strings.Contains(RenderIndexHTML(IndexSnapshot{GeneratedAt: renderTestSnapshot().GeneratedAt, DockerReachable: true}), `class="legend"`) {
			t.Error("empty page should carry no legend")
		}
	})

	t.Run("heads every column and row for assistive technology, and names each table", func(t *testing.T) {
		html := RenderIndexHTML(renderTestSnapshot())
		if !strings.Contains(html, `<th scope="col">Branch</th>`) {
			t.Error("missing Branch column header")
		}
		if got := countMatches(html, regexp.MustCompile(`scope="col"`)); got != 5 {
			t.Errorf("scope=col count = %d, want 5", got)
		}
		if !strings.Contains(html, `<th scope="row"><strong>feature/&lt;a&gt;</strong>`) {
			t.Error("missing row header")
		}
		// Named once, by the heading it already shows, through an id that
		// counts sections.
		if !strings.Contains(html, `<div class="table-wrap" role="region" aria-labelledby="checkouts-0">`) {
			t.Error("missing labelled region")
		}
		// Focusable only while it has something to scroll to, which the
		// script decides.
		if strings.Contains(html, `tabindex="0"`) {
			t.Error("page markup should not set tabindex itself")
		}
		if !strings.Contains(script, "setAttribute('tabindex', '0')") {
			t.Error("script should set tabindex when scrolling")
		}
		if strings.Contains(html, "<caption") {
			t.Error("should not use a caption element")
		}
		if !strings.Contains(html, `<h2 id="checkouts-0">`) {
			t.Error("missing section heading id")
		}
	})

	t.Run("keeps a service in the tab it already opened rather than one tab per click", func(t *testing.T) {
		html := RenderIndexHTML(renderTestSnapshot())
		if !strings.Contains(html, `target="feature-a.acme.localhost-api"`) {
			t.Error("missing named target")
		}
		// `noopener` would make every named target behave like `_blank`.
		if strings.Contains(html, `rel="noopener"`) {
			t.Error("should not set rel=noopener")
		}
	})

	t.Run("re-colours the muted text and every status pill for a dark scheme", func(t *testing.T) {
		html := RenderIndexHTML(renderTestSnapshot())
		darkParts := strings.SplitN(html, "@media (prefers-color-scheme: dark)", 2)
		dark := ""
		if len(darkParts) > 1 {
			dark = strings.SplitN(darkParts[1], "</style>", 2)[0]
		}
		for _, status := range []string{"running", "partial", "stopped", "missing", "unknown"} {
			if !regexp.MustCompile(`\.status-` + status + `\s*\{`).MatchString(dark) {
				t.Errorf("dark scheme missing rule for status-%s", status)
			}
		}
		for _, selector := range []string{"muted", "guidance", "empty", "notice", "path-compact"} {
			if !regexp.MustCompile(`\.` + selector + `[^{]*\{`).MatchString(dark) {
				t.Errorf("dark scheme missing rule for .%s", selector)
			}
		}
	})

	t.Run("moves the checkout path under the branch on a narrow screen instead of dropping it", func(t *testing.T) {
		html := RenderIndexHTML(renderTestSnapshot())
		narrowParts := strings.SplitN(html, "@media (max-width: 900px)", 2)
		narrow := ""
		if len(narrowParts) > 1 {
			narrow = strings.SplitN(narrowParts[1], "@media", 2)[0]
		}
		if !regexp.MustCompile(`\.checkout \{ display: none; \}`).MatchString(narrow) {
			t.Error("missing checkout hide rule")
		}
		if !regexp.MustCompile(`\.path-compact \{ display: block;`).MatchString(narrow) {
			t.Error("missing path-compact show rule")
		}
		// Rendered either way; the stylesheet decides which one a reader sees.
		if !strings.Contains(html, `<td class="checkout">`) {
			t.Error("missing checkout cell")
		}
		phoneParts := strings.SplitN(html, "@media (max-width: 400px)", 2)
		phone := ""
		if len(phoneParts) > 1 {
			phone = strings.SplitN(phoneParts[1], "@media", 2)[0]
		}
		if !regexp.MustCompile(`\.host \{ display: none; \}`).MatchString(phone) {
			t.Error("missing host hide rule")
		}
		if !regexp.MustCompile(`\.host-compact \{ display: block;`).MatchString(phone) {
			t.Error("missing host-compact show rule")
		}
		if !strings.Contains(html, `<span class="host-compact"><code>feature-a.acme.localhost</code></span>`) {
			t.Error("missing host-compact span")
		}
		// Not under the pinned home, so compactPath leaves it whole, and it
		// carries no character escapeHTML would touch; a hardcoded literal
		// rather than a call to either, so a regression in one can't hide
		// behind this test calling the same code it means to check.
		if !strings.Contains(html, `<span class="path-compact">/Users/dev/worktrees/feature-a</span>`) {
			t.Error("missing path-compact span")
		}
	})

	t.Run("declares exactly the one script it carries, and no network destination but itself", func(t *testing.T) {
		html := RenderIndexHTML(renderTestSnapshot())
		// indexOf, like a browser: the element ends at the first closing
		// tag, and a hash over more than that would be a policy nothing
		// satisfies.
		start := strings.Index(html, "<script>") + len("<script>")
		end := strings.Index(html, "</script>")
		embedded := html[start:end]
		if regexp.MustCompile(`(?i)</script|<!--`).MatchString(embedded) {
			t.Error("embedded script should not contain a closing tag or a comment marker")
		}
		sum := sha256.Sum256([]byte(embedded))
		hash := base64.StdEncoding.EncodeToString(sum[:])
		if !strings.Contains(html, "script-src 'sha256-"+hash+"'") {
			t.Error("CSP script-src hash does not match the embedded script")
		}
		if !strings.Contains(html, "default-src 'none'") {
			t.Error("missing default-src 'none'")
		}
		if !strings.Contains(html, "connect-src 'self'") {
			t.Error("missing connect-src 'self'")
		}
		// Without this the page's own stylesheet is blocked and it renders bare.
		if !strings.Contains(html, "style-src 'unsafe-inline'") {
			t.Error("missing style-src 'unsafe-inline'")
		}
	})

	t.Run("refreshes the whole page when there is no scripting to swap with", func(t *testing.T) {
		if !strings.Contains(RenderIndexHTML(renderTestSnapshot()), `<noscript><meta http-equiv="refresh" content="60"></noscript>`) {
			t.Error("missing noscript refresh")
		}
	})

	t.Run("renders a registration that has never been up, with nothing to show in either twin", func(t *testing.T) {
		bare := IndexEntry{
			RepositoryID: testRepo, WorktreeID: testWorktreeIDs[1], Project: "acme.localhost", Domain: "acme.localhost",
			Slug: "feature-b", Host: "feature-b.acme.localhost", Status: StatusStopped,
		}
		snapshot := IndexSnapshot{Entries: []IndexEntry{bare}, GeneratedAt: renderTestSnapshot().GeneratedAt, DockerReachable: true}
		html := RenderIndexHTML(snapshot)
		if got := countMatches(html, regexp.MustCompile(`<span class="muted">—</span>`)); got != 2 {
			t.Errorf("muted-dash count = %d, want 2", got)
		}
		if strings.Contains(html, `<span class="path-compact">`) {
			t.Error("should carry no path-compact span with no root")
		}
		// The text twin has to say it too, or an empty line reads as an
		// omission.
		textOut := FormatIndexText(snapshot, renderTestURL)
		if !strings.Contains(textOut, "    checkout  -") {
			t.Error("missing checkout placeholder")
		}
		if !strings.Contains(textOut, "    (no services)") {
			t.Error("missing no-services placeholder")
		}
	})

	t.Run("gives a service its own line for the URL rather than one a terminal would fold", func(t *testing.T) {
		wordyEntry := renderTestEntry()
		wordyEntry.Services = []IndexServiceLink{{
			Name: "admin", Label: "Postgres admin", Purpose: "schema browser and query console",
			URL: "http://admin.feature-a.acme.localhost/console", Available: true,
		}}
		wordy := IndexSnapshot{Entries: []IndexEntry{wordyEntry}, GeneratedAt: renderTestSnapshot().GeneratedAt, DockerReachable: true}
		lines := strings.Split(FormatIndexText(wordy, renderTestURL), "\n")
		for _, line := range lines {
			if text.JSLen(line) > 80 {
				t.Errorf("line exceeds 80 columns: %q", line)
			}
		}
		if !slicesContain(lines, "      Postgres admin (schema browser and query console)") {
			t.Error("missing wrapped service name line")
		}
		if !slicesContain(lines, "        http://admin.feature-a.acme.localhost/console") {
			t.Error("missing indented URL line")
		}
	})

	t.Run("says what a failed Git lookup cost the page, which is not always the same thing", func(t *testing.T) {
		reason := "Git could not list the worktrees of /work/acme: fatal: not a git repository"
		scopedSnapshot := renderTestSnapshot()
		scopedSnapshot.IdentityError = &IdentityFailure{Reason: reason, RowsUnknown: true}
		scoped := RenderIndexHTML(scopedSnapshot)
		if !strings.Contains(scoped, "Its rows show Unknown rather than Missing.") {
			t.Error("missing scoped consequence")
		}
		wholesaleSnapshot := renderTestSnapshot()
		wholesaleSnapshot.IdentityError = &IdentityFailure{Reason: reason, RowsUnknown: false}
		wholesale := RenderIndexHTML(wholesaleSnapshot)
		if !strings.Contains(wholesale, "No checkout below could be compared against it, so none is reported missing.") {
			t.Error("missing wholesale consequence")
		}
		if strings.Contains(wholesale, "rather than Missing") {
			t.Error("wholesale failure should not mention rows")
		}
		if !strings.Contains(FormatIndexText(wholesaleSnapshot, renderTestURL), "none is reported missing") {
			t.Error("text twin missing wholesale consequence")
		}
	})

	t.Run("keeps a backtick in whatever Git said from marking up the rest of the sentence", func(t *testing.T) {
		snapshot := renderTestSnapshot()
		snapshot.IdentityError = &IdentityFailure{Reason: "fatal: `--porcelain' is not a git command", RowsUnknown: true}
		html := RenderIndexHTML(snapshot)
		start := strings.Index(html, `<p class="notice">`)
		end := strings.Index(html[start:], "</p>") + start
		notice := html[start:end]
		if !strings.Contains(notice, "<code>berth index</code>") {
			t.Error("missing the one legitimate code span")
		}
		if got := countMatches(notice, regexp.MustCompile(`<code>`)); got != 1 {
			t.Errorf("code span count = %d, want 1", got)
		}
	})

	t.Run("leaves a word longer than the terminal whole rather than breaking a path", func(t *testing.T) {
		if got, want := wrapText(strings.Repeat("x", 95), 78), strings.Repeat("x", 95); got != want {
			t.Errorf("wrapText(95 x's) = %q, want %q", got, want)
		}
		if got, want := wrapText("one two three", 7), "one two\nthree"; got != want {
			t.Errorf("wrapText = %q, want %q", got, want)
		}
	})

	t.Run("carries every hook its own script looks up", func(t *testing.T) {
		// The script finds these by name. Rename one in the markup and the
		// page still renders, still passes every other test, and never
		// updates again.
		html := RenderIndexHTML(renderTestSnapshot())
		for _, id := range []string{"index-announcer", "index-stale"} {
			if !strings.Contains(script, "getElementById('"+id+"')") {
				t.Errorf("script missing getElementById(%q)", id)
			}
			if !strings.Contains(html, `id="`+id+`"`) {
				t.Errorf("html missing id=%q", id)
			}
		}
		if !strings.Contains(script, "querySelector('header time')") {
			t.Error("script missing header time lookup")
		}
		if !strings.Contains(script, "querySelector('header')") {
			t.Error("script missing header lookup")
		}
		if !strings.Contains(script, "querySelector('main')") {
			t.Error("script missing main lookup")
		}
		if !regexp.MustCompile(`(?s)<header>.*<time datetime=`).MatchString(html) {
			t.Error("missing header/time structure")
		}
		if !strings.Contains(html, "<main data-body=") {
			t.Error("missing main data-body attribute")
		}
	})

	t.Run("replaces what a terminal or a reader would be fooled by, and keeps what they need", func(t *testing.T) {
		// Built from code points rather than string escapes, so the source
		// file carries plain ASCII rather than the hostile characters
		// themselves.
		esc, rtlOverride, wordJoiner := string(rune(0x1b)), string(rune(0x202e)), string(rune(0x2060))
		softHyphen, replacement := string(rune(0xad)), string(rune(0xfffd))
		zwj, zwnj := string(rune(0x200d)), string(rune(0x200c))
		manWithLaptop := string(rune(0x1f468)) + zwj + string(rune(0x1f4bb))
		tagCharacter := string(rune(0xe0041))

		hostile := "feature/" + esc + "[2K" + rtlOverride + "x" + wordJoiner + "y"
		if got, want := text.Printable(hostile), "feature/"+replacement+"[2K"+replacement+"x"+replacement+"y"; got != want {
			t.Errorf("Printable(hostile) = %q, want %q", got, want)
		}
		if got, want := text.Printable("fea"+softHyphen+"ture"), "fea"+replacement+"ture"; got != want {
			t.Errorf("Printable(soft hyphen) = %q, want %q", got, want)
		}
		if got, want := text.Printable("one\ntwo\tthree"), "one two three"; got != want {
			t.Errorf("Printable(whitespace) = %q, want %q", got, want)
		}
		// The joiners carry meaning: an emoji sequence and Persian orthography.
		if got, want := text.Printable(manWithLaptop+" m"+zwnj+"i"), manWithLaptop+" m"+zwnj+"i"; got != want {
			t.Errorf("Printable(joiners) = %q, want %q", got, want)
		}
		if got, want := text.Printable("a"+tagCharacter+"b"), "a"+replacement+"b"; got != want {
			t.Errorf("Printable(tag character) = %q, want %q", got, want)
		}
		hostileEntry := renderTestEntry()
		hostileEntry.Branch = strPtr(hostile)
		one := IndexSnapshot{Entries: []IndexEntry{hostileEntry}, GeneratedAt: renderTestSnapshot().GeneratedAt, DockerReachable: true}
		if hidden := hiddenCharacters(RenderIndexHTML(one)); len(hidden) != 0 {
			t.Errorf("rendered HTML still carries hidden characters: %q", hidden)
		}
		if hidden := hiddenCharacters(FormatIndexText(one, renderTestURL)); len(hidden) != 0 {
			t.Errorf("formatted text still carries hidden characters: %q", hidden)
		}
	})

	t.Run("folds a multi-line reason from Git into a sentence that still fits a terminal", func(t *testing.T) {
		reason := "fatal: not a git repository\nhint: use the checkout this repository was cloned into, or run it again from there"
		wordy := renderTestSnapshot()
		wordy.IdentityError = &IdentityFailure{Reason: reason, RowsUnknown: true}
		textOut := FormatIndexText(wordy, renderTestURL)
		if strings.Contains(textOut, "�") {
			t.Error("a line break should fold, not become a replacement character")
		}
		for _, line := range strings.Split(textOut, "\n") {
			if text.JSLen(line) > 80 {
				t.Errorf("line exceeds 80 columns: %q", line)
			}
		}
		if !strings.Contains(RenderIndexHTML(wordy), "fatal: not a git repository hint: use the checkout") {
			t.Error("HTML should join the reason's lines with a space")
		}
	})

	t.Run("leaves the remove command on one line however long the slug", func(t *testing.T) {
		long := "feature-a-really-long-branch-name-that-uses-the-whole-slug-budget"
		goneEntry := renderTestEntry()
		goneEntry.Slug, goneEntry.Status = long, StatusMissing
		gone := IndexSnapshot{Entries: []IndexEntry{goneEntry}, GeneratedAt: renderTestSnapshot().GeneratedAt, DockerReachable: true}
		if !strings.Contains(FormatIndexText(gone, renderTestURL), "    gone      berth worktree remove "+long) {
			t.Error("remove command should stay on one line")
		}
	})

	t.Run("orders the tables by the heading a reader sees, and names every project on a shared domain", func(t *testing.T) {
		// beta's row comes first, so the heading has to sort the names
		// rather than list them in whatever order the rows arrived in.
		zeta := renderTestEntry()
		zeta.Project, zeta.Domain, zeta.Slug, zeta.Branch = "zeta", "a.localhost", "z", strPtr("z")
		beta := renderTestEntry()
		beta.Project, beta.Domain, beta.Slug, beta.Branch = "beta", "b.localhost", "b", strPtr("b")
		alpha := renderTestEntry()
		alpha.Project, alpha.Domain, alpha.Slug, alpha.Branch = "alpha", "b.localhost", "a", strPtr("a")
		rowsSnapshot := IndexSnapshot{Entries: []IndexEntry{zeta, beta, alpha}, GeneratedAt: renderTestSnapshot().GeneratedAt, DockerReachable: true}
		html := RenderIndexHTML(rowsSnapshot)
		headingPattern := regexp.MustCompile(`<h2 id="[^"]*">([^<]*)</h2>`)
		matches := headingPattern.FindAllStringSubmatch(html, -1)
		headings := make([]string, len(matches))
		for i, m := range matches {
			headings[i] = m[1]
		}
		if want := []string{"alpha, beta · b.localhost", "zeta · a.localhost"}; !slicesEqual(headings, want) {
			t.Errorf("headings = %v, want %v", headings, want)
		}
		if !strings.Contains(FormatIndexText(rowsSnapshot, renderTestURL), "alpha, beta · b.localhost") {
			t.Error("text twin missing the joined heading")
		}
	})

	t.Run("compacts paths under the home directory only", func(t *testing.T) {
		if got, want := compactPath("/home/me/dev/x", "/home/me"), "~/dev/x"; got != want {
			t.Errorf("compactPath = %q, want %q", got, want)
		}
		if got, want := compactPath("/opt/x", "/home/me"), "/opt/x"; got != want {
			t.Errorf("compactPath = %q, want %q", got, want)
		}
		// The home itself is not `~`: it is where the path already is.
		if got, want := compactPath("/home/me", "/home/me"), "/home/me"; got != want {
			t.Errorf("compactPath = %q, want %q", got, want)
		}
	})
}

func slicesContain(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// hiddenCharacters mirrors the TypeScript test's `hidden` helper: whatever a
// terminal or a browser would render as invisible or cursor-moving, which
// must never survive rendering, distinct from whitespace, which is the
// page's own layout.
func hiddenCharacters(value string) []rune {
	var hidden []rune
	for _, r := range value {
		whitespace := r == 0x09 || r == 0x0a || r == 0x0d
		control := (r < 0x20 && !whitespace) || r == 0x7f || (r >= 0x80 && r <= 0x9f)
		if control || r == 0x00ad || r == 0x061c || r == 0xfeff ||
			(r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) ||
			(r >= 0x2060 && r <= 0x2069) {
			hidden = append(hidden, r)
		}
	}
	return hidden
}
