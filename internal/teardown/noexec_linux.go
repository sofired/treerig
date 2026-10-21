package teardown

import "golang.org/x/sys/unix"

// noExecFlag reports whether statfs's mount flags include ST_NOEXEC.
func noExecFlag(flags int64) bool { return flags&unix.ST_NOEXEC != 0 }

// mountedNoExec reports whether the filesystem holding path, which must
// exist, is mounted noexec. It runs nothing.
func mountedNoExec(path string) (bool, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return false, err
	}
	return noExecFlag(stat.Flags), nil
}
