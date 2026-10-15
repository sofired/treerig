// Package docker runs the `docker` CLI — never an SDK — and tells Docker
// being unavailable apart from Docker refusing a request.
package docker

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/run"
)

// UnavailableError is Docker unable to do what was asked of it — not
// installed, daemon down, the socket not ours, the Compose plugin missing —
// so nothing about containers can be known. Every verb reports it the same
// way, whichever call hit it.
type UnavailableError struct{ Detail string }

// Error is "Docker is not available: " followed by the detail.
func (e *UnavailableError) Error() string { return "Docker is not available: " + e.Detail }

// ExitStatus is 1.
func (e *UnavailableError) ExitStatus() int { return 1 }

// dockerItself matches a line Docker prints when the problem is Docker itself
// rather than the request. Anchored to the start of a line so that a build
// step's or a container's own output, which may embed the same phrases, never
// matches: those lines carry a `#N t.ttt` or progress prefix.
var dockerItself = regexp.MustCompile(`(?i)^(?:docker: |ERROR: |Error response from daemon: )?(?:` + strings.Join([]string{
	`failed to connect to the docker API`,
	`Cannot connect to the Docker daemon`,
	`error during connect`,
	// Docker 28 reworded this from "the Docker daemon socket" to "the docker API"; Engine 20 said "Got".
	`(?:Got )?permission denied while trying to connect to the docker (?:API|daemon socket)`,
	// Docker Desktop while its engine is still starting.
	`Bad response from Docker engine`,
	`request returned (?:500 )?Internal Server Error for API route`,
	`dial unix \S*docker\S*\.sock: connect: connection refused`,
	`'[^']+' is not a docker command`,
	`unknown command: docker\b`,
}, "|") + `)`)

// trimmedLines splits text into lines, trims each, and drops the ones left
// empty, so blank-line noise never masks or mimics a match.
func trimmedLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

// Complaint is the line, if any, on which Docker says it is the problem — the
// same line that gets reported.
func Complaint(stderr string) (string, bool) {
	for _, line := range trimmedLines(stderr) {
		if dockerItself.MatchString(line) {
			return line, true
		}
	}
	return "", false
}

// Run runs `docker`. A `docker` that cannot start, or one that answers that
// it cannot reach its daemon or lacks a plugin, is Docker being unavailable —
// reported as such whatever the verb and whatever the caller allowed, since
// no exit code of the request describes it. Only then is a rejecting caller's
// failure returned, exactly as the runner itself would. Every docker child
// inherits the locks held in ctx.
func Run(ctx context.Context, runner run.Runner, args []string, spec run.Spec) (run.Result, error) {
	spec.Command = "docker"
	spec.Args = args
	spec.InheritLocks = true
	allowed := spec.AllowFailure
	// Never let the runner reject first: Docker's own complaint must be read before the request's verdict.
	spec.AllowFailure = true
	result, err := runner.Run(ctx, spec)
	if err != nil {
		var start *run.StartError
		if errors.As(err, &start) {
			// 127 is the runner's verdict for "not found"; every other start failure keeps its own words.
			if start.ExitCode == 127 {
				return result, &UnavailableError{Detail: "the `docker` command is not on PATH (ENOENT); is Docker installed?"}
			}
			return result, &UnavailableError{Detail: "the `docker` command " + start.Detail}
		}
		return result, err
	}
	if result.ExitCode != 0 {
		if complaint, ok := Complaint(result.Stderr); ok {
			return result, &UnavailableError{Detail: complaint}
		}
	}
	spec.AllowFailure = allowed
	return run.Finish(spec, result)
}

// noise is what Docker prints around the reason, never as it: warnings
// before, help hints after.
var noise = regexp.MustCompile(`(?i)^(WARN(ING)?\b|time=.*\blevel=(warn|warning|info|debug)\b|(see|run) 'docker\b[^']*--help')`)

// Said is the line of Docker's stderr that says what went wrong, for callers
// that report a failed request themselves: the first that is neither a
// warning nor a hint.
func Said(stderr, fallback string) string {
	lines := trimmedLines(stderr)
	for _, line := range lines {
		if !noise.MatchString(line) {
			return line
		}
	}
	if len(lines) > 0 {
		return lines[len(lines)-1]
	}
	return fallback
}

// composeFileVariables would let the caller's environment change what Compose
// loads or how it reconciles: which files, which .env, whether orphans are
// kept. DOCKER_HOST and friends are deliberately not here — the plain docker
// calls inherit them too, so stripping them would split `up` and `remove`
// across daemons.
//
// Audited against Compose's full environment-variable list (#54; Compose
// reference, v5.5, the version this build tests against). Every other
// COMPOSE_* variable is either already forced by a flag or overlay Compose
// gives precedence to regardless of the inherited environment
// (COMPOSE_PROJECT_NAME by `--project-name`, always passed; COMPOSE_PROGRESS
// by the "plain" overlay below; COMPOSE_MENU is moot since `up` always
// passes `--detach`; COMPOSE_IGNORE_ORPHANS's opposite,
// COMPOSE_REMOVE_ORPHANS, by `--remove-orphans`, always passed on `up` and
// `down`) or carries no risk to a loaded file, the project name, the orphan
// sweep or ownership labels (COMPOSE_PARALLEL_LIMIT, COMPOSE_ANSI,
// COMPOSE_STATUS_STDOUT, COMPOSE_EXPERIMENTAL, COMPOSE_CONVERT_WINDOWS_PATHS,
// COMPOSE_BAKE — Inventory already finds and verifies built images by
// ownership labels alone, never by Compose's own project label, because
// Bake's builder does not stamp it either).
//
// COMPOSE_PROFILES is the one variable that changes which services exist at
// all, by filtering `docker compose config`'s own output to the active
// profiles. That does not put a profile-activated service's ownership
// labels at risk: overrideDocument stamps every service composeConfigJSON
// reports, whatever profile filtering produced that list, and prepare's own
// config read and the `up` it feeds both run from the same environment, one
// right after the other. The one way `up` could start a service the
// override never labelled is naming it on the command line: Compose then
// enables that service's profile even when the environment does not. The
// backend refuses any named service absent from the model it labels
// (refuseUnlabelledTargets), so the services the override labels are always
// the services `up` starts. See compose_up_test.go's profile cases.
var composeFileVariables = map[string]bool{
	"COMPOSE_FILE": true, "COMPOSE_PATH_SEPARATOR": true, "COMPOSE_ENV_FILES": true, "COMPOSE_IGNORE_ORPHANS": true,
	"COMPOSE_DISABLE_ENV_FILE": true,
}

// ComposeEnvironment is the environment every Compose invocation gets — the
// worktree runtime's and the router's alike: the inherited environment minus
// the redirecting variables, plain progress output, then extra, which wins.
func ComposeEnvironment(inherited []string, extra ...string) []string {
	var environment []string
	for _, entry := range inherited {
		name, _, _ := strings.Cut(entry, "=")
		if !composeFileVariables[name] {
			environment = append(environment, entry)
		}
	}
	return Merge(environment, append([]string{"COMPOSE_PROGRESS=plain"}, extra...)...)
}

// Merge overlays entries onto an environment, later names replacing earlier
// ones, so the child sees exactly one value per name.
func Merge(base []string, overlay ...string) []string {
	index := map[string]int{}
	var merged []string
	for _, entry := range append(append([]string(nil), base...), overlay...) {
		name, _, _ := strings.Cut(entry, "=")
		if i, ok := index[name]; ok {
			merged[i] = entry
			continue
		}
		index[name] = len(merged)
		merged = append(merged, entry)
	}
	return merged
}

// Unavailable reports whether err is Docker being unavailable.
func Unavailable(err error) bool {
	var unavailable *UnavailableError
	return errors.As(err, &unavailable)
}

var _ clierr.Exiter = (*UnavailableError)(nil)
