package commands

import (
	"bufio"
	"io"
	"strings"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/text"
)

// Destructive operations ask the user to type an exact phrase, so a stray `y`
// cannot approve them. Both halves of the question are filtered: the
// hostname in them comes from a registry record, and this is the one screen a
// reader checks before something is destroyed. Per line, because the plan is
// an itemised list — a caller that interpolates a borrowed value into a line
// filters it there, or the value could add lines of its own.

// confirmationPhrase is what the reader is shown, and therefore what they
// have to type. A phrase with something unprintable in it is displayed
// filtered, so the filtered form is the one that can be matched — asking for
// the raw one would leave no way through but --yes, which skips the plan too.
func confirmationPhrase(expected string) string { return text.Printable(expected) }

// confirmationMatches reports whether what was typed is what was asked for.
func confirmationMatches(expected, answer string) bool {
	return answer == confirmationPhrase(expected)
}

// styler colours the way Node's util.styleText does for a given stream: only
// when the stream can show colour.
type styler struct{ enabled bool }

// style wraps value in codes' escape pairs when colour is enabled, and
// returns value unchanged otherwise.
func (s styler) style(value string, codes ...[2]string) string {
	if !s.enabled {
		return value
	}
	left, right := "", ""
	for _, code := range codes {
		left += code[0]
		right = code[1] + right
	}
	return left + value + right
}

var (
	bold = [2]string{"\x1b[1m", "\x1b[22m"}
	cyan = [2]string{"\x1b[36m", "\x1b[39m"}
)

// colourFor decides as Node does for styleText's stream option: FORCE_COLOR
// forces it, NO_COLOR, NODE_DISABLE_COLORS and TERM=dumb refuse it, and
// otherwise only a terminal gets it.
func colourFor(env *Env) styler {
	lookup := func(name string) (string, bool) {
		if env.Getenv == nil {
			return "", false
		}
		return env.Getenv(name)
	}
	if force, ok := lookup("FORCE_COLOR"); ok {
		switch force {
		case "", "1", "2", "3", "true":
			return styler{enabled: true}
		case "0", "false":
			return styler{enabled: false}
		}
	}
	if _, ok := lookup("NO_COLOR"); ok {
		return styler{enabled: false}
	}
	if _, ok := lookup("NODE_DISABLE_COLORS"); ok {
		return styler{enabled: false}
	}
	if term, _ := lookup("TERM"); term == "dumb" {
		return styler{enabled: false}
	}
	return styler{enabled: env.StderrIsTTY}
}

// confirmationQuestion is the whole screen: the plan, then the phrase to type.
func confirmationQuestion(env *Env, prompt, expected string) string {
	colour := colourFor(env)
	phrase := colour.style(confirmationPhrase(expected), bold, cyan)
	input := colour.style("> ", bold)
	return text.PrintableLines(prompt) + "\n\nType this exact phrase to continue:\n\n  " + phrase + "\n\n" + input
}

// confirm requires the user to type the exact phrase; approved is the escape
// hatch for an explicit --yes.
func confirm(env *Env, prompt, expected string, approved bool) error {
	if approved {
		return nil
	}
	if !env.StdinIsTTY || env.Stdin == nil {
		return clierr.Newf("Interactive confirmation required: \"%s\".", confirmationPhrase(expected))
	}
	if env.Stderr != nil {
		_, _ = io.WriteString(env.Stderr, confirmationQuestion(env, prompt, expected))
	}
	answer, err := bufio.NewReader(env.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return err
	}
	answer = strings.TrimRight(answer, "\r\n")
	if !confirmationMatches(expected, answer) {
		return clierr.New("Confirmation did not match; operation cancelled.")
	}
	return nil
}
