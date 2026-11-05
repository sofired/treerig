package text

import (
	"regexp"
	"slices"
	"strings"
)

// Redacted is what Redact leaves where it removed a value.
const Redacted = "<redacted>"

// urlUserinfo matches the scheme and userinfo of a URL such as
// https://user:token@host/path. The userinfo runs to the last "@" before the
// first "/", "?" or "#", the way a URL parser reads it, so a password with an
// unescaped "@" in it goes whole.
var urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^\s/?#]*@`)

// assignment matches a KEY=value pair: a name of letters, digits and
// underscores that does not continue a longer word or dotted name, an equals
// sign, and a value that is quoted or runs to the next space.
var assignment = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])([A-Za-z_][A-Za-z0-9_]*)=("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|\S+)`)

// authorization matches an HTTP credential: the value of an Authorization (or
// Proxy-Authorization) header to the end of the line, and a bearer token
// wherever it appears.
var authorization = regexp.MustCompile(`(?i)((?:proxy-)?authorization\s*[:=]\s*)[^\r\n]+|(\bbearer\s+)[A-Za-z0-9._~+/=-]+`)

// secretWords are the parts of a name that mark its value as a secret
// whatever its case.
var secretWords = []string{"pass", "pwd", "secret", "token", "key", "auth", "cred", "signature", "bearer", "private", "jwt"}

// Redact removes what a line must not carry into a terminal, a log or a bug
// report: the userinfo of every URL, and the value of every KEY=value pair
// whose name looks like an environment variable (upper case, digits and
// underscores) or like a secret (it contains pass, secret, token, key, auth
// or cred in any case). The names stay, so the line still says what was set.
// Other pairs, such as the label=com.docker.compose.project=x of a Docker
// filter, are left alone. It is the one function the verbose output runs its
// own lines through, and it is not a promise about text it never sees: the
// transcript of a child process passes as that child wrote it.
func Redact(value string) string {
	value = urlUserinfo.ReplaceAllString(value, "${1}"+Redacted+"@")
	value = authorization.ReplaceAllStringFunc(value, func(match string) string {
		parts := authorization.FindStringSubmatch(match)
		if parts[1] != "" {
			return parts[1] + Redacted
		}
		return parts[2] + Redacted
	})
	return assignment.ReplaceAllStringFunc(value, func(match string) string {
		parts := assignment.FindStringSubmatch(match)
		prefix, name := parts[1], parts[2]
		if !sensitiveName(name) {
			return match
		}
		return prefix + name + "=" + Redacted
	})
}

// valueFlags are the flags whose next argument is a secret, or a KEY=VALUE
// pair, whatever its name: it is replaced whole (a secret) or its value is
// (a pair).
var (
	secretFlags = []string{"--password", "--passwd", "--token", "--secret", "--auth", "--api-key", "--apikey", "--credential"}
	pairFlags   = []string{"-e", "--env", "--build-arg", "--set-env"}
)

// RedactArguments is args with every secret removed, one argument at a time, so
// that a value with spaces or quotes in it stays whole: the next argument of a
// flag such as --password, the value of a KEY=VALUE that follows -e, --env or
// --build-arg, a --flag=value whose flag name looks like a secret, and then
// whatever Redact finds in each. The names stay. It returns a new slice.
func RedactArguments(args []string) []string {
	out := make([]string, len(args))
	hide := 0 // 1: replace the whole next argument; 2: replace the value of its KEY=VALUE
	for i, arg := range args {
		switch {
		case hide == 1:
			out[i] = Redacted
		case hide == 2:
			name, _, ok := strings.Cut(arg, "=")
			if ok {
				out[i] = name + "=" + Redacted
			} else {
				out[i] = arg
			}
		default:
			out[i] = redactFlagValue(arg)
		}
		hide = 0
		switch {
		case slices.Contains(secretFlags, arg):
			hide = 1
		case slices.Contains(pairFlags, arg):
			hide = 2
		}
	}
	return out
}

// redactFlagValue is arg with the value of a --flag=value removed when the
// flag's name looks like a secret or is one of the pair flags, and otherwise
// whatever Redact finds in it.
func redactFlagValue(arg string) string {
	if flag, value, ok := strings.Cut(arg, "="); ok && strings.HasPrefix(flag, "-") {
		if slices.Contains(secretFlags, flag) || secretLike(flag) {
			return flag + "=" + Redacted
		}
		if slices.Contains(pairFlags, flag) {
			name, _, pair := strings.Cut(value, "=")
			if pair {
				return flag + "=" + name + "=" + Redacted
			}
		}
	}
	return Redact(arg)
}

// secretLike reports whether a flag name contains a secret word.
func secretLike(flag string) bool {
	lower := strings.ToLower(flag)
	for _, word := range secretWords {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

// sensitiveName reports whether the value of name is redacted.
func sensitiveName(name string) bool {
	if name == strings.ToUpper(name) {
		return true
	}
	lower := strings.ToLower(name)
	for _, word := range secretWords {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}
