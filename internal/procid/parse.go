package procid

import (
	"bytes"
	"encoding/binary"
	"regexp"
	"strconv"
	"strings"
)

// parseProcStat reads /proc/<pid>/stat: the command name is parenthesised and
// may itself hold spaces and parentheses, so fields are counted from the last
// ")". The state is field 3 and the start time field 22 (index 19 once the
// first two fields are dropped).
func parseProcStat(text string) (state string, startTicks uint64, ok bool) {
	close := strings.LastIndexByte(text, ')')
	if close < 0 {
		return "", 0, false
	}
	rest := text[close+2:]
	fields := strings.Split(rest, " ")
	if len(fields) == 0 {
		return "", 0, false
	}
	state = fields[0]
	if len(state) != 1 || !isLetter(state[0]) {
		return "", 0, false
	}
	if len(fields) <= 19 {
		return "", 0, false
	}
	n, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return state, n, true
}

// isLetter reports whether b is an ASCII letter, the only valid shape for the
// single-character process-state field in /proc/<pid>/stat.
func isLetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// stateOf is a process-state letter, as /proc and ps both spell it.
func stateOf(letter byte) State {
	switch letter {
	case 'Z', 'X', 'x':
		return Zombie
	case 'T', 't':
		return Stopped
	default:
		return Running
	}
}

var etimePattern = regexp.MustCompile(`^(?:(\d+)-)?(?:(\d+):)?(\d+):(\d+)$`)

// parseEtime reads ps's elapsed time, [[dd-]hh:]mm:ss, in seconds.
func parseEtime(text string) (int64, bool) {
	m := etimePattern.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return 0, false
	}
	days := atoiOr(m[1], 0)
	hours := atoiOr(m[2], 0)
	minutes := atoiOr(m[3], 0)
	seconds := atoiOr(m[4], 0)
	return ((days*24+hours)*60+minutes)*60 + seconds, true
}

// atoiOr parses s as a base-10 integer, or returns fallback if s is empty or
// not a valid number — ps's elapsed-time fields omit the leading components
// ([[dd-]hh:]mm:ss), so a missing one means zero, not an error.
func atoiOr(s string, fallback int64) int64 {
	if s == "" {
		return fallback
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

// parsePsLine reads one `ps -o pid=,stat=,etime=` line — for the PID asked
// about, or nothing to go on. Only the first three whitespace-separated
// fields matter; anything past etime is ignored, as the TypeScript build's
// destructuring split ignored it too.
func parsePsLine(line string, pid int) (stat string, elapsedSeconds int64, ok bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	shown := ""
	etime := ""
	if len(fields) > 0 {
		shown = fields[0]
	}
	if len(fields) > 1 {
		stat = fields[1]
	}
	if len(fields) > 2 {
		etime = fields[2]
	}
	elapsedSeconds, ok = parseEtime(etime)
	shownPid, err := strconv.Atoi(shown)
	if err != nil || shownPid != pid || stat == "" || !isLetter(stat[0]) || !ok {
		return "", 0, false
	}
	return stat, elapsedSeconds, true
}

var ioregUUIDPattern = regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([^"]+)"`)

// parseIoregUUID reads the platform UUID out of `ioreg -rd1 -c IOPlatformExpertDevice`.
func parseIoregUUID(text string) (string, bool) {
	m := ioregUUIDPattern.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return m[1], true
}

var psScanLinePattern = regexp.MustCompile(`^\s*(\d+)\s+(.*)$`)

// parsePsScan reads `ps -A -ww -E -o pid=,command=`: arguments and
// environment in one column, space-separated. They cannot be told apart, so
// each whole token is offered as both — a marker is a whole token either way.
func parsePsScan(text string, exclude map[int]bool) []Proc {
	var procs []Proc
	for _, line := range strings.Split(text, "\n") {
		m := psScanLinePattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		pid, err := strconv.Atoi(m[1])
		if err != nil || exclude[pid] {
			continue
		}
		tokens := strings.Fields(m[2])
		procs = append(procs, Proc{PID: pid, Args: tokens, Env: tokens, EnvRead: true})
	}
	return procs
}

// parseProcArgs2 reads the buffer kern.procargs2 returns for one PID: a
// little-endian argc, the saved exec path (NUL-terminated, then NUL-padded to
// the next word), argv, each NUL-terminated, and finally the environment,
// each entry NUL-terminated, running to the end of the buffer: the layout
// libtop documents and `ps` and `top` rely on. The macOS CI job reads real
// buffers through the lock layer's marker scan.
func parseProcArgs2(raw []byte) (argv, envp []string, ok bool) {
	if len(raw) < 4 {
		return nil, nil, false
	}
	argc := int32(binary.LittleEndian.Uint32(raw[:4]))
	if argc < 0 {
		return nil, nil, false
	}
	rest := raw[4:]
	execEnd := bytes.IndexByte(rest, 0)
	if execEnd < 0 {
		return nil, nil, false
	}
	rest = rest[execEnd:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	// argc is the kernel's word, but the buffer is what bounds the allocation:
	// each argument takes at least its NUL, and a truncated buffer holds fewer.
	argv = make([]string, 0, min(int(argc), len(rest)))
	for i := int32(0); i < argc && len(rest) > 0; i++ {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			argv = append(argv, string(rest))
			rest = nil
			break
		}
		argv = append(argv, string(rest[:end]))
		rest = rest[end+1:]
	}
	for len(rest) > 0 {
		if rest[0] == 0 {
			rest = rest[1:]
			continue
		}
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			envp = append(envp, string(rest))
			break
		}
		envp = append(envp, string(rest[:end]))
		rest = rest[end+1:]
	}
	return argv, envp, true
}
