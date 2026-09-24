package card_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sofired/berth/internal/checkoutindex/card"
	"github.com/sofired/berth/internal/layout"
)

const validCard = `{
  "version": 1,
  "project": "acme",
  "domain": "acme.localhost",
  "slug": "feature-login",
  "host": "feature-login.acme.localhost",
  "root": "/repo/worktree",
  "branch": "feature/login",
  "labelNamespace": "dev.berth",
  "services": [
    {"name": "api", "url": "http://feature-login.acme.localhost", "label": "API", "purpose": "REST surface"}
  ],
  "updatedAt": "2026-01-01T00:00:00.000Z"
}`

func writeCard(t *testing.T, dir, contents string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	file := filepath.Join(dir, card.IndexCardFile)
	if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return file
}

func TestReadIndexCardAt(t *testing.T) {
	t.Run("reads a well-formed card", func(t *testing.T) {
		file := writeCard(t, t.TempDir(), validCard)
		got := card.ReadIndexCardAt(file)
		if got == nil {
			t.Fatalf("expected a card")
		}
		if got.Project != "acme" || got.Slug != "feature-login" || got.LabelNamespace != "dev.berth" {
			t.Fatalf("unexpected card: %+v", got)
		}
		if len(got.Services) != 1 || got.Services[0].Name != "api" || got.Services[0].URL != "http://feature-login.acme.localhost" {
			t.Fatalf("unexpected services: %+v", got.Services)
		}
	})

	t.Run("a missing file reads as no card", func(t *testing.T) {
		got := card.ReadIndexCardAt(filepath.Join(t.TempDir(), "index.json"))
		if got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("a file over the size limit reads as no card, without being read", func(t *testing.T) {
		dir := t.TempDir()
		huge := strings.Repeat("x", 128*1024+1)
		file := writeCard(t, dir, huge)
		got := card.ReadIndexCardAt(file)
		if got != nil {
			t.Fatalf("expected nil for an oversized file, got %+v", got)
		}
	})

	t.Run("invalid JSON reads as no card", func(t *testing.T) {
		file := writeCard(t, t.TempDir(), "{ not json")
		if got := card.ReadIndexCardAt(file); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("an unknown key reads as no card", func(t *testing.T) {
		file := writeCard(t, t.TempDir(), strings.Replace(validCard, `"version": 1,`, `"version": 1, "bogus": true,`, 1))
		if got := card.ReadIndexCardAt(file); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("a URL carrying userinfo reads as no card", func(t *testing.T) {
		hostile := strings.Replace(validCard, `"url": "http://feature-login.acme.localhost"`, `"url": "http://trusted@elsewhere.example"`, 1)
		file := writeCard(t, t.TempDir(), hostile)
		if got := card.ReadIndexCardAt(file); got != nil {
			t.Fatalf("expected nil for a URL with userinfo, got %+v", got)
		}
	})

	t.Run("a non-http(s) URL reads as no card", func(t *testing.T) {
		hostile := strings.Replace(validCard, `"url": "http://feature-login.acme.localhost"`, `"url": "javascript:alert(1)"`, 1)
		file := writeCard(t, t.TempDir(), hostile)
		if got := card.ReadIndexCardAt(file); got != nil {
			t.Fatalf("expected nil for a non-http(s) URL, got %+v", got)
		}
	})

	t.Run("more than the service limit reads as no card", func(t *testing.T) {
		var services strings.Builder
		for i := 0; i < 65; i++ {
			if i > 0 {
				services.WriteString(",")
			}
			services.WriteString(`{"name": "s", "url": "http://h.example", "label": "L", "purpose": ""}`)
		}
		hostile := strings.Replace(validCard, `"services": [
    {"name": "api", "url": "http://feature-login.acme.localhost", "label": "API", "purpose": "REST surface"}
  ],`, `"services": [`+services.String()+`],`, 1)
		file := writeCard(t, t.TempDir(), hostile)
		if got := card.ReadIndexCardAt(file); got != nil {
			t.Fatalf("expected nil for too many services, got %+v", got)
		}
	})

	t.Run("root and branch may be empty (detached HEAD), but the required text fields may not", func(t *testing.T) {
		empty := strings.Replace(strings.Replace(validCard, `"root": "/repo/worktree",`, `"root": "",`, 1), `"branch": "feature/login",`, `"branch": "",`, 1)
		file := writeCard(t, t.TempDir(), empty)
		got := card.ReadIndexCardAt(file)
		if got == nil || got.Root != "" || got.Branch != "" {
			t.Fatalf("expected root/branch to be allowed empty, got %+v", got)
		}

		noProject := strings.Replace(validCard, `"project": "acme",`, `"project": "",`, 1)
		file = writeCard(t, t.TempDir(), noProject)
		if got := card.ReadIndexCardAt(file); got != nil {
			t.Fatalf("expected nil for an empty project, got %+v", got)
		}
	})
}

func TestRecordedLabelNamespace(t *testing.T) {
	const repositoryID = "a08b4892-5111-49f1-a47a-5ab988c7f676"
	const worktreeID = "2b41d9d4-ed88-49c1-be9c-5439bdf6a431"

	t.Run("the registration's own namespace wins outright", func(t *testing.T) {
		namespace := "dev.recorded"
		got := card.RecordedLabelNamespace(t.TempDir(), card.Registration{
			RepositoryID: repositoryID, WorktreeID: worktreeID, LabelNamespace: &namespace,
		})
		if got == nil || *got != "dev.recorded" {
			t.Fatalf("expected dev.recorded, got %v", got)
		}
	})

	t.Run("falls back to the live home's card", func(t *testing.T) {
		controlHome := t.TempDir()
		wt := layout.WorktreeLayout(controlHome, repositoryID, worktreeID)
		writeCard(t, wt.Home, validCard)

		got := card.RecordedLabelNamespace(controlHome, card.Registration{RepositoryID: repositoryID, WorktreeID: worktreeID})
		if got == nil || *got != "dev.berth" {
			t.Fatalf("expected dev.berth from the card, got %v", got)
		}
	})

	t.Run("falls back further to the quarantined home when the live one has no card", func(t *testing.T) {
		controlHome := t.TempDir()
		wt := layout.WorktreeLayout(controlHome, repositoryID, worktreeID)
		writeCard(t, wt.QuarantinePath, validCard)

		got := card.RecordedLabelNamespace(controlHome, card.Registration{RepositoryID: repositoryID, WorktreeID: worktreeID})
		if got == nil || *got != "dev.berth" {
			t.Fatalf("expected dev.berth from the quarantined card, got %v", got)
		}
	})

	t.Run("reads the namespace even from a card the full schema refuses", func(t *testing.T) {
		controlHome := t.TempDir()
		wt := layout.WorktreeLayout(controlHome, repositoryID, worktreeID)
		// An unknown key and a URL with userinfo both fail the full card
		// schema, but the namespace alone is still readable.
		broken := strings.Replace(validCard, `"version": 1,`, `"version": 1, "bogus": true,`, 1)
		writeCard(t, wt.Home, broken)

		got := card.RecordedLabelNamespace(controlHome, card.Registration{RepositoryID: repositoryID, WorktreeID: worktreeID})
		if got == nil || *got != "dev.berth" {
			t.Fatalf("expected dev.berth despite the broken card, got %v", got)
		}
		if full := card.ReadIndexCard(controlHome, repositoryID, worktreeID); full != nil {
			t.Fatalf("expected the full card read to still refuse it, got %+v", full)
		}
	})

	t.Run("undefined when nothing recorded it", func(t *testing.T) {
		got := card.RecordedLabelNamespace(t.TempDir(), card.Registration{RepositoryID: repositoryID, WorktreeID: worktreeID})
		if got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})
}

func TestTimestamp(t *testing.T) {
	got := card.Timestamp(time.Date(2026, 1, 2, 3, 4, 5, 6_000_000, time.FixedZone("test", 3600)))
	if want := "2026-01-02T02:04:05.006Z"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestWriteIndexCard(t *testing.T) {
	const repositoryID = "a08b4892-5111-49f1-a47a-5ab988c7f676"
	const worktreeID = "2b41d9d4-ed88-49c1-be9c-5439bdf6a431"

	validWrite := func() card.IndexCard {
		return card.IndexCard{
			Project: "acme", Domain: "acme.localhost", Slug: "feature-login",
			Host: "feature-login.acme.localhost", Root: "/repo/worktree", Branch: "feature/login",
			LabelNamespace: "dev.berth",
			Services: []card.IndexService{
				{Name: "api", URL: "http://feature-login.acme.localhost", Label: "API", Purpose: "REST surface"},
			},
			UpdatedAt: "2026-01-01T00:00:00.000Z",
		}
	}

	t.Run("writes a card that reads back exactly, in the schema's key order", func(t *testing.T) {
		controlHome := t.TempDir()
		home := layout.WorktreeLayout(controlHome, repositoryID, worktreeID).Home
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := card.WriteIndexCard(controlHome, repositoryID, worktreeID, validWrite()); err != nil {
			t.Fatalf("WriteIndexCard: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(home, card.IndexCardFile))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		text := string(data)
		orderedKeys := []string{
			`"version"`, `"project"`, `"domain"`, `"slug"`, `"host"`, `"root"`, `"branch"`,
			`"labelNamespace"`, `"services"`, `"name"`, `"url"`, `"label"`, `"purpose"`, `"updatedAt"`,
		}
		last := -1
		for _, key := range orderedKeys {
			index := strings.Index(text, key)
			if index < 0 {
				t.Fatalf("expected %s in written card:\n%s", key, text)
			}
			if index < last {
				t.Fatalf("key %s out of order in written card:\n%s", key, text)
			}
			last = index
		}
		if !strings.HasSuffix(text, "\n") {
			t.Fatalf("expected a trailing newline, got %q", text)
		}

		got := card.ReadIndexCardAt(filepath.Join(home, card.IndexCardFile))
		if got == nil {
			t.Fatalf("expected the written card to read back")
		}
		if got.Project != "acme" || got.Slug != "feature-login" || len(got.Services) != 1 {
			t.Fatalf("unexpected round trip: %+v", got)
		}
	})

	t.Run("does not escape '<', '>' or '&' the way encoding/json would by default", func(t *testing.T) {
		controlHome := t.TempDir()
		home := layout.WorktreeLayout(controlHome, repositoryID, worktreeID).Home
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		hostile := validWrite()
		hostile.Branch = "feature/<a>&b"
		if err := card.WriteIndexCard(controlHome, repositoryID, worktreeID, hostile); err != nil {
			t.Fatalf("WriteIndexCard: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(home, card.IndexCardFile))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !strings.Contains(string(data), `feature/<a>&b`) {
			t.Fatalf("expected the branch to be written unescaped, got:\n%s", data)
		}
	})

	t.Run("writes the JavaScript line separator raw and a C0 control escaped, matching JSON.stringify", func(t *testing.T) {
		controlHome := t.TempDir()
		home := layout.WorktreeLayout(controlHome, repositoryID, worktreeID).Home
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		lineSeparator := string(rune(0x2028))
		controlOne := string(rune(0x0001))
		hostile := validWrite()
		hostile.Branch = "feature/" + lineSeparator + "x" + controlOne + "y"
		if err := card.WriteIndexCard(controlHome, repositoryID, worktreeID, hostile); err != nil {
			t.Fatalf("WriteIndexCard: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(home, card.IndexCardFile))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !strings.Contains(string(data), lineSeparator) {
			t.Fatalf("expected the raw U+2028 separator in the written bytes, got:\n%s", data)
		}
		if !strings.Contains(string(data), "\\u0001") {
			t.Fatalf("expected U+0001 escaped as \\u0001, got:\n%s", data)
		}
		got := card.ReadIndexCardAt(filepath.Join(home, card.IndexCardFile))
		if got == nil || got.Branch != hostile.Branch {
			t.Fatalf("expected the branch to read back exactly, got %+v", got)
		}
	})

	t.Run("refuses a card outside the schema's bounds", func(t *testing.T) {
		controlHome := t.TempDir()
		home := layout.WorktreeLayout(controlHome, repositoryID, worktreeID).Home
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		invalid := validWrite()
		invalid.Project = ""
		if err := card.WriteIndexCard(controlHome, repositoryID, worktreeID, invalid); err == nil {
			t.Fatalf("expected an error for an empty project")
		}
	})
}

// TestCardBoundsSharedByReadAndWrite checks WriteIndexCard and
// ReadIndexCardAt at the same boundary values, just at and just past each
// bound cardBounds enforces — proving the two paths share one definition of
// "valid" rather than two that could drift apart.
func TestCardBoundsSharedByReadAndWrite(t *testing.T) {
	const repositoryID = "a08b4892-5111-49f1-a47a-5ab988c7f676"
	const worktreeID = "2b41d9d4-ed88-49c1-be9c-5439bdf6a431"
	repeat := func(n int) string { return strings.Repeat("a", n) }

	baseCard := func() card.IndexCard {
		return card.IndexCard{
			Project: "acme", Domain: "acme.localhost", Slug: "feature-login",
			Host: "feature-login.acme.localhost", Root: "/repo/worktree", Branch: "feature/login",
			LabelNamespace: "dev.berth", UpdatedAt: "2026-01-01T00:00:00.000Z",
		}
	}

	checkWrite := func(t *testing.T, c card.IndexCard, valid bool) {
		t.Helper()
		controlHome := t.TempDir()
		home := layout.WorktreeLayout(controlHome, repositoryID, worktreeID).Home
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		err := card.WriteIndexCard(controlHome, repositoryID, worktreeID, c)
		if valid && err != nil {
			t.Fatalf("WriteIndexCard: unexpected error: %v", err)
		}
		if !valid && err == nil {
			t.Fatalf("WriteIndexCard: expected an error, got none")
		}
	}

	checkRead := func(t *testing.T, c card.IndexCard, valid bool) {
		t.Helper()
		services := make([]map[string]any, len(c.Services))
		for i, s := range c.Services {
			services[i] = map[string]any{"name": s.Name, "url": s.URL, "label": s.Label, "purpose": s.Purpose}
		}
		raw := map[string]any{
			"version": 1, "project": c.Project, "domain": c.Domain, "slug": c.Slug,
			"host": c.Host, "root": c.Root, "branch": c.Branch,
			"labelNamespace": c.LabelNamespace, "services": services, "updatedAt": c.UpdatedAt,
		}
		data, err := json.Marshal(raw)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		file := filepath.Join(t.TempDir(), card.IndexCardFile)
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		got := card.ReadIndexCardAt(file)
		if valid && got == nil {
			t.Fatalf("ReadIndexCardAt: expected a card, got nil (wrote %s)", data)
		}
		if !valid && got != nil {
			t.Fatalf("ReadIndexCardAt: expected nil, got %+v (wrote %s)", got, data)
		}
	}

	cases := []struct {
		name  string
		apply func(c *card.IndexCard)
		valid bool
	}{
		{"project at the 512-character limit", func(c *card.IndexCard) { c.Project = repeat(512) }, true},
		{"project one character over the limit", func(c *card.IndexCard) { c.Project = repeat(513) }, false},
		{"domain at the limit", func(c *card.IndexCard) { c.Domain = repeat(512) }, true},
		{"domain one character over the limit", func(c *card.IndexCard) { c.Domain = repeat(513) }, false},
		{"slug at the limit", func(c *card.IndexCard) { c.Slug = repeat(512) }, true},
		{"slug one character over the limit", func(c *card.IndexCard) { c.Slug = repeat(513) }, false},
		{"host at the limit", func(c *card.IndexCard) { c.Host = repeat(512) }, true},
		{"host one character over the limit", func(c *card.IndexCard) { c.Host = repeat(513) }, false},
		{"root at the 4096-character path limit", func(c *card.IndexCard) { c.Root = repeat(4096) }, true},
		{"root one character over the path limit", func(c *card.IndexCard) { c.Root = repeat(4097) }, false},
		{"branch at the path limit", func(c *card.IndexCard) { c.Branch = repeat(4096) }, true},
		{"branch one character over the path limit", func(c *card.IndexCard) { c.Branch = repeat(4097) }, false},
		{"updatedAt at the 512-character limit", func(c *card.IndexCard) { c.UpdatedAt = repeat(512) }, true},
		{"updatedAt one character over the limit", func(c *card.IndexCard) { c.UpdatedAt = repeat(513) }, false},
		{
			"labelNamespace at the 253-character limit", func(c *card.IndexCard) {
				c.LabelNamespace = "aa." + repeat(250) // 2 + 1 + 250 = 253, still matches the reverse-domain pattern
			}, true,
		},
		{
			"labelNamespace one character over the limit", func(c *card.IndexCard) {
				c.LabelNamespace = "aa." + repeat(251) // 254
			}, false,
		},
		{
			"a service name at the limit", func(c *card.IndexCard) {
				c.Services = []card.IndexService{{Name: repeat(512), URL: "http://x.example", Label: "L", Purpose: ""}}
			}, true,
		},
		{
			"a service name one character over the limit", func(c *card.IndexCard) {
				c.Services = []card.IndexService{{Name: repeat(513), URL: "http://x.example", Label: "L", Purpose: ""}}
			}, false,
		},
		{
			"a service label at the limit", func(c *card.IndexCard) {
				c.Services = []card.IndexService{{Name: "N", URL: "http://x.example", Label: repeat(512), Purpose: ""}}
			}, true,
		},
		{
			"a service label one character over the limit", func(c *card.IndexCard) {
				c.Services = []card.IndexService{{Name: "N", URL: "http://x.example", Label: repeat(513), Purpose: ""}}
			}, false,
		},
		{
			"a service purpose at the limit", func(c *card.IndexCard) {
				c.Services = []card.IndexService{{Name: "N", URL: "http://x.example", Label: "L", Purpose: repeat(512)}}
			}, true,
		},
		{
			"a service purpose one character over the limit", func(c *card.IndexCard) {
				c.Services = []card.IndexService{{Name: "N", URL: "http://x.example", Label: "L", Purpose: repeat(513)}}
			}, false,
		},
		{
			"a service URL at the 2048-character limit", func(c *card.IndexCard) {
				c.Services = []card.IndexService{{Name: "N", URL: "http://" + repeat(2048-len("http://")), Label: "L", Purpose: ""}}
			}, true,
		},
		{
			"a service URL one character over the limit", func(c *card.IndexCard) {
				c.Services = []card.IndexService{{Name: "N", URL: "http://" + repeat(2049-len("http://")), Label: "L", Purpose: ""}}
			}, false,
		},
		{
			"exactly the 64-service limit", func(c *card.IndexCard) {
				c.Services = make([]card.IndexService, 64)
				for i := range c.Services {
					c.Services[i] = card.IndexService{Name: "N", URL: "http://x.example", Label: "L", Purpose: ""}
				}
			}, true,
		},
		{
			"one service over the limit", func(c *card.IndexCard) {
				c.Services = make([]card.IndexService, 65)
				for i := range c.Services {
					c.Services[i] = card.IndexService{Name: "N", URL: "http://x.example", Label: "L", Purpose: ""}
				}
			}, false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			built := baseCard()
			tc.apply(&built)
			t.Run("write", func(t *testing.T) { checkWrite(t, built, tc.valid) })
			t.Run("read", func(t *testing.T) { checkRead(t, built, tc.valid) })
		})
	}
}
