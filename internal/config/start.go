package config

import (
	"github.com/sofired/berth/internal/shape"
	"github.com/sofired/berth/internal/text"
)

// StartIssues is the one rule the base loader (Parse, Load, LoadNearest)
// deliberately does not enforce: no control character — an escape
// sequence, a bidi override, a zero-width character, a raw C1 byte — in any
// declared service's [service.<name>.index] label or purpose (#19).
//
// Every command that only reads an already-running worktree's config —
// `down`, `worktree remove`, `status`, `urls`, `index`, `worktree list`,
// `worktree unlock`, and so on — must load the same file a hand edit or a
// paste could have left with one, and go ahead regardless: text.Printable,
// which every text and HTML renderer that shows these fields already runs
// them through, neutralizes it wherever it is displayed. Only a command
// that is about to start something calls StartIssues and refuses: `up`
// itself, `config validate` answering "would up work?" on its behalf, and
// `doctor`'s "This checkout" section doing the same for a registered
// worktree it is not itself starting.
func StartIssues(cfg Config) []shape.Issue {
	var issues []shape.Issue
	for _, svc := range cfg.Service {
		if svc.Index == nil {
			continue
		}
		if text.HasControl(svc.Index.Label) {
			issues = append(issues, shape.Issue{
				Path: []string{"service", svc.Name, "index", "label"}, Code: shape.CodeCustom,
				Message: "must not contain control characters",
			})
		}
		if text.HasControl(svc.Index.Purpose) {
			issues = append(issues, shape.Issue{
				Path: []string{"service", svc.Name, "index", "purpose"}, Code: shape.CodeCustom,
				Message: "must not contain control characters",
			})
		}
	}
	return issues
}

// CheckStart runs StartIssues against an already-loaded config and, if it
// finds any, formats them exactly as a schema issue from Parse itself would
// be: "<file> is not a valid berth.toml:" plus one indented line per issue —
// the same message the old, schema-level Refine printed. A schema issue
// itself already stops Load before StartIssues ever gets a chance to run
// (Parse returns before decodeConfig produces a Config to check), so the
// two kinds of problem never compete for which "wins": a schema error keeps
// its exclusive claim on that message, unchanged from before this moved.
func CheckStart(loaded *Loaded) error {
	return issuesError(loaded.File, StartIssues(loaded.Config))
}
