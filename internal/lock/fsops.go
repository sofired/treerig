package lock

import (
	"os"
	"path/filepath"
	"syscall"

	"github.com/sofired/treerig/internal/clierr"
)

// dirOf is the directory a lock target file lives in.
func dirOf(target string) string { return filepath.Dir(target) }

// openTarget creates the target file 0600 (parent 0700) if it is missing,
// opens it O_RDWR, and reports its identity. A lock target is a managed
// file of ours, so a directory or symbolic link there is refused rather
// than followed or replaced — inspecting what another waiter created in
// between is why this loops instead of trusting a single lstat.
func openTarget(path, description string) (*os.File, FileID, error) {
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return nil, FileID{}, err
	}
	for {
		info, err := os.Lstat(path)
		if err == nil {
			if !info.Mode().IsRegular() {
				return nil, FileID{}, clierr.Newf(
					"Refusing to use %s for the %s lock: it is not a plain file "+
						"(a %s stands in its place). Inspect it before removing anything.", path, description, fileKind(info.Mode()))
			}
			break
		}
		if !os.IsNotExist(err) {
			return nil, FileID{}, err
		}
		f, createErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			if os.IsExist(createErr) {
				continue // another waiter created it first; look at what it made
			}
			return nil, FileID{}, createErr
		}
		f.Close()
	}

	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return nil, FileID{}, err
	}
	id, err := fstatID(file)
	if err != nil {
		file.Close()
		return nil, FileID{}, err
	}
	return file, id, nil
}

// fstatID reads file's device and inode through fstat, the identity a held
// lock is later checked against to prove its target has not been replaced.
func fstatID(file *os.File) (FileID, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &st); err != nil {
		return FileID{}, err
	}
	return FileID{Dev: uint64(st.Dev), Ino: uint64(st.Ino)}, nil
}

// sameFile compares fstat(fd) with lstat(path): the same dev and ino, and a
// link count above zero, so treerig never trusts a lock it holds on a target
// that has since been unlinked and replaced with a different file.
func sameFile(file *os.File, path string) (bool, error) {
	fid, err := fstatID(file)
	if err != nil {
		return false, err
	}
	var lst syscall.Stat_t
	if err := syscall.Lstat(path, &lst); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if uint64(lst.Nlink) == 0 {
		return false, nil
	}
	return fid.Dev == uint64(lst.Dev) && fid.Ino == uint64(lst.Ino), nil
}

// fileKind names what stands where a lock file should be.
func fileKind(mode os.FileMode) string {
	switch {
	case mode&os.ModeSymlink != 0:
		return "symbolic link"
	case mode.IsDir():
		return "directory"
	case mode&os.ModeNamedPipe != 0:
		return "named pipe"
	case mode&os.ModeSocket != 0:
		return "socket"
	case mode&os.ModeDevice != 0:
		return "device"
	default:
		return "special file"
	}
}
