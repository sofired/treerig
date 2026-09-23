//go:build darwin

package lock

import (
	"strings"

	"golang.org/x/sys/unix"
)

// refusedTypeName is every statfs f_fstypename CheckFS refuses. Unverified —
// there is no macOS in this spike — so this follows the names macOS's own
// mount(8) and Finder use for these filesystems. The cluster filesystems the
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

func indexByte0(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}
