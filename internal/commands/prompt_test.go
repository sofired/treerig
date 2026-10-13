package commands

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/registry/registrytest"
)

var vtControl = regexp.MustCompile("\x1b\\[[0-9;]*m")

// stripVT removes ANSI colour escapes from value, so a test can assert on
// text regardless of whether colour was applied.
func stripVT(value string) string { return vtControl.ReplaceAllString(value, "") }

// plainEnv is an Env with no environment variables set and no terminal
// attached, the baseline a test customises from.
func plainEnv() *Env {
	return &Env{Getenv: func(string) (string, bool) { return "", false }}
}

func TestTypedConfirmationPrompt(t *testing.T) {
	t.Run("sets the required phrase apart from the explanation and input cursor", func(t *testing.T) {
		question := stripVT(confirmationQuestion(plainEnv(), "This removes the environment and its data.", "remove feature-login"))
		want := "This removes the environment and its data.\n\n" +
			"Type this exact phrase to continue:\n\n" +
			"  remove feature-login\n\n" +
			"> "
		if question != want {
			t.Fatalf("question = %q", question)
		}
	})

	t.Run("keeps a multi-line plan multi-line, and filters inside each line", func(t *testing.T) {
		question := stripVT(confirmationQuestion(plainEnv(),
			"This will permanently remove x.localhost:\n  2 containers\n  its \x1b[2Kregistration", "x.localhost"))
		lines := strings.Split(question, "\n")[:3]
		want := []string{"This will permanently remove x.localhost:", "  2 containers", "  its �[2Kregistration"}
		if strings.Join(lines, "|") != strings.Join(want, "|") {
			t.Fatalf("lines = %q", lines)
		}
	})

	t.Run("names the backup bundles that go with the home directory", func(t *testing.T) {
		plan := DescribePlan("api.acme", nil, true, 2)
		if want := "  its home directory under the control home, including 2 backup bundle(s) in backups/"; !strings.Contains(plan, want) {
			t.Fatalf("plan = %q, want a line %q", plan, want)
		}
		if plan := DescribePlan("api.acme", nil, true, 0); strings.Contains(plan, "backup") {
			t.Fatalf("plan = %q mentions backups with none", plan)
		}
	})

	t.Run("counts the bundles under a backups directory, none when it is missing", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		if n := countBackups(filepath.Join(dir, "backups")); n != 0 {
			t.Fatalf("missing: %d", n)
		}
		for _, name := range []string{"a", "b"} {
			if err := os.MkdirAll(filepath.Join(dir, "backups", name), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if n := countBackups(filepath.Join(dir, "backups")); n != 2 {
			t.Fatalf("two: %d", n)
		}
	})

	t.Run("counts none, and does not fail, when backups is a file", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		if err := os.WriteFile(filepath.Join(dir, "backups"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if n := countBackups(filepath.Join(dir, "backups")); n != 0 {
			t.Fatalf("a file: %d", n)
		}
	})

	t.Run("will not let a hostname write its own line into the plan", func(t *testing.T) {
		question := stripVT(confirmationQuestion(plainEnv(), DescribePlan("api.acme\nnothing will be removed from this machine", nil, true, 0), "api.acme"))
		lines := strings.Split(question, "\n")
		if lines[0] != "This will permanently remove api.acme nothing will be removed from this machine:" {
			t.Fatalf("first line = %q", lines[0])
		}
		count := 0
		for _, line := range lines {
			if strings.HasPrefix(line, "This will") {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("%d plan lines", count)
		}
	})

	t.Run("asks for the phrase it showed, so an odd hostname is still removable", func(t *testing.T) {
		shown := stripVT(confirmationQuestion(plainEnv(), "plan", "api.acme\x1b[2K"))
		if !strings.Contains(shown, "api.acme�[2K") {
			t.Fatalf("shown = %q", shown)
		}
		if confirmationPhrase("api.acme\x1b[2K") != "api.acme�[2K" {
			t.Fatal("phrase not filtered")
		}
		if !confirmationMatches("api.acme\x1b[2K", "api.acme�[2K") || confirmationMatches("api.acme\x1b[2K", "api.acme\x1b[2K") {
			t.Fatal("matching")
		}
	})

	t.Run("refuses without prompting when the session is non-interactive", func(t *testing.T) {
		err := confirm(plainEnv(), "Remove?", "remove it", false)
		if err == nil || err.Error() != `Interactive confirmation required: "remove it".` {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("skips the prompt entirely when the caller has already approved", func(t *testing.T) {
		if err := confirm(plainEnv(), "Remove?", "remove it", true); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("takes the typed phrase from a terminal, and refuses anything else", func(t *testing.T) {
		env := plainEnv()
		env.StdinIsTTY = true
		var shown strings.Builder
		env.Stderr = &shown
		env.Stdin = strings.NewReader("remove it\n")
		if err := confirm(env, "Remove?", "remove it", false); err != nil {
			t.Fatal(err)
		}
		env.Stdin = strings.NewReader("y\n")
		if err := confirm(env, "Remove?", "remove it", false); err == nil || err.Error() != "Confirmation did not match; operation cancelled." {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("colours the phrase only for a terminal, and never when NO_COLOR is set", func(t *testing.T) {
		env := plainEnv()
		if strings.Contains(confirmationQuestion(env, "p", "x"), "\x1b[") {
			t.Fatal("coloured for a pipe")
		}
		env.StderrIsTTY = true
		if !strings.Contains(confirmationQuestion(env, "p", "x"), "\x1b[1m\x1b[36mx\x1b[39m\x1b[22m") {
			t.Fatal("not coloured for a terminal")
		}
		env.Getenv = func(name string) (string, bool) { return "", name == "NO_COLOR" }
		if strings.Contains(confirmationQuestion(env, "p", "x"), "\x1b[") {
			t.Fatal("coloured despite NO_COLOR")
		}
	})
}
