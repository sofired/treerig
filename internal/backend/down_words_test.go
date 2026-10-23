package backend

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/run/fakerun"
)

func TestDownTranscript(t *testing.T) {
	ctx := context.Background()

	t.Run("captures Compose's transcript, and shows nothing, without -v", func(t *testing.T) {
		fake := newFake()
		rt, _ := newRuntime(t, fake)
		if err := rt.Down(ctx, ref); err != nil {
			t.Fatal(err)
		}
		if downs := fake.composeCalls("down"); len(downs) != 1 || downs[0].Output != run.Capture {
			t.Fatalf("down calls = %+v, want one with output Capture", downs)
		}
	})

	t.Run("shows it on stderr with -v, and never the warning that nothing was found", func(t *testing.T) {
		fake := newFake()
		rt, _ := newRuntimeWith(t, fake, func(o *Options) { o.Verbose = true })
		if err := rt.Down(ctx, ref); err != nil {
			t.Fatal(err)
		}
		downs := fake.composeCalls("down")
		if len(downs) != 1 || downs[0].Output != run.TeeToStderr {
			t.Fatalf("down calls = %+v, want one with output TeeToStderr", downs)
		}
		hide := downs[0].HideLine
		if hide == nil {
			t.Fatal("Down hides no line")
		}
		warning := `time="2026-10-23T21:13:32Z" level=warning msg="Warning: No resource found to remove for project \"p\"."`
		if !hide(warning) || hide("Container web  Stopped") {
			t.Errorf("HideLine(warning) = %v, HideLine(other) = %v, want true and false", hide(warning), hide("Container web  Stopped"))
		}
	})

	t.Run("quotes what Compose said in a failure, without the warning that nothing was found", func(t *testing.T) {
		fake := newFake()
		fake.downExitCode = 1
		fake.downStderr = `time="t" level=warning msg="Warning: No resource found to remove for project \"p\"."` + "\nError response from daemon: boom"
		rt, _ := newRuntime(t, fake)
		err := rt.Down(ctx, ref)
		if err == nil || !strings.Contains(err.Error(), "boom") || strings.Contains(err.Error(), "No resource found") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestUpTranscript(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		fake := newFake()
		rt, controlHome := routedRuntime(t, fake, nil)
		rt.options.Verbose = verbose
		ensureHome(t, controlHome)
		if err := rt.Up(context.Background(), ref, nil); err != nil {
			t.Fatal(err)
		}
		want := run.Capture
		if verbose {
			want = run.TeeToStderr
		}
		if up := fake.composeCalls("up"); len(up) != 1 || up[0].Output != want {
			t.Errorf("verbose %v: up calls = %+v, want one with output %v", verbose, up, want)
		}
	}
}

func TestLeftover(t *testing.T) {
	// answering lists the given states for `ps --all` and the given ids for
	// `network ls`.
	answering := func(states, networks string) *fakerun.Runner {
		return &fakerun.Runner{Handler: func(_ context.Context, spec run.Spec) (run.Result, error) {
			switch {
			case spec.Args[0] == "ps" && slices.Contains(spec.Args, "{{.State}}") && slices.Contains(spec.Args, "--all"):
				return run.Result{Stdout: states}, nil
			case slices.Equal(spec.Args[:3], []string{"network", "ls", "-q"}):
				return run.Result{Stdout: networks}, nil
			}
			t.Errorf("unexpected call %v", spec.Args)
			return run.Result{}, nil
		}}
	}
	for _, tc := range []struct {
		name             string
		states, networks string
		want             Leftover
		wantAny          bool
	}{
		{"nothing left", "", "", Leftover{}, false},
		{"a running container", "running\n", "", Leftover{Running: true, Containers: true}, true},
		{"a restarting one counts as running", "exited\nrestarting\n", "", Leftover{Running: true, Containers: true}, true},
		{"only a stopped container", "exited\n", "", Leftover{Containers: true}, true},
		{"only a network", "", "n1\n", Leftover{Networks: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := answering(tc.states, tc.networks)
			rt, _ := newRuntime(t, newFake())
			rt.options.Runner = fake
			got, err := rt.Leftover(context.Background(), ref)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want || got.Any() != tc.wantAny {
				t.Fatalf("Leftover = %+v (Any %v), want %+v (Any %v)", got, got.Any(), tc.want, tc.wantAny)
			}
			if n := len(fake.Calls()); n != 2 {
				t.Errorf("asked Docker %d times, want 2: %v", n, fake.Argv())
			}
		})
	}
}

func TestUpFailureCarriesWhatComposeSaid(t *testing.T) {
	fake := newFake()
	var lines []string
	for i := 1; i <= 70; i++ {
		lines = append(lines, "step "+strconv.Itoa(i))
	}
	fake.upExitCode, fake.upStderr = 3, strings.Join(lines, "\n")+"\n"
	rt, controlHome := routedRuntime(t, fake, nil)
	ensureHome(t, controlHome)
	err := rt.Up(context.Background(), ref, nil)
	var failed *UpError
	if !errors.As(err, &failed) {
		t.Fatalf("err = %T %v, want an *UpError", err, err)
	}
	if failed.Result.ExitCode != 3 || strings.Count(failed.Result.Stderr, "step ") != 70 {
		t.Errorf("Result = exit %d with %d lines, want the whole transcript", failed.Result.ExitCode, strings.Count(failed.Result.Stderr, "step "))
	}
	if clierr.Code(err) != 1 || !strings.Contains(err.Error(), "step 70") || strings.Contains(err.Error(), "step 1\n") {
		t.Errorf("error = %q, code %d, want the end of the transcript", err, clierr.Code(err))
	}
}

func TestLeftoverDeadline(t *testing.T) {
	hung := &fakerun.Runner{Handler: func(ctx context.Context, _ run.Spec) (run.Result, error) {
		<-ctx.Done()
		return run.Result{}, ctx.Err()
	}}
	rt, _ := newRuntimeWith(t, newFake(), func(o *Options) { o.Timeouts.Probe = 20 * time.Millisecond; o.Runner = hung })

	t.Run("a Docker that does not answer in time is a timeout, not a bare context error", func(t *testing.T) {
		_, err := rt.Leftover(context.Background(), ref)
		if !errors.Is(err, ErrLeftoverTimeout) || strings.Contains(err.Error(), "context deadline exceeded") {
			t.Fatalf("err = %v, want ErrLeftoverTimeout", err)
		}
	})

	t.Run("a cancelled caller is not a timeout", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := rt.Leftover(ctx, ref); errors.Is(err, ErrLeftoverTimeout) {
			t.Fatalf("err = %v, the caller cancelled", err)
		}
	})
}
