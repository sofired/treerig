//go:build integration

// Package lock_test holds the real-process scenarios from the design doc's
// numbered list. It is an external test package — not "package lock" —
// because scenario 3a exercises lifecycle.Hold end to end, and lifecycle
// imports lock; only lock's own consumers, not lock's internal test files,
// can import something that imports lock back.
//
// The test binary re-executes itself as a helper, chosen by
// BERTH_LOCK_HELPER_ROLE. A top-level helper (one startHelper starts
// directly) reports readiness over an inherited pipe at fd 3; a helper
// spawned by another helper reports over a ready file instead, since its
// own fd 3 is often the lock itself. Cleanup kills exact PIDs only — never
// pkill/killall by name.
package lock_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/berth/internal/lifecycle"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/procid"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/run/osrun"
)

const (
	roleEnv   = "BERTH_LOCK_HELPER_ROLE"
	targetEnv = "BERTH_LOCK_HELPER_TARGET"
	// readyFileEnv is where a non-top-level hop (one that did not get fd 3
	// as a plain readiness pipe, because fd 3 is the lock it inherited)
	// reports its own PID once it is ready.
	readyFileEnv = "BERTH_LOCK_HELPER_READY_FILE"
	// nextReadyFileEnv is the ready-file path this hop hands to whatever it
	// spawns next.
	nextReadyFileEnv = "BERTH_LOCK_HELPER_NEXT_READY_FILE"
	// grandchildReadyFileEnv is threaded two hops down, from a holder to the
	// middle process to the grandchild it starts, for the Go-chain scenario.
	grandchildReadyFileEnv = "BERTH_LOCK_HELPER_GRANDCHILD_READY_FILE"
	// markerWorktreeEnv carries the worktree ID a grandchild should present
	// as BERTH_WORKTREE_ID, so the marker scan can find it.
	markerWorktreeEnv = "BERTH_LOCK_HELPER_MARKER_WORKTREE"
)

func TestMain(m *testing.M) {
	if role := os.Getenv(roleEnv); role != "" {
		os.Exit(runHelper(role))
	}
	os.Exit(m.Run())
}

// park blocks this helper until it is killed from outside. Not select{}: with
// every goroutine blocked and no timer pending, Go's runtime declares the
// process deadlocked and exits it. On Linux a cgo build (os/user links cgo)
// has an extra thread that keeps that check from firing; on macOS, or on
// Linux with CGO_ENABLED=0, nothing does, and a helper that exits closes the
// very descriptor it was started to keep open.
func park() {
	for {
		time.Sleep(time.Hour)
	}
}

// runHelper dispatches by role. "hold" and "exit" acquire the lock named by
// targetEnv directly and report their PID on fd 3, the pipe startHelper
// gave them. Every other role has its own protocol, documented on its own
// runner function, because fd 3 is not always a plain pipe — for a
// grandchild that inherited the lock, it is the lock.
func runHelper(role string) int {
	switch role {
	case "hold", "exit":
		scope := lock.Scope{Target: os.Getenv(targetEnv), Description: "helper"}
		lk := lock.New(scope, lock.Options{Watchdog: -1})
		if _, err := lk.Acquire(context.Background(), "helper "+role); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		reportReadyFD3(os.Getpid())
		if role == "exit" {
			return 0 // leaves the lock unreleased: os.Exit runs no deferred Release.
		}
		park() // "hold": park until this process is signalled from outside.

	case "hold-grandchild-inherit":
		return runHolderWithGrandchild("grandchild-keep-fd")
	case "hold-grandchild-close":
		return runHolderWithGrandchild("grandchild-close-fd")
	case "grandchild-keep-fd":
		// Never touch fd 3: keep the inherited lock descriptor open and park.
		if err := reportReadyFile(os.Getenv(readyFileEnv)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		park()
	case "grandchild-close-fd":
		// Drop the inherited descriptor before parking: the lock frees, but
		// this process — carrying the worktree's markers — keeps running.
		os.NewFile(3, "lock").Close()
		if err := reportReadyFile(os.Getenv(readyFileEnv)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		park()

	case "chain-holder":
		return runChainHolder()
	case "chain-middle":
		return runChainMiddle()
	case "chain-grandchild":
		if err := reportReadyFile(os.Getenv(readyFileEnv)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		park() // never touches fd 3; it arrived non-close-on-exec through two plain execs.

	default:
		fmt.Fprintln(os.Stderr, "unknown role: "+role)
		return 2
	}
	return 0 // not reached: a role that parks is only ever killed from outside
}

func reportReadyFD3(pid int) {
	ready := os.NewFile(3, "ready")
	fmt.Fprintf(ready, "%d\n", pid)
	ready.Close()
}

// runHolderWithGrandchild is the "hold-grandchild-inherit"/"hold-grandchild-close"
// helper (scenarios 3a/3b): it acquires the operation lock through
// lifecycle.Hold — not by calling lock.WithHeld itself — reports readiness
// on fd 3, starts one grandchild through osrun with InheritLocks so
// lock.Inherited(ctx), populated by Hold, hands it the lock's fd, then
// parks. Hold never returns, so the lock is neither released nor
// abandoned; only an external SIGKILL ends this process, exactly as a
// crashed berth would.
func runHolderWithGrandchild(grandchildRole string) int {
	scope := lock.Scope{Target: os.Getenv(targetEnv), Description: "helper"}
	if worktreeID := os.Getenv(markerWorktreeEnv); worktreeID != "" {
		scope.Markers = procid.Markers{WorktreeID: worktreeID}
	}
	l := lifecycle.NewLifecycle(lifecycle.New(), logger.Silent{})
	err := l.Hold(context.Background(),
		lifecycle.Env{Logger: logger.Silent{}, Invocation: "helper " + grandchildRole},
		lifecycle.Requests{Operation: &scope},
		func(ctx context.Context) error {
			reportReadyFD3(os.Getpid())

			self, err := os.Executable()
			if err != nil {
				return err
			}
			childEnv := buildChildEnv(grandchildRole, os.Getenv(nextReadyFileEnv), os.Getenv(markerWorktreeEnv))
			go func() {
				_, _ = osrun.New().Run(ctx, run.Spec{
					Command: self, Env: childEnv, InheritLocks: true, AllowFailure: true,
				})
			}()
			park() // this call — and so Hold, and so this process — never returns on its own.
			return nil
		})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	return 1
}

// buildChildEnv is a fresh environment for a helper this process spawns:
// this process's own environment, with every helper-protocol variable
// stripped and replaced, so a child never inherits a stale role or target.
func buildChildEnv(role, readyFile, markerWorktreeID string) []string {
	env := filterEnv(os.Environ(), roleEnv, targetEnv, readyFileEnv, nextReadyFileEnv, grandchildReadyFileEnv, markerWorktreeEnv)
	env = append(env, roleEnv+"="+role, readyFileEnv+"="+readyFile)
	if markerWorktreeID != "" {
		env = append(env, "BERTH_WORKTREE_ID="+markerWorktreeID)
	}
	return env
}

func filterEnv(env []string, keys ...string) []string {
	out := make([]string, 0, len(env))
next:
	for _, entry := range env {
		for _, key := range keys {
			if strings.HasPrefix(entry, key+"=") {
				continue next
			}
		}
		out = append(out, entry)
	}
	return out
}

// reportReadyFile writes this process's PID to path atomically (temp file,
// then rename), so a poller never observes a partial write.
func reportReadyFile(path string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// runChainHolder is the top of the Go-chain scenario (6). It holds the lock
// directly — this scenario is about fd survival across plain exec calls,
// not about Hold's own wiring, which scenario 3a already covers — reports
// readiness on fd 3, starts a middle process through osrun with
// InheritLocks, and parks.
func runChainHolder() int {
	scope := lock.Scope{Target: os.Getenv(targetEnv), Description: "helper"}
	lk := lock.New(scope, lock.Options{Watchdog: -1})
	ctx, err := lk.Acquire(context.Background(), "helper chain-holder")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	reportReadyFD3(os.Getpid())

	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	heldCtx := lock.WithHeld(ctx, lk)
	middleEnv := filterEnv(os.Environ(), roleEnv, targetEnv, readyFileEnv, nextReadyFileEnv, grandchildReadyFileEnv)
	middleEnv = append(middleEnv,
		roleEnv+"=chain-middle",
		readyFileEnv+"="+os.Getenv(nextReadyFileEnv),
		grandchildReadyFileEnv+"="+os.Getenv(grandchildReadyFileEnv),
	)
	go func() {
		_, _ = osrun.New().Run(heldCtx, run.Spec{
			Command: self, Env: middleEnv, InheritLocks: true, AllowFailure: true,
		})
	}()
	park()
	return 0 // not reached: killed from outside
}

// runChainMiddle receives the lock at fd 3 through osrun's InheritLocks, and
// starts its own grandchild with a bare exec.Command and no ExtraFiles at
// all — the shape a `docker` CLI starting its `compose` plugin takes. The
// descriptor survives anyway: exec(2) only ever closes a descriptor with
// FD_CLOEXEC set, and Go's ExtraFiles dup2's fd 3 in without setting it (see
// the design doc's "Go facts to rely on"), so this process's own fd 3 keeps
// whatever close-on-exec state it arrived with — none — straight through
// this exec, with no help from this process at all.
func runChainMiddle() int {
	if err := reportReadyFile(os.Getenv(readyFileEnv)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	grandchildEnv := filterEnv(os.Environ(), roleEnv, readyFileEnv, targetEnv, nextReadyFileEnv, grandchildReadyFileEnv)
	grandchildEnv = append(grandchildEnv, roleEnv+"=chain-grandchild", readyFileEnv+"="+os.Getenv(grandchildReadyFileEnv))
	cmd := exec.Command(self)
	cmd.Env = grandchildEnv
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stderr = os.Stderr
	// Deliberately no cmd.ExtraFiles: fd 3 must survive without it.
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	park()
	return 0 // not reached: killed from outside
}

// requireListedAsHolding asserts procid's Holding names pid among the
// processes with a descriptor on target. Linux must answer. Elsewhere Holding
// may be unsupported, as on macOS, which has no /proc to read descriptors
// from; the lock's notices then go without the list, and so does this check.
func requireListedAsHolding(t *testing.T, target string, pid int) {
	t.Helper()
	dev, ino := statFileID(t, target)
	holding, err := procid.Platform().Holding(dev, ino)
	if errors.Is(err, errors.ErrUnsupported) && runtime.GOOS != "linux" {
		t.Logf("Holding is not supported on %s, so which process holds the lock is not checked", runtime.GOOS)
		return
	}
	if err != nil {
		t.Fatalf("Holding: %v", err)
	}
	for _, p := range holding {
		if p.PID == pid {
			return
		}
	}
	t.Fatalf("expected Holding to list %d, got %v", pid, holding)
}

// waitForReadyFile polls for path to hold a PID, written by reportReadyFile.
func waitForReadyFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			var pid int
			if _, scanErr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); scanErr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s to report ready", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForGone polls until pid is absent or a zombie, or fails the test.
func waitForGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	table := procid.Platform()
	for {
		status, err := table.Inspect(pid)
		if err == nil && (status.State == procid.Absent || status.State == procid.Zombie) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for PID %d to exit", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// killPID sends sig to exactly this PID and nothing else, ignoring the
// error if it is already gone.
func killPID(pid int, sig syscall.Signal) { _ = syscall.Kill(pid, sig) }

// statFileID reports the (dev, ino) of path, for Holding.
func statFileID(t *testing.T, path string) (dev, ino uint64) {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return uint64(st.Dev), uint64(st.Ino)
}

// startHelper re-execs this test binary with role and target set, and
// blocks until it reports itself ready (holding the lock). extraEnv adds
// further NAME=value entries, for a role that needs more than target — a
// ready-file path to hand down to a grandchild, say.
func startHelper(t *testing.T, target, role string, extraEnv ...string) (*exec.Cmd, int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	readR, readW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(append(os.Environ(), roleEnv+"="+role, targetEnv+"="+target), extraEnv...)
	cmd.ExtraFiles = []*os.File{readW}
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Registered before the readiness wait, so a helper that never reports
	// ready is not left behind; through its own handle, so a helper already
	// waited on is never mistaken for whatever reused its PID.
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	readW.Close()

	pidCh := make(chan int, 1)
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := readR.Read(buf)
		if err != nil {
			errCh <- err
			return
		}
		var pid int
		fmt.Sscanf(strings.TrimSpace(string(buf[:n])), "%d", &pid)
		pidCh <- pid
	}()

	var pid int
	select {
	case pid = <-pidCh:
	case err := <-errCh:
		t.Fatalf("helper did not report ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("helper never became ready")
	}
	readR.Close()
	return cmd, pid
}

// 1. Holder SIGKILLed: the lock is free at once, and the next acquire
// succeeds after judging the holder gone.
func TestHolderSIGKILLed(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "worktree")
	cmd, pid := startHelper(t, target, "hold")

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	scope := lock.Scope{Target: target, Description: "worktree main"}
	lk := lock.New(scope, lock.Options{Watchdog: -1})
	_, err := lk.Acquire(context.Background(), "next")
	if err != nil {
		t.Fatalf("expected the SIGKILLed holder to be judged gone, got %v", err)
	}
	defer lk.Release()
}

// 2. Holder SIGSTOPped: a 1 s acquire times out with the kill -CONT advice.
func TestHolderSIGSTOPped(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "worktree")
	cmd, pid := startHelper(t, target, "hold")

	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	// Resume it so the SIGKILL startHelper's own cleanup sends is not itself
	// blocked delivering to a stopped process; that cleanup still reaps it.
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGCONT) })
	_ = cmd

	scope := lock.Scope{Target: target, Description: "worktree busy"}
	lk := lock.New(scope, lock.Options{Timeout: time.Second, Retry: 25 * time.Millisecond, Watchdog: -1})
	_, err := lk.Acquire(context.Background(), "waiter")
	if err == nil {
		t.Fatal("expected the wait to time out")
	}
	if !strings.Contains(err.Error(), "kill -CONT") {
		t.Fatalf("got %q, want kill -CONT advice", err)
	}
}

// 4. Holder exits (os.Exit) while holding: the record is unreleased, and the
// next acquire judges and proceeds.
func TestHolderExitsWhileHolding(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "worktree")
	cmd, _ := startHelper(t, target, "exit")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper: %v", err)
	}

	scope := lock.Scope{Target: target, Description: "worktree main"}
	desc, err := lock.Describe(scope, procid.Platform())
	if err != nil {
		t.Fatal(err)
	}
	if desc.Record == nil || desc.Record.ReleasedAt != nil {
		t.Fatalf("expected an unreleased record, got %+v", desc.Record)
	}

	lk := lock.New(scope, lock.Options{Watchdog: -1})
	_, err = lk.Acquire(context.Background(), "next")
	if err != nil {
		t.Fatalf("expected the exited holder to be judged gone, got %v", err)
	}
	defer lk.Release()
}

// 3a. Grandchild in its own session, holding the lock's descriptor: the
// lock stays held after the holder is SIGKILLed, Holding lists the
// grandchild's exact PID on Linux, and Acquire succeeds once it too is
// killed. The holder gets there through lifecycle.Hold + osrun +
// InheritLocks end to end — not by calling lock.WithHeld itself — so this
// covers how Hold wires held locks into the action context.
func TestGrandchildInheritsDescriptorKeepsLockHeld(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "worktree")
	grandchildReady := filepath.Join(dir, "grandchild.ready")

	cmd, holderPID := startHelper(t, target, "hold-grandchild-inherit", nextReadyFileEnv+"="+grandchildReady)
	grandchildPID := waitForReadyFile(t, grandchildReady)
	t.Cleanup(func() { killPID(grandchildPID, syscall.SIGKILL) })

	killPID(holderPID, syscall.SIGKILL)
	_ = cmd.Wait()

	scope := lock.Scope{Target: target, Description: "worktree main"}
	probe := lock.New(scope, lock.Options{Timeout: 500 * time.Millisecond, Retry: 25 * time.Millisecond, Watchdog: -1})
	if _, err := probe.Acquire(context.Background(), "probe"); err == nil {
		t.Fatal("expected the lock to still be held by the surviving grandchild")
	}

	requireListedAsHolding(t, target, grandchildPID)

	killPID(grandchildPID, syscall.SIGKILL)
	waitForGone(t, grandchildPID)

	if _, err := probe.Acquire(context.Background(), "probe2"); err != nil {
		t.Fatalf("expected the lock to free and the dead holder to be judged gone: %v", err)
	}
	probe.Release()
}

// 3b. Grandchild that closed the inherited descriptor, carrying the
// worktree's markers: the flock frees as soon as the holder dies, but
// Acquire refuses, naming the surviving marked process, until it too is
// killed.
func TestGrandchildClosesDescriptorRefusesOnSurvivor(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "worktree")
	grandchildReady := filepath.Join(dir, "grandchild.ready")
	const worktreeID = "test-worktree-3b"

	cmd, holderPID := startHelper(t, target, "hold-grandchild-close",
		nextReadyFileEnv+"="+grandchildReady,
		markerWorktreeEnv+"="+worktreeID,
	)
	grandchildPID := waitForReadyFile(t, grandchildReady)
	t.Cleanup(func() { killPID(grandchildPID, syscall.SIGKILL) })

	killPID(holderPID, syscall.SIGKILL)
	_ = cmd.Wait()

	scope := lock.Scope{Target: target, Description: "worktree main", Markers: procid.Markers{WorktreeID: worktreeID}}
	probe := lock.New(scope, lock.Options{Timeout: 3 * time.Second, Retry: 25 * time.Millisecond, Watchdog: -1})
	_, err := probe.Acquire(context.Background(), "probe")
	if err == nil {
		t.Fatal("expected a refusal naming the marked survivor")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("PID %d", grandchildPID)) {
		t.Fatalf("got %q, want it to name PID %d", err, grandchildPID)
	}

	killPID(grandchildPID, syscall.SIGKILL)
	waitForGone(t, grandchildPID)

	if _, err := probe.Acquire(context.Background(), "probe2"); err != nil {
		t.Fatalf("expected the lock to be acquired once the survivor is gone: %v", err)
	}
	probe.Release()
}

// 6. A Go chain: a middle process, started through osrun with InheritLocks,
// starts its own grandchild with a plain exec.Command and no ExtraFiles at
// all — the docker-CLI-starts-compose-plugin shape — and the grandchild
// still holds fd 3 after both the holder and the middle process are
// SIGKILLed.
func TestGoChainKeepsDescriptorAcrossExecWithoutExtraFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "worktree")
	middleReady := filepath.Join(dir, "middle.ready")
	grandchildReady := filepath.Join(dir, "grandchild.ready")

	cmd, holderPID := startHelper(t, target, "chain-holder",
		nextReadyFileEnv+"="+middleReady,
		grandchildReadyFileEnv+"="+grandchildReady,
	)
	middlePID := waitForReadyFile(t, middleReady)
	t.Cleanup(func() { killPID(middlePID, syscall.SIGKILL) })
	grandchildPID := waitForReadyFile(t, grandchildReady)
	t.Cleanup(func() { killPID(grandchildPID, syscall.SIGKILL) })

	killPID(holderPID, syscall.SIGKILL)
	_ = cmd.Wait()
	killPID(middlePID, syscall.SIGKILL)
	waitForGone(t, middlePID)

	scope := lock.Scope{Target: target, Description: "worktree main"}
	probe := lock.New(scope, lock.Options{Timeout: 500 * time.Millisecond, Retry: 25 * time.Millisecond, Watchdog: -1})
	if _, err := probe.Acquire(context.Background(), "probe"); err == nil {
		t.Fatal("expected the lock to still be held by the surviving grandchild")
	}

	requireListedAsHolding(t, target, grandchildPID)

	killPID(grandchildPID, syscall.SIGKILL)
	waitForGone(t, grandchildPID)
	if _, err := probe.Acquire(context.Background(), "probe2"); err != nil {
		t.Fatalf("expected the lock to free once the grandchild died: %v", err)
	}
	probe.Release()
}

// 9. Contradiction: the flock is free, but the sidecar record names a real,
// live process with this machine's true identity — this test process
// itself, which is certainly alive and certainly on this machine, this
// boot, this PID namespace, with its true start ticks. Acquire must not
// proceed as though the holder were merely gone; it refuses with the
// contradiction message.
func TestContradictionWithARealProcess(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "worktree")

	f, err := os.OpenFile(target, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	dev, ino := statFileID(t, target)
	f.Close()

	table := procid.Platform()
	self, err := table.Self()
	if err != nil {
		t.Fatal(err)
	}
	status, err := table.Inspect(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}

	rec := struct {
		PID        int         `json:"pid"`
		Host       string      `json:"host"`
		Command    string      `json:"command"`
		StartedAt  time.Time   `json:"startedAt"`
		File       lock.FileID `json:"file"`
		MachineID  string      `json:"machineId,omitempty"`
		BootID     string      `json:"bootId,omitempty"`
		PIDNS      string      `json:"pidNs,omitempty"`
		StartTicks *uint64     `json:"startTicks,omitempty"`
		StartUs    *int64      `json:"startUs,omitempty"`
	}{
		PID: os.Getpid(), Host: hostname, Command: "this test process",
		StartedAt: time.Now(), File: lock.FileID{Dev: dev, Ino: ino},
		MachineID: self.MachineID, BootID: self.BootID, PIDNS: self.PIDNS,
		StartTicks: status.StartTicks, StartUs: status.StartUs,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+".holder", append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	scope := lock.Scope{Target: target, Description: "worktree main"}
	lk := lock.New(scope, lock.Options{Watchdog: -1})
	start := time.Now()
	_, err = lk.Acquire(context.Background(), "next")
	if err == nil {
		t.Fatal("expected the contradiction to be refused")
	}
	if !strings.Contains(err.Error(), "Locking does not work on this filesystem") {
		t.Fatalf("got %q", err)
	}
	// The contradiction takes one more look before giving up.
	if time.Since(start) < 900*time.Millisecond {
		t.Fatalf("expected the contradiction's second look to wait, took %s", time.Since(start))
	}
}
