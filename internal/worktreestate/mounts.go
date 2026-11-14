package worktreestate

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sofired/treerig/internal/text"
)

// Mounts is the machine's mount table: the mount point of every filesystem
// that is mounted now, with its type. It is read once per run (/proc/self/mountinfo
// on Linux, getfsstat(2), which is what getmntinfo wraps, on macOS), so a
// directory's absence is judged against one consistent picture.
type Mounts map[string]string

// automount is the filesystem type of an automounter's placeholder. The mount
// table lists it, but nothing is mounted beneath it until something is
// opened there, so a directory below it that looks empty or absent says
// nothing about the files.
const automount = "autofs"

// Automounted reports whether path is, or is below, an automount placeholder.
func (m Mounts) Automounted(path string) bool {
	for dir := filepath.Clean(path); ; dir = filepath.Dir(dir) {
		if m[dir] == automount {
			return true
		}
		if dir == filepath.Dir(dir) {
			return false
		}
	}
}

// parseMountInfo reads /proc/self/mountinfo: one mount per line, `36 35 98:0
// /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw`, the mount point being
// the fifth field (with space, tab, newline and backslash escaped in octal)
// and the filesystem type the first field after the lone "-". It fails closed:
// a non-empty line this build cannot read may be a mount it needs, so the
// table is incomplete and nothing may be concluded from it.
func parseMountInfo(table string) (Mounts, error) {
	mounts := Mounts{}
	for _, line := range strings.Split(table, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if separator < 6 || separator+1 >= len(fields) {
			return nil, fmt.Errorf("the mount table has a line treerig can't read: %q", text.OneLine(line, 80))
		}
		mounts[filepath.Clean(unescapeMountField(fields[4]))] = fields[separator+1]
	}
	return mounts, nil
}

// unescapeMountField undoes the octal escapes (`\040` for a space) the kernel
// writes into a mountinfo path.
func unescapeMountField(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var out strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+3 < len(field) {
			if value, err := strconv.ParseUint(field[i+1:i+4], 8, 8); err == nil {
				out.WriteByte(byte(value))
				i += 3
				continue
			}
		}
		out.WriteByte(field[i])
	}
	return out.String()
}
