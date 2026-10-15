package teardown

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sofired/treerig/internal/binstore"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/text"
)

// FilePin is the pin `teardown pin --file` writes: a file pin of the absolute,
// cleaned path, labelled release, whose one platform entry names binarySHA256.
// The caller has checked the arguments; Validate (and so Save) is the check on
// the result.
func FilePin(path, release, platform, binarySHA256 string, now time.Time) Pin {
	return Pin{
		Release:   release,
		Source:    fileScheme + path,
		PinnedAt:  FormatTime(now),
		Platforms: map[string]Platform{platform: {BinarySHA256: binarySHA256}},
	}
}

// ReleasePin is the pin `teardown pin --release` and `--latest` write: a
// release pin of the https source and the signer cosign verified, whose
// platforms hold archiveSHA256 (the digests in the verified checksums.txt, one
// per platform) and, for platform alone, the extracted binary's digest. The
// caller has checked the arguments; Validate (and so Save) is the check on the
// result.
func ReleasePin(release, source, signer, checksumsSHA256 string, archiveSHA256 map[string]string, platform, binarySHA256 string, now time.Time) Pin {
	platforms := make(map[string]Platform, len(archiveSHA256))
	for key, digest := range archiveSHA256 {
		platforms[key] = Platform{ArchiveSHA256: digest}
	}
	entry := platforms[platform]
	entry.BinarySHA256 = binarySHA256
	platforms[platform] = entry
	return Pin{
		Release:         release,
		Source:          source,
		Signer:          signer,
		ChecksumsSHA256: checksumsSHA256,
		PinnedAt:        FormatTime(now),
		Platforms:       platforms,
	}
}

// ValidSHA256 reports whether s is 64 lowercase hexadecimal characters, the
// shape of every digest in a pin.
func ValidSHA256(s string) bool { return sha256Pattern.MatchString(s) }

// BinaryState says how the stored binary for a platform compares with a pin.
type BinaryState string

const (
	// BinaryOK: the stored file is a regular file with the pinned digest.
	BinaryOK BinaryState = "ok"
	// BinaryMissing: the pin names a binary and no file is stored for it.
	BinaryMissing BinaryState = "missing"
	// BinaryMismatch: the stored file is not the pinned binary: other bytes,
	// or something that cannot be read or is not a regular file (a symlink
	// included). All of these are refused alike.
	BinaryMismatch BinaryState = "mismatch"
	// BinaryNone: the pin names no binary for the platform.
	BinaryNone BinaryState = "none"
)

// BinaryCheck is what InspectBinary found.
type BinaryCheck struct {
	State BinaryState
	// Path is the stored file's path; empty for BinaryNone.
	Path string
	// Pinned is the digest the pin records for the platform; empty for
	// BinaryNone.
	Pinned string
	// Found is the digest of the file on disk, when it could be read.
	Found string
	// Problem finishes "The pinned teardown binary <path> ..." for any state
	// but BinaryOK and BinaryNone.
	Problem string
}

// InspectBinary compares the stored binary for platform with the digest pin
// records. It does not follow a symlink at the stored path, and it hashes the
// bytes of the descriptor it opened, so what it reports is what that
// descriptor holds. It never runs the binary and never modifies anything.
//
// It is not by itself enough for the removal gate: the gate must verify and
// execute the same bytes, through one descriptor or a copy it made, because
// the path can be swapped between this check and an exec of it.
func InspectBinary(l layout.Control, pin Pin, platform string) BinaryCheck {
	pinned := pin.Platforms[platform].BinarySHA256
	if pinned == "" {
		return BinaryCheck{State: BinaryNone}
	}
	result := BinaryCheck{Path: BinaryPath(l, pinned), Pinned: pinned}
	mismatch := func(problem string) BinaryCheck {
		result.State, result.Problem = BinaryMismatch, problem
		return result
	}
	unreadable := func(err error) BinaryCheck {
		return mismatch(fmt.Sprintf("cannot be read (%s)", text.OneLine(reasonOf(err), 200)))
	}
	if info, err := os.Lstat(result.Path); errors.Is(err, fs.ErrNotExist) {
		result.State, result.Problem = BinaryMissing, "is missing"
		return result
	} else if err != nil {
		return unreadable(err)
	} else if !info.Mode().IsRegular() {
		return mismatch("is not a regular file")
	}
	// O_NOFOLLOW is defence against a swap between the Lstat and the open;
	// O_NONBLOCK keeps a FIFO swapped in there from hanging the open, and the
	// Stat on the descriptor then refuses it.
	file, err := os.OpenFile(result.Path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			result.State, result.Problem = BinaryMissing, "is missing"
			return result
		}
		if errors.Is(err, syscall.ELOOP) {
			return mismatch("is not a regular file")
		}
		return unreadable(err)
	}
	defer func() { _ = file.Close() }()
	if info, err := file.Stat(); err != nil {
		return unreadable(err)
	} else if !info.Mode().IsRegular() {
		return mismatch("is not a regular file")
	}
	found, err := binstore.HashReader(file)
	if err != nil {
		return unreadable(err)
	}
	result.Found = found
	if found != pinned {
		return mismatch(fmt.Sprintf("has sha256 %s, not the pinned %s", found, pinned))
	}
	result.State = BinaryOK
	return result
}

// reasonOf is err's system error without the path an *fs.PathError names, for
// a message that names the path once.
func reasonOf(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return err.Error()
}
