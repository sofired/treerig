package teardown_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/binstore"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/teardown"
)

// helperExitCode is what a writer process exits with when Save fails.
const helperExitCode = 3

// TestMultiProcessHelper is the writer process TestKilledWriters starts: it is
// the test binary re-run with MP_CONTROL set, and does nothing otherwise. It
// saves pins in a loop until it is killed.
func TestMultiProcessHelper(t *testing.T) {
	control := os.Getenv("MP_CONTROL")
	if control == "" {
		t.Skip("helper process for TestKilledWriters")
	}
	id, err := strconv.Atoi(os.Getenv("MP_ID"))
	if err != nil {
		t.Fatal(err)
	}
	l := layout.ControlLayout(control)
	for round := 0; ; round++ {
		content := fmt.Sprintf("writer %d round %d", id, round)
		release, checksums := "1.0.0", "shared"
		if id%2 == 1 {
			release, checksums = fmt.Sprintf("2.%d.0", id), fmt.Sprintf("own %d", id)
		}
		incoming := releasePin(release, checksums, map[string]string{teardown.ReleasePlatforms[id%4]: content})
		existing, found, err := teardown.ReadPin(l)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			os.Exit(helperExitCode)
		}
		if _, err := teardown.Save(l, os.Getenv("MP_HOME"), []byte(content), teardown.Update(existing, found, incoming).Pin, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "save:", err)
			os.Exit(helperExitCode)
		}
	}
}

func TestKilledWriters(t *testing.T) {
	t.Run("writer processes killed at random leave a whole pin and nothing the next sweep cannot clear", func(t *testing.T) {
		if testing.Short() {
			t.Skip("starts writer processes for about three seconds")
		}
		l, home := newControl(t)
		if err := files.SecureTeardownDirectory(l, home); err != nil {
			t.Fatal(err)
		}
		const writers = 6
		start := func(id int) *exec.Cmd {
			cmd := exec.Command(os.Args[0], "-test.run=^TestMultiProcessHelper$")
			cmd.Env = append(os.Environ(), "MP_CONTROL="+l.ControlHome, "MP_HOME="+home, "MP_ID="+strconv.Itoa(id))
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			return cmd
		}
		// stop kills cmd and reports a failure only if it had already failed
		// by itself.
		stop := func(cmd *exec.Cmd) {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			err := cmd.Wait()
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == helperExitCode {
				t.Errorf("a writer process failed by itself")
			}
		}
		cmds := make([]*exec.Cmd, writers)
		for id := range cmds {
			cmds[id] = start(id)
		}
		t.Cleanup(func() {
			for _, cmd := range cmds {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		})

		dangling := 0
		for round, deadline := 0, time.Now().Add(3*time.Second); time.Now().Before(deadline); round++ {
			pin, found, err := teardown.ReadPin(l)
			if err != nil {
				t.Fatalf("a reader met a broken pin: %v", err)
			}
			if found {
				for _, hash := range pin.BinaryHashes() {
					if _, statErr := os.Stat(teardown.BinaryPath(l, hash)); statErr != nil {
						dangling++
					}
				}
			}
			victim := round % writers
			stop(cmds[victim])
			cmds[victim] = start(victim)
			time.Sleep(20 * time.Millisecond)
		}
		for _, cmd := range cmds {
			stop(cmd)
		}
		if dangling != 0 {
			t.Errorf("a pin named a binary that was not there, %d times", dangling)
		}

		pin, found, err := teardown.ReadPin(l)
		if err != nil || !found {
			t.Fatalf("final ReadPin: %v, %v", found, err)
		}
		named := map[string]bool{"pin.json": true}
		for _, hash := range pin.BinaryHashes() {
			data, err := os.ReadFile(teardown.BinaryPath(l, hash))
			if err != nil || binstore.Hash(data) != hash {
				t.Errorf("the final pin's binary %s: %v", hash, err)
			}
			named[hash] = true
		}
		// A sweep far enough ahead for the grace window to have passed clears
		// every orphan and every temporary file the killed writers left.
		if _, err := teardown.Sweep(l, pin, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(l.TeardownDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !named[entry.Name()] {
				t.Errorf("left after the sweep: %s", entry.Name())
			}
			delete(named, entry.Name())
		}
		for name := range named {
			t.Errorf("missing after the sweep: %s", name)
		}
	})
}
