package files

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteNew creates destination with contents and mode, and refuses, with an
// error that satisfies errors.Is(err, fs.ErrExist), if anything already
// occupies that path: a file, a directory, or a symlink, dangling or not. The
// refusal is the kernel's O_EXCL, so of any number of callers racing for one
// path exactly one wins, and nothing is ever overwritten. destination's
// directory must already exist. The mode is exact: it is set with chmod after
// the create, so the process's umask does not trim it.
//
// The file is written in place, not renamed into position, so there is a
// window in which it exists but is incomplete. A reader that lands in it, or a
// kill or power loss during it, sees a truncated file. A failed write removes
// the file it created. A kill cannot, so it leaves the truncated file behind,
// and a later WriteNew then refuses with fs.ErrExist: delete the file by hand.
// The window is as long as one small write and a fsync.
//
// Failing to sync the directory afterwards is not reported: the file exists
// and is complete, and the sync only narrows what a crash could lose.
func WriteNew(destination string, contents []byte, mode fs.FileMode) error {
	return createNew(destination, mode, func(handle *os.File) error {
		_, err := handle.Write(contents)
		return err
	})
}

// createNew is WriteNew with the filling of the new file as a parameter: it
// creates destination exclusively, runs fill on it, sets the exact mode,
// syncs and closes, and removes the file again if any of that fails, since the
// exclusive create means this call made it and owns it.
func createNew(destination string, mode fs.FileMode, fill func(*os.File) error) error {
	handle, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	err = fill(handle)
	if err == nil {
		err = handle.Chmod(mode)
	}
	if err == nil {
		err = handle.Sync()
	}
	if closeErr := handle.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(destination)
		return fmt.Errorf("writing %s: %w", destination, err)
	}
	if dir, openErr := os.Open(filepath.Dir(destination)); openErr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
