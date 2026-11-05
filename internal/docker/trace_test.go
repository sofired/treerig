package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/run/fakerun"
)

func TestTraced(t *testing.T) {
	t.Run("says the command docker.Run builds, and then runs it", func(t *testing.T) {
		fake := answering(run.Result{})
		var said []string
		runner := Traced(fake, func(line string) { said = append(said, line) })
		if _, err := Run(context.Background(), runner, []string{"compose", "--project-name", "p", "up", "--detach"}, run.Spec{}); err != nil {
			t.Fatal(err)
		}
		if want := []string{"docker compose --project-name p up --detach"}; !equalLines(said, want) {
			t.Fatalf("said %q, want %q", said, want)
		}
		if got := len(fake.Calls()); got != 1 {
			t.Fatalf("the inner runner ran %d times, want 1", got)
		}
	})

	t.Run("says nothing for a command that is not docker.Run's", func(t *testing.T) {
		var said []string
		runner := Traced(&fakerun.Runner{}, func(line string) { said = append(said, line) })
		// A hook that happens to be called docker: it never sets InheritLocks.
		if _, err := runner.Run(context.Background(), run.Spec{Command: "docker", Args: []string{"compose", "exec", "web", "TOKEN=abc"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := runner.Run(context.Background(), run.Spec{Command: "git", Args: []string{"status"}, InheritLocks: true}); err != nil {
			t.Fatal(err)
		}
		if len(said) != 0 {
			t.Fatalf("said %q", said)
		}
	})
}

func TestCommandLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"joins plain arguments", []string{"ps", "-aq", "--filter", "label=com.docker.compose.project=p"}, "docker ps -aq --filter label=com.docker.compose.project=p"},
		{"quotes an argument a shell would split", []string{"inspect", "--format", "{{json .State}}"}, "docker inspect --format '{{json .State}}'"},
		{"stops at a double dash", []string{"compose", "exec", "web", "--", "psql", "-c", "select 1"}, "docker compose exec web -- ..."},
		{"redacts a pair and a URL's userinfo", []string{"run", "-e", "API_TOKEN=abc", "https://u:p@example.com/x"}, "docker run -e API_TOKEN=<redacted> https://<redacted>@example.com/x"},
		{"redacts a value with spaces whole before quoting it", []string{"build", "--build-arg", "MSG=it's a secret", "--password", "a b"}, "docker build --build-arg MSG=<redacted> --password <redacted>"},
		{"redacts a --flag=value with a secret name", []string{"login", "--api-key=abc def"}, "docker login --api-key=<redacted>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CommandLine(tc.args); got != tc.want {
				t.Fatalf("CommandLine = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTracedKeepsTheInnerRunnersQuiesce(t *testing.T) {
	inner := &quiescingRunner{}
	runner := Traced(inner, func(string) {})
	q, ok := runner.(interface{ Quiesce(context.Context) error })
	if !ok {
		t.Fatal("Traced hides Quiesce")
	}
	if err := q.Quiesce(context.Background()); err != nil || inner.quiesced != 1 {
		t.Fatalf("Quiesce = %v, reached the inner runner %d time(s)", err, inner.quiesced)
	}
}

// quiescingRunner is a runner that counts Quiesce calls.
type quiescingRunner struct {
	fakerun.Runner
	quiesced int
}

// Quiesce counts the call.
func (r *quiescingRunner) Quiesce(context.Context) error { r.quiesced++; return nil }

func TestRunStillInheritsLocks(t *testing.T) {
	// Traced says only the commands Run builds, by InheritLocks.
	fake := answering(run.Result{})
	if _, err := Run(context.Background(), fake, []string{"ps"}, run.Spec{}); err != nil {
		t.Fatal(err)
	}
	if calls := fake.Calls(); len(calls) != 1 || !calls[0].InheritLocks || calls[0].Command != "docker" {
		t.Fatalf("Run built %+v, want docker with InheritLocks", calls)
	}
}

// equalLines reports whether two lists of lines are the same.
func equalLines(a, b []string) bool { return strings.Join(a, "\n") == strings.Join(b, "\n") }
