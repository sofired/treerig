// Package progress is how a long phase is shown while it runs. It is its own
// package, rather than living beside one of its callers, so that both
// internal/commands (a mutation's own phases) and internal/hooks (one hook
// run) can use it without either importing the other.
//
// Progress is the small interface they use; Plain is its one writer today, one
// line per change plus a heartbeat, which is what a pipe, CI and an agent get
// and what every in-process test gets, so goldens stay byte-exact. The live
// writer that redraws on a terminal comes with docs/design/cli-1.0.md,
// "Rendering" (#208).
package progress

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/text"
)

// heartbeat is how often a long phase says it is still going.
var heartbeat = 10 * time.Second

// SetHeartbeatForTest overrides how often Plain reports a still-going
// heartbeat, for a test that needs to observe one without waiting out the real
// interval. Returns a func that restores the previous value; callers defer it
// so an override never leaks into another test.
func SetHeartbeatForTest(d time.Duration) (restore func()) {
	previous := heartbeat
	heartbeat = d
	return func() { heartbeat = previous }
}

// PhaseSpec describes one long phase.
type PhaseSpec struct {
	// Verb is capitalised ("Starting") and What is the rest of the sentence
	// ("web for feature-x.myapp.localhost"); both are treerig's own words or
	// text already safe to print.
	Verb, What string
	// Passthrough says the action's own output owns the terminal while it
	// runs (a hook): a writer that redraws must not draw over it.
	Passthrough bool
	// Note, when set, answers what the action last reported (the last line
	// Compose wrote), for the heartbeat to carry: a stuck or killed `up` then
	// leaves its last words without -v. It may be called from another
	// goroutine; see Latest.
	Note func() string
}

// Progress shows one long phase to whoever runs treerig.
//
// The live writer of docs/design/cli-1.0.md, "Rendering" (#208), still needs
// what this interface does not carry: a source of per-service state to poll
// (`docker compose ps --all --format json` while `up` runs). It will arrive as
// another field of PhaseSpec, so that callers which do not set it keep working.
type Progress interface {
	// Phase announces "<Verb> <What>." and runs action, and while it runs
	// keeps saying it is not stuck. It returns action's error, and says
	// nothing once it has returned.
	Phase(spec PhaseSpec, action func() error) error
}

// Or is p, or the plain writer over log when p is nil: the one fallback for a
// hand-built context that never chose a writer.
func Or(p Progress, log logger.Logger) Progress {
	if p == nil {
		return NewPlain(log)
	}
	return p
}

// Latest holds the last line a child wrote, for PhaseSpec.Note. The zero value
// is ready, and it is safe for concurrent use.
type Latest struct {
	mu   sync.Mutex
	line string
}

// Set records line, unless it is blank.
func (l *Latest) Set(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.line = line
}

// Get is the last line recorded, or "".
func (l *Latest) Get() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.line
}

// noteLimit is how many characters of a note the heartbeat carries.
const noteLimit = 120

// Plain is the writer for output that is not a terminal: the announcement is
// one line, and while the phase runs a heartbeat line, "still starting web, 40s",
// follows every ten seconds, so a pipe, CI or an agent never sees a silent
// minute. Every line goes through the logger, so it is filtered like any other
// notice. Until the live writer exists (#208) it is also what a terminal gets.
type Plain struct {
	log logger.Logger
}

// NewPlain makes the plain writer over log.
func NewPlain(log logger.Logger) *Plain { return &Plain{log: log} }

// Phase logs the announcement, then a heartbeat every heartbeat while action
// runs, followed by ": <note>" when spec.Note has something to say. No
// heartbeat is ever logged once Phase has returned: the deferred cleanup waits
// for the heartbeat goroutine to have actually stopped, not just for it to have
// been told to, since a tick ready at the same moment as done being closed is
// not guaranteed to lose a select.
func (p *Plain) Phase(spec PhaseSpec, action func() error) error {
	p.log.Info(spec.Verb + " " + spec.What + ".")
	still := "still " + lowerFirst(spec.Verb) + " " + spec.What
	started := time.Now()
	done := make(chan struct{})
	stopped := make(chan struct{})
	ticker := time.NewTicker(heartbeat)
	go func() {
		defer close(stopped)
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				// select does not prefer done over a tick that became ready
				// at the same time; this second, non-blocking check is what
				// actually keeps a heartbeat from logging once action has
				// returned.
				select {
				case <-done:
					return
				default:
				}
				line := fmt.Sprintf("%s, %s", still, elapsed(started))
				if spec.Note != nil {
					if note := clip(spec.Note()); note != "" {
						line += ": " + note
					}
				}
				p.log.Info(line)
			}
		}
	}()
	defer func() {
		ticker.Stop()
		close(done)
		<-stopped
	}()
	return action()
}

// clip is a note made safe to print, with secrets in it redacted (Compose
// echoes URLs and variables), and cut to noteLimit characters.
func clip(note string) string {
	note = strings.TrimSpace(text.Redact(text.Printable(note)))
	runes := []rune(note)
	if len(runes) > noteLimit {
		return strings.TrimSpace(string(runes[:noteLimit-3])) + "..."
	}
	return note
}

// lowerFirst lower-cases the first letter of an ASCII verb.
func lowerFirst(verb string) string {
	if verb == "" {
		return verb
	}
	return strings.ToLower(verb[:1]) + verb[1:]
}

// elapsed is the time since started, formatted by formatElapsed.
func elapsed(started time.Time) string {
	return formatElapsed(time.Since(started))
}

// formatElapsed renders d rounded down to whole seconds (never below 1s) as
// "Ns" under a minute or "Mm SSs" at or above it.
func formatElapsed(d time.Duration) string {
	total := max(1, int(d/time.Second))
	if total < 60 {
		return fmt.Sprintf("%ds", total)
	}
	return fmt.Sprintf("%dm %02ds", total/60, total%60)
}
