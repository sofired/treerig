//go:build integration

package cli_test

// `run`, through the real binary (#94): a registered fixture worktree with a
// fake docker and no real Docker, the same one the `up` tests use. Signals
// are sent to the process for real; the terminal tests run the binary under
// a real pty, playing the shell around it.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/sofired/berth/internal/run/ptytest"
)

// syncBuffer is a strings.Builder that can be read while a child process is
// still writing to it.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

// Write appends p.
func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// String is everything written so far.
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// runScripts are the fixture's commands: name, then the script body. The
// state directory each one records into is filled in by newRunFixture.
var runScripts = map[string]string{
	// Prints its arguments on both streams; exits with its first, if any.
	"emit": "echo \"out:$*\"\necho \"err:$*\" >&2\nexit \"${1:-0}\"\n",
	// Says what the command was given.
	"envdump": "echo \"command=$BERTH_COMMAND\"\necho \"stage=${BERTH_STAGE-unset}\"\necho \"worktree=$BERTH_WORKTREE_ID\"\n" +
		"echo \"url=$BERTH_URL\"\necho \"api=$BERTH_SERVICE_URL_API\"\necho \"pwd=$(pwd)\"\necho \"argc=$#\"\n",
	"killed": "kill -9 $$\n",
	// Holds until released, with a grandchild in its group.
	"hold": "trap 'echo INT > \"$STATE/got\"; exit 0' INT\ntrap 'echo TERM > \"$STATE/got\"; exit 0' TERM\ntrap 'echo HUP > \"$STATE/got\"; exit 0' HUP\n" +
		"echo started > \"$STATE/started\"\nsh -c 'echo $$ > \"$0/hung.pid\"; exec sleep 60' \"$STATE\" &\n" +
		"while [ ! -e \"$STATE/release\" ]; do sleep 0.1; done\n",
	// Prompts, like `terraform apply` without -auto-approve.
	"ask": "printf 'Approve? '\nread answer\necho \"got:$answer\"\n[ \"$answer\" = yes ]\n",
	// A grandchild that ignores SIGINT (as a background job of a non-interactive
	// shell does), so only berth's drain of the group can stop it. The leader
	// execs sleep rather than waiting in the shell: a SIGINT during `wait`
	// makes macOS's /bin/sh (bash 3.2) exit 129 instead of dying by the
	// signal, while an exec'd sleep dies by SIGINT under every shell.
	"stubborn": "sleep 60 &\necho $! > \"$STATE/hung.pid\"\necho ready\nexec sleep 60\n",
}

// newRunFixture is a worktree that has been through `up`, with a berth.toml
// that also declares the commands above.
func newRunFixture(t *testing.T, binary string, register bool) upFixture {
	t.Helper()
	f := newUpFixture(t)
	if err := os.MkdirAll(filepath.Join(f.checkout, "cmds"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range runScripts {
		script := "#!/bin/sh\nSTATE=" + strconv.Quote(f.state) + "\n" + body
		if err := os.WriteFile(filepath.Join(f.checkout, "cmds", name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.checkout, "cmds", "notexec"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toml, err := os.OpenFile(filepath.Join(f.checkout, "berth.toml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer toml.Close()
	if _, err := toml.WriteString(`
[commands]
emit = "./cmds/emit"
envdump = "./cmds/envdump"
missing = "./cmds/missing"
notexec = "./cmds/notexec"
killed = "./cmds/killed"
ask = { command = "./cmds/ask", kill-after = "2s" }
stubborn = { command = "./cmds/stubborn", kill-after = "2s" }
[commands.hold]
command = "./cmds/hold"
kill-after = "2s"
[commands.holdfree]
command = "./cmds/hold"
lock = false
kill-after = "2s"
`); err != nil {
		t.Fatal(err)
	}
	if register {
		cmd := f.command(binary, "up")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("up: %v\n%s", err, output)
		}
	}
	return f
}

// worktreeID is the fixture worktree's UUID, from its identity markers.
func (f upFixture) worktreeID(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.checkout, ".git", "berth", "worktree-id"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// lockHeld reports whether the lock file at path is flock-held by anyone.
func lockHeld(t *testing.T, path string) bool {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return true
	}
	return false
}

// exitStatus is the exit code of a finished run; -1 if it did not exit.
func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

// runFixture runs the binary in f's checkout to completion, returning what it
// wrote to each stream and its exit code.
func runFixture(t *testing.T, f upFixture, binary string, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := f.command(binary, args...)
	cmd.Env = append(cmd.Env, env...)
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	return out.String(), errOut.String(), exitStatus(err)
}

func TestRunThroughTheBinary(t *testing.T) {
	binary := buildBinary(t)
	f := newRunFixture(t, binary, true)

	t.Run("prints only the command's own output on each stream, so it can be piped", func(t *testing.T) {
		stdout, stderr, code := runFixture(t, f, binary, nil, "run", "emit", "--", "0", "two words")
		if stdout != "out:0 two words\n" || stderr != "err:0 two words\n" || code != 0 {
			t.Fatalf("stdout = %q, stderr = %q, code = %d", stdout, stderr, code)
		}
	})

	t.Run("passes the command's exit code through, in a line that says command", func(t *testing.T) {
		stdout, stderr, code := runFixture(t, f, binary, nil, "run", "emit", "--", "3")
		if stdout != "out:3\n" || code != 3 {
			t.Fatalf("stdout = %q, code = %d", stdout, code)
		}
		if want := "err:3\n[berth] ERROR: The emit command failed (exit 3).\n"; stderr != want {
			t.Fatalf("stderr = %q, want %q", stderr, want)
		}
	})

	t.Run("reports a command that cannot start with the shell's 127 and 126, and one a signal killed with 128 plus it", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			code int
			want string
		}{
			{"missing", 127, "[berth] ERROR: The missing command could not be started: `" + filepath.Join(f.checkout, "cmds", "missing") + "` does not exist"},
			{"notexec", 126, "[berth] ERROR: The notexec command could not be started: `" + filepath.Join(f.checkout, "cmds", "notexec") + "` is not executable (EACCES)."},
			{"killed", 137, "[berth] ERROR: The killed command was terminated by SIGKILL."},
		} {
			_, stderr, code := runFixture(t, f, binary, nil, "run", tc.name)
			if code != tc.code || !strings.Contains(stderr, tc.want) {
				t.Errorf("%s: code = %d, stderr = %q, want %d and %q", tc.name, code, stderr, tc.code, tc.want)
			}
		}
	})

	t.Run("gives the command the worktree's environment, BERTH_COMMAND, and no BERTH_STAGE even when it inherited one", func(t *testing.T) {
		stdout, _, code := runFixture(t, f, binary, []string{"BERTH_STAGE=prepare", "BERTH_COMMAND=stale"}, "run", "envdump", "--", "a", "b")
		if code != 0 {
			t.Fatalf("code = %d", code)
		}
		want := "command=envdump\nstage=unset\nworktree=" + f.worktreeID(t) +
			"\nurl=http://feature-login.acme.localhost:18080\napi=http://feature-login.acme.localhost:18080\npwd=" + f.checkout + "\nargc=2\n"
		if stdout != want {
			t.Fatalf("stdout =\n%s\nwant\n%s", stdout, want)
		}
	})

	t.Run("without a name, or with an unknown one, lists the configured commands on stderr and exits 1", func(t *testing.T) {
		const defined = "berth.toml defines emit, envdump, missing, notexec, killed, ask, stubborn, hold, holdfree."
		for _, tc := range []struct {
			args []string
			want string
		}{
			{[]string{"run"}, "[berth] ERROR: No command given: " + defined + "\n"},
			{[]string{"run", "nope"}, "[berth] ERROR: No command \"nope\": " + defined + "\n"},
			{[]string{"run", "--", "emit"}, "[berth] ERROR: No command given: " + defined + "\n"},
		} {
			stdout, stderr, code := runFixture(t, f, binary, nil, tc.args...)
			if stdout != "" || stderr != tc.want || code != 1 {
				t.Errorf("%v: stdout = %q, stderr = %q, code = %d, want %q", tc.args, stdout, stderr, code, tc.want)
			}
		}
	})

	t.Run("refuses arguments between the name and --, running nothing", func(t *testing.T) {
		stdout, stderr, code := runFixture(t, f, binary, nil, "run", "emit", "3", "--", "4")
		if stdout != "" || code != 1 || !strings.Contains(stderr, `takes one command name before "--"`) {
			t.Fatalf("stdout = %q, stderr = %q, code = %d", stdout, stderr, code)
		}
	})

	t.Run("refuses a locked command inside its own worktree's environment before taking the lock, and runs nothing", func(t *testing.T) {
		started := time.Now()
		stdout, stderr, code := runFixture(t, f, binary, []string{"BERTH_WORKTREE_ID=" + f.worktreeID(t)}, "run", "emit", "--", "3")
		if stdout != "" || code != 1 || !strings.Contains(stderr, "runs inside feature-login.acme.localhost's own hooks or commands, which may hold its lock") {
			t.Fatalf("stdout = %q, stderr = %q, code = %d", stdout, stderr, code)
		}
		if elapsed := time.Since(started); elapsed > 10*time.Second {
			t.Fatalf("took %v: it waited on the lock", elapsed)
		}
	})

	t.Run("runs from another worktree's environment", func(t *testing.T) {
		_, _, code := runFixture(t, f, binary, []string{"BERTH_WORKTREE_ID=00000000-0000-4000-8000-000000000000"}, "run", "emit")
		if code != 0 {
			t.Fatalf("code = %d", code)
		}
	})

	t.Run("refuses a worktree that is not registered, running nothing", func(t *testing.T) {
		fresh := newRunFixture(t, binary, false)
		stdout, stderr, code := runFixture(t, fresh, binary, nil, "run", "emit")
		if want := "[berth] ERROR: This worktree is not registered. Run 'berth up' first.\n"; stdout != "" || stderr != want || code != 1 {
			t.Fatalf("stdout = %q, stderr = %q, code = %d", stdout, stderr, code)
		}
	})
}

// startHold starts `berth run <name>` for the fixture's hold script and
// returns once the command is running, with the berth process and its
// stderr as it goes. A cleanup registered on t releases the command and
// reaps everything it left, whether or not the test got that far.
func startHold(t *testing.T, f upFixture, binary, name string) (*exec.Cmd, *syncBuffer) {
	t.Helper()
	for _, marker := range []string{"started", "release", "hung.pid", "got"} {
		_ = os.Remove(filepath.Join(f.state, marker))
	}
	cmd := f.command(binary, "run", name)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The command's grandchild outlives a berth killed here, holding its
	// stderr pipe open, so it goes first; Wait would otherwise sit out its
	// whole sleep.
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(f.state, "release"), nil, 0o600)
		_ = cmd.Process.Kill()
		if data, err := os.ReadFile(filepath.Join(f.state, "hung.pid")); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				_ = unix.Kill(pid, syscall.SIGKILL)
			}
		}
		_ = cmd.Wait()
	})
	waitForFile(t, filepath.Join(f.state, "started"))
	return cmd, stderr
}

func TestRunLocking(t *testing.T) {
	binary := buildBinary(t)

	t.Run("a locked command makes `up` wait, naming the command, until it finishes", func(t *testing.T) {
		f := newRunFixture(t, binary, true)
		hold, _ := startHold(t, f, binary, "hold")
		if !lockHeld(t, f.worktreeLock(t)) {
			t.Fatal("the worktree lock is free while a locked command runs")
		}

		up := f.command(binary, "up")
		stderr := &syncBuffer{}
		up.Stderr = stderr
		if err := up.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = up.Process.Kill(); _ = up.Wait() })
		deadline := time.Now().Add(30 * time.Second)
		for !strings.Contains(stderr.String(), "Waiting for the worktree feature-login lock.") {
			if time.Now().After(deadline) {
				t.Fatalf("`up` never said it was waiting; stderr = %q", stderr.String())
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !strings.Contains(stderr.String(), `The holder, "berth run hold" (PID `+strconv.Itoa(hold.Process.Pid)+" on ") {
			t.Fatalf("`up` did not name the command holding the lock: %q", stderr.String())
		}

		if err := os.WriteFile(filepath.Join(f.state, "release"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := hold.Wait(); err != nil {
			t.Fatalf("run: %v", err)
		}
		if err := up.Wait(); err != nil {
			t.Fatalf("up: %v\n%s", err, stderr.String())
		}
		requireReleased(t, f.worktreeLock(t))
	})

	t.Run("a lock = false command does not make `up` wait, and takes no lock", func(t *testing.T) {
		f := newRunFixture(t, binary, true)
		startHold(t, f, binary, "holdfree")
		if lockHeld(t, f.worktreeLock(t)) {
			t.Fatal("a lock = false command holds the worktree lock")
		}
		up := f.command(binary, "up")
		var stderr syncBuffer
		up.Stderr = &stderr
		if err := up.Run(); err != nil {
			t.Fatalf("up: %v\n%s", err, stderr.String())
		}
		if strings.Contains(stderr.String(), "Waiting for") {
			t.Fatalf("`up` waited: %q", stderr.String())
		}
	})
}

// A signal reaches the command and everything it started, and the process
// dies by that signal only once they are gone, with the lock released: the
// same whether or not the command took the lock, and with no terminal.
func TestRunInterrupted(t *testing.T) {
	binary := buildBinary(t)
	// A signal ignored when berth starts stays ignored (lifecycle.Start
	// leaves it alone, as a nohup'd process expects), and the test may itself
	// be running under nohup, whose ignored SIGHUP every child inherits. A
	// caught signal is reset to its default across exec, so catching them
	// here hands berth the defaults whatever this process was started with.
	caught := make(chan os.Signal, 1)
	signal.Notify(caught, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	t.Cleanup(func() { signal.Stop(caught) })
	for _, name := range []string{"hold", "holdfree"} {
		for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
			t.Run(fmt.Sprintf("dies by %v during %s, with the command's group gone and the lock released", sig, name), func(t *testing.T) {
				f := newRunFixture(t, binary, true)
				cmd, stderr := startHold(t, f, binary, name)
				hung, _ := strconv.Atoi(waitForFile(t, filepath.Join(f.state, "hung.pid")))

				if err := cmd.Process.Signal(sig); err != nil {
					t.Fatal(err)
				}
				err := cmd.Wait()
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("exit = %v\n%s", err, stderr.String())
				}
				if status := exit.Sys().(syscall.WaitStatus); !status.Signaled() || status.Signal() != sig {
					t.Fatalf("status = %v, want death by %v\n%s", status, sig, stderr.String())
				}
				if alive(hung) {
					t.Fatalf("the command's group survived: %d is alive", hung)
				}
				// SIGINT and SIGTERM are forwarded as received; a hangup means
				// the operator is gone, so the command is asked to stop with
				// SIGTERM, which terraform and docker wind down on.
				want := map[syscall.Signal]string{syscall.SIGINT: "INT", syscall.SIGTERM: "TERM", syscall.SIGHUP: "TERM"}[sig]
				if got := waitForFile(t, filepath.Join(f.state, "got")); got != want {
					t.Fatalf("the command was sent SIG%s, want SIG%s", got, want)
				}
				requireReleased(t, f.worktreeLock(t))
			})
		}
	}
}

// startBerthOnTerminal runs `berth run args…` in f's checkout under a real
// pty, as its session leader, so it is the terminal's foreground job and
// hands the terminal to the command.
func startBerthOnTerminal(t *testing.T, f upFixture, binary string, args ...string) (ptmx *os.File, pid int) {
	t.Helper()
	cmd := f.command(binary, args...)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	pid = cmd.Process.Pid
	t.Cleanup(func() { _ = ptmx.Close() })
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	// A grandchild that outlived a failed test goes too.
	t.Cleanup(func() {
		if data, err := os.ReadFile(filepath.Join(f.state, "hung.pid")); err == nil {
			if hung, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && hung > 0 {
				_ = unix.Kill(hung, syscall.SIGKILL)
			}
		}
	})
	return ptmx, pid
}

func TestRunOnATerminal(t *testing.T) {
	binary := buildBinary(t)

	t.Run("a prompt reads what is typed, the lock is held meanwhile and released after, and the exit code is the command's", func(t *testing.T) {
		for _, tc := range []struct {
			answer   string
			wantCode int
		}{{"yes", 0}, {"no", 1}} {
			f := newRunFixture(t, binary, true)
			ptmx, pid := startBerthOnTerminal(t, f, binary, "run", "ask")
			ptytest.ReadUntil(t, ptmx, "Approve? ")
			if !lockHeld(t, f.worktreeLock(t)) {
				t.Fatal("the worktree lock is free while the command prompts")
			}

			if _, err := ptmx.Write([]byte(tc.answer + "\n")); err != nil {
				t.Fatal(err)
			}
			ptytest.ReadUntil(t, ptmx, "got:"+tc.answer)
			// Read before the wait: once the slave side closes, macOS may
			// drop output nobody has read yet.
			if tc.wantCode != 0 {
				ptytest.ReadUntil(t, ptmx, "The ask command failed (exit 1).")
			}
			status := ptytest.WaitHelperEvent(t, pid)
			if !status.Exited() || status.ExitStatus() != tc.wantCode {
				t.Fatalf("answer %q: status = %+v, want exit %d", tc.answer, status, tc.wantCode)
			}
			requireReleased(t, f.worktreeLock(t))
		}
	})

	t.Run("Ctrl-C reaches the command and what it started, and once they are gone the worktree is unlocked", func(t *testing.T) {
		f := newRunFixture(t, binary, true)
		ptmx, pid := startBerthOnTerminal(t, f, binary, "run", "stubborn")
		ptytest.ReadUntil(t, ptmx, "ready")
		hung, _ := strconv.Atoi(waitForFile(t, filepath.Join(f.state, "hung.pid")))
		ptytest.WaitForegroundHandoff(t, ptmx, pid)
		if !lockHeld(t, f.worktreeLock(t)) {
			t.Fatal("the worktree lock is free while the command runs")
		}

		status := ptytest.SendTerminalSignal(t, ptmx, pid, 0x03) // Ctrl-C
		// The terminal signalled the command's group, not berth, so berth
		// reports how the command ended and exits with 128 plus SIGINT.
		if !status.Exited() || status.ExitStatus() != 128+int(syscall.SIGINT) {
			// What berth printed names the cause: "terminated by SIGHUP" is
			// the command ending that way, anything else with 129 is berth
			// itself having been hung up.
			t.Fatalf("status = %+v, want exit %d\nberth printed: %q", status, 128+int(syscall.SIGINT), ptytest.Transcript(ptmx))
		}
		ptytest.ReadUntil(t, ptmx, "The stubborn command was terminated by SIGINT.")
		if alive(hung) {
			t.Fatalf("the command's grandchild, which ignores SIGINT, survived: %d is alive", hung)
		}
		requireReleased(t, f.worktreeLock(t))
	})
}
