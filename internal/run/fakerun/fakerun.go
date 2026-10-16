// Package fakerun is a scripted run.Runner for tests: every call is
// recorded, and a handler decides each result.
package fakerun

import (
	"context"
	"strings"
	"sync"

	"github.com/sofired/treerig/internal/run"
)

// Handler answers one call. Returning a nil error with a Result lets the fake
// apply Spec.AllowFailure exactly as the real runner does.
type Handler func(ctx context.Context, spec run.Spec) (run.Result, error)

// Runner records calls and answers them with Handler, or exit 0 with no output.
type Runner struct {
	mu          sync.Mutex
	Handler     Handler
	Executables map[string]bool
	calls       []run.Spec
}

// Run records the call, then answers it.
func (r *Runner) Run(ctx context.Context, spec run.Spec) (run.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, spec)
	handler := r.Handler
	r.mu.Unlock()
	if handler == nil {
		return run.Result{}, nil
	}
	result, err := handler(ctx, spec)
	if err != nil {
		return result, err
	}
	return run.Finish(spec, result)
}

// Which answers from Executables.
func (r *Runner) Which(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Executables[name]
}

// Calls returns every recorded call so far.
func (r *Runner) Calls() []run.Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]run.Spec(nil), r.calls...)
}

// Argv renders each recorded call as "command arg arg…", for compact assertions.
func (r *Runner) Argv() []string {
	calls := r.Calls()
	lines := make([]string, len(calls))
	for i, spec := range calls {
		lines[i] = strings.TrimSpace(spec.Command + " " + strings.Join(spec.Args, " "))
	}
	return lines
}

// Script answers calls by the first matching prefix of "command arg…";
// unmatched calls exit 0 with no output.
func Script(answers map[string]run.Result) Handler {
	return func(_ context.Context, spec run.Spec) (run.Result, error) {
		line := strings.TrimSpace(spec.Command + " " + strings.Join(spec.Args, " "))
		best := ""
		for prefix := range answers {
			if strings.HasPrefix(line, prefix) && len(prefix) > len(best) {
				best = prefix
			}
		}
		if best == "" {
			return run.Result{}, nil
		}
		return answers[best], nil
	}
}
