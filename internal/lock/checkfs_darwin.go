//go:build darwin

package lock

import (
	"strings"

	"golang.org/x/sys/unix"
)

// refusedTypeName is every statfs f_fstypename CheckFS refuses, by the names
// macOS's own mount(8) and Finder use for these filesystems. CI runs on APFS,
// so these refusals are checked only by name. The cluster filesystems the
// Linux list refuses have no native macOS clients; the ones reachable at all
// (CephFS, say) come through FUSE, which is refused here. OpenAFS is the
// exception, and is listed.
var refusedTypeName = map[string]string{
	"nfs":     "NFS",
	"smbfs":   "SMB",
	"afpfs":   "AFP",
	"webdav":  "WebDAV",
	"fusefs":  "FUSE",
	"macfuse": "FUSE",
	"osxfuse": "FUSE",
	"9p":      "9p",
	"afs":     "AFS",
}

// unsupportedFSType reports dir's filesystem type and whether CheckFS
// refuses it, by looking up its statfs f_fstypename in refusedTypeName. A
// statfs failure is reported as supported, leaving any real error to surface
// from the self-test or the caller's own use of dir.
func unsupportedFSType(dir string) (string, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return "", false
	}
	raw := st.Fstypename[:]
	if i := indexByte0(raw); i >= 0 {
		raw = raw[:i]
	}
	name := strings.ToLower(string(raw))
	if label, refused := refusedTypeName[name]; refused {
		return label, true
	}
	return "", false
}

// indexByte0 finds the first NUL in b, the way Fstypename — a fixed-size
// byte array, not a Go string — terminates its name.
func indexByte0(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}
