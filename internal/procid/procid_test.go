package procid

import "testing"

func TestMarked(t *testing.T) {
	const project = "berth-0123456789abcdef01234567"
	const worktree = "2b41d9d4-ed88-49c1-be9c-5439bdf6a431"
	markers := Markers{Project: project, WorktreeID: worktree}

	t.Run("finds this worktree's processes by whole project token or exact environment marker only", func(t *testing.T) {
		scanned := []Proc{
			{PID: 1, Args: []string{"docker", "compose", "--project-name", project, "up"}},
			{PID: 2, Args: []string{"sh", "seed.sh"}, Env: []string{"BERTH_WORKTREE_ID=" + worktree}, EnvRead: true},
			{PID: 3, Args: []string{"docker", "exec", "x"}, Env: []string{"COMPOSE_PROJECT_NAME=" + project}, EnvRead: true},
			{PID: 4, Args: []string{"docker", "compose", "--project-name", project + "0"}},
			{PID: 5, Args: []string{"--project-name=" + project}},
			{PID: 6, Args: []string{"sh"}, Env: []string{"BERTH_WORKTREE_ID=" + worktree + "x", "OTHER=1"}, EnvRead: true},
			{PID: 7, Args: []string{"sh"}},
		}
		got := Marked(scanned, markers)
		want := []int{1, 2, 3}
		if len(got) != len(want) {
			t.Fatalf("got %v", got)
		}
		for i, p := range got {
			if p.PID != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})
}
