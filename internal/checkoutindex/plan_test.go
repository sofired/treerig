package checkoutindex

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// "survives the round trip through the page as plain JavaScript" is left
// out; see render_test.go's package comment for why.

// planTestPage is a minimal document carrying the four fields
// planIndexUpdate reads, defaulting title, shell, and body when
// titleShellBody omits them.
func planTestPage(stamp, main string, titleShellBody ...string) string {
	title, shell, body := "Checkouts (1)", "shell-1", "body-1"
	if len(titleShellBody) > 0 {
		title = titleShellBody[0]
	}
	if len(titleShellBody) > 1 {
		shell = titleShellBody[1]
	}
	if len(titleShellBody) > 2 {
		body = titleShellBody[2]
	}
	return fmt.Sprintf(
		`<!doctype html><html lang="en" data-shell="%s"><head><title>%s</title></head><body>`+
			`<header><p>Updated <time datetime="%s">x</time></p></header>`+
			`<main data-body="%s">%s</main></body></html>`,
		shell, title, stamp, body, main,
	)
}

// planTestEntry is a plain running checkout with no services, used only by
// the fingerprinting test below.
var planTestEntry = func() IndexEntry {
	return IndexEntry{
		RepositoryID: testRepo, WorktreeID: testWorktreeIDs[0], Project: "acme", Domain: "acme.localhost", Slug: "feature-a",
		Host: "feature-a.acme.localhost", Branch: strPtr("feature/a"), Root: "/Users/dev/worktrees/feature-a",
		Status: StatusRunning,
	}
}

// TestPlanIndexUpdate covers planIndexUpdate.
func TestPlanIndexUpdate(t *testing.T) {
	t.Run("reports the stamp, title, fingerprints and table of a page whose stamp has moved", func(t *testing.T) {
		current := "2026-09-21T12:00:00.000Z"
		page := planTestPage("2026-09-21T12:30:00.000Z", "<p>rows</p>", "Checkouts (2)")
		got := planIndexUpdate(&current, page)
		want := indexUpdate{Kind: indexUpdateReady, Stamp: "2026-09-21T12:30:00.000Z", Title: "Checkouts (2)", Main: "<p>rows</p>", Shell: "shell-1", Body: "body-1"}
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("says a page whose stamp has not moved is unchanged, which is not the same as unreadable", func(t *testing.T) {
		current := "2026-09-21T12:00:00.000Z"
		page := planTestPage("2026-09-21T12:00:00.000Z", "<p>rows</p>")
		if got, want := planIndexUpdate(&current, page), (indexUpdate{Kind: indexUpdateUnchanged}); got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("calls anything it cannot read foreign, so a tab can say so instead of sitting on stale rows", func(t *testing.T) {
		current := "2026-09-21T12:00:00.000Z"
		full := planTestPage("2026-09-21T12:30:00.000Z", "<p>rows</p>")
		if got, want := planIndexUpdate(&current, full[:strings.Index(full, "<main")]), (indexUpdate{Kind: indexUpdateForeign}); got != want {
			t.Errorf("cut before <main>: got %+v, want %+v", got, want)
		}
		if got, want := planIndexUpdate(&current, full[:strings.Index(full, "</main>")]), (indexUpdate{Kind: indexUpdateForeign}); got != want {
			t.Errorf("cut before </main>: got %+v, want %+v", got, want)
		}
		// Someone else's 200 answers none of it.
		if got, want := planIndexUpdate(&current, "<html><body>404 Not Found</body></html>"), (indexUpdate{Kind: indexUpdateForeign}); got != want {
			t.Errorf("unrelated page: got %+v, want %+v", got, want)
		}
		if got, want := planIndexUpdate(nil, planTestPage("", "<p>rows</p>")), (indexUpdate{Kind: indexUpdateForeign}); got != want {
			t.Errorf("empty stamp: got %+v, want %+v", got, want)
		}
	})

	t.Run("refuses an attribute the tag ends before, rather than reading on to the next quote", func(t *testing.T) {
		// Cut inside the timestamp: the next quote in the document is a
		// whole tag away.
		truncated := `<html data-shell="s"><head><title>T</title></head><body><header>` +
			`<time datetime="2026><main data-body="b"><p>rows</p></main></body></html>`
		if got, want := planIndexUpdate(nil, truncated), (indexUpdate{Kind: indexUpdateForeign}); got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
		// And one with no closing quote anywhere after it.
		unterminated := `<html data-shell="s"><title>T</title><main data-body="b">x</main><time datetime="2026`
		if got, want := planIndexUpdate(nil, unterminated), (indexUpdate{Kind: indexUpdateForeign}); got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("reads back exactly what this module renders, and none of the script that parses it", func(t *testing.T) {
		first := RenderIndexHTML(IndexSnapshot{GeneratedAt: "2026-09-21T12:00:00.000Z", DockerReachable: true})
		second := RenderIndexHTML(IndexSnapshot{GeneratedAt: "2026-09-21T12:30:00.000Z", DockerReachable: true})
		current := "2026-09-21T12:00:00.000Z"
		plan := planIndexUpdate(&current, second)
		if plan.Kind != indexUpdateReady {
			t.Fatalf("expected an update, got %+v", plan)
		}
		if plan.Stamp != "2026-09-21T12:30:00.000Z" {
			t.Errorf("stamp = %q", plan.Stamp)
		}
		if plan.Title != "Checkouts (0)" {
			t.Errorf("title = %q", plan.Title)
		}
		// Exactly the body, to the byte. The page embeds this function's
		// own source, so a sentinel matched against that copy would splice
		// the script into the table and still look right in a browser.
		mainAt := strings.Index(second, "<main")
		wantMain := second[strings.Index(second[mainAt:], ">")+mainAt+1 : strings.Index(second, "</main>")]
		if plan.Main != wantMain {
			t.Errorf("main = %q, want %q", plan.Main, wantMain)
		}
		if strings.Contains(plan.Main, "<script") {
			t.Error("main should not contain the script")
		}
		if strings.Contains(plan.Main, "planIndexUpdate") {
			t.Error("main should not contain the embedded function's own name")
		}
		// The clock, the announcer and the stale marker live outside the
		// swapped region: inside it, each would be detached on the first
		// update.
		if strings.Contains(plan.Main, "index-announcer") {
			t.Error("main should not contain the announcer")
		}
		if strings.Contains(plan.Main, "index-stale") {
			t.Error("main should not contain the stale marker")
		}
		if strings.Contains(plan.Main, "<time") {
			t.Error("main should not contain the clock")
		}
		if got, want := planIndexUpdate(&current, first), (indexUpdate{Kind: indexUpdateUnchanged}); got != want {
			t.Errorf("re-reading the same stamp: got %+v, want %+v", got, want)
		}
	})

	t.Run("fingerprints the shell and the rows separately, from what they are made of", func(t *testing.T) {
		rendered := RenderIndexHTML(IndexSnapshot{GeneratedAt: "2026-09-21T12:30:00.000Z", DockerReachable: true})
		current := "2026-09-21T12:00:00.000Z"
		plan := planIndexUpdate(&current, rendered)
		if plan.Kind != indexUpdateReady {
			t.Fatalf("expected an update, got %+v", plan)
		}
		// The body is a digest of exactly what lands between the tags: the
		// tab compares its own data-body against it to decide whether to
		// swap.
		sum := sha256.Sum256([]byte(plan.Main))
		wantBody := base64.RawURLEncoding.EncodeToString(sum[:])[:16]
		if plan.Body != wantBody {
			t.Errorf("body = %q, want %q", plan.Body, wantBody)
		}
		// Different rows, same build: the body moves and the shell does not.
		differentEntry := planTestEntry()
		differentEntry.Slug, differentEntry.Branch = "feature-z", strPtr("feature/z")
		other := planIndexUpdate(nil, RenderIndexHTML(IndexSnapshot{
			Entries: []IndexEntry{differentEntry}, GeneratedAt: "2026-09-21T12:30:00.000Z", DockerReachable: true,
		}))
		if other.Shell != plan.Shell {
			t.Errorf("shell changed: got %q, want %q", other.Shell, plan.Shell)
		}
		if other.Body == plan.Body {
			t.Error("body should change with different rows")
		}
		// Same build, same fingerprints; the timestamp is part of neither.
		later := planIndexUpdate(nil, RenderIndexHTML(IndexSnapshot{GeneratedAt: "later", DockerReachable: true}))
		if later.Shell != plan.Shell || later.Body != plan.Body {
			t.Errorf("later = %+v, want shell %q and body %q", later, plan.Shell, plan.Body)
		}
	})

	t.Run("calls a page with no fingerprints foreign, rather than reloading for a shell it never read", func(t *testing.T) {
		rendered := RenderIndexHTML(IndexSnapshot{GeneratedAt: "2026-09-21T12:30:00.000Z", DockerReachable: true})
		current := "2026-09-21T12:00:00.000Z"
		// An older treerig wrote this: without both fingerprints there is
		// nothing to compare, and a plan would drive a reload keyed on
		// nothing.
		noShell := stripAttr(rendered, ` data-shell="[^"]+"`)
		if got, want := planIndexUpdate(&current, noShell), (indexUpdate{Kind: indexUpdateForeign}); got != want {
			t.Errorf("no shell: got %+v, want %+v", got, want)
		}
		noBody := stripAttr(rendered, ` data-body="[^"]+"`)
		if got, want := planIndexUpdate(&current, noBody), (indexUpdate{Kind: indexUpdateForeign}); got != want {
			t.Errorf("no body: got %+v, want %+v", got, want)
		}
	})
}

// stripAttr removes every match of pattern from html, for knocking an
// attribute like data-shell="…" out of a rendered page.
func stripAttr(html, pattern string) string {
	return regexp.MustCompile(pattern).ReplaceAllString(html, "")
}
