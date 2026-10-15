package releasefetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/sofired/berth/internal/text"
)

// Limits bound extraction.
type Limits struct {
	// Expanded is the most the gzip stream may expand to, however many
	// entries it holds.
	Expanded int64
	// Binary is the most the berth entry may be.
	Binary int64
}

// DefaultLimits are docs/design/teardown.md's: 500 MiB through the gzip
// stream, 200 MiB for the binary.
var DefaultLimits = Limits{Expanded: 500 << 20, Binary: 200 << 20}

// ArchiveError is an archive that is refused. Reason completes "the archive …".
type ArchiveError struct{ Reason string }

// Error is the reason.
func (e *ArchiveError) Error() string { return e.Reason }

// refuseArchive is an *ArchiveError whose reason is the formatted text.
func refuseArchive(format string, args ...any) error {
	return &ArchiveError{Reason: fmt.Sprintf(format, args...)}
}

// boundedReader fails, rather than ends, once more than limit bytes have been
// read through it, so a tar reader cannot mistake a cut stream for a complete
// one.
type boundedReader struct {
	r    io.Reader
	left int64
}

// errExpanded is boundedReader's failure.
var errExpanded = errors.New("expanded past its bound")

// Read implements io.Reader.
func (b *boundedReader) Read(p []byte) (int, error) {
	if b.left < 0 {
		return 0, errExpanded
	}
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	if b.left < 0 {
		return n, errExpanded
	}
	return n, err
}

// ExtractBerth returns the bytes of the one entry of a release archive named
// `berth` or `./berth`. The archive is read through limits.Expanded. It
// accepts exactly one regular entry of that name, of at most limits.Binary.
// Other regular entries (the licence, completions/berth) and directories are
// skipped and never written anywhere. A link of any kind, an entry of any
// other type, an absolute name, a name with a `..` element, a second `berth`
// or none at all refuses the archive.
func ExtractBerth(archive []byte, limits Limits) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, refuseArchive("is not a gzip file (%s)", text.OneLine(err.Error(), 200))
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(&boundedReader{r: gz, left: limits.Expanded})
	var binary []byte
	found := false
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, errExpanded) {
				return nil, refuseArchive("expands to more than %s", sizeText(limits.Expanded))
			}
			return nil, refuseArchive("is not a readable tar file (%s)", text.OneLine(err.Error(), 200))
		}
		name := header.Name
		if strings.HasPrefix(name, "/") {
			return nil, refuseArchive("has an entry with an absolute name")
		}
		for _, element := range strings.Split(name, "/") {
			if element == ".." {
				return nil, refuseArchive("has an entry with a `..` in its name")
			}
		}
		switch header.Typeflag {
		case tar.TypeSymlink, tar.TypeLink:
			return nil, refuseArchive("has a link entry")
		case tar.TypeDir, tar.TypeXGlobalHeader:
			continue
		case tar.TypeReg:
		default:
			return nil, refuseArchive("has an entry that is not a regular file or a directory")
		}
		if name != "berth" && name != "./berth" {
			continue
		}
		if found {
			return nil, refuseArchive("has more than one `berth` entry")
		}
		found = true
		if header.Size > limits.Binary {
			return nil, refuseArchive("holds a `berth` larger than %s", sizeText(limits.Binary))
		}
		binary, err = io.ReadAll(io.LimitReader(tr, limits.Binary+1))
		if err != nil {
			if errors.Is(err, errExpanded) {
				return nil, refuseArchive("expands to more than %s", sizeText(limits.Expanded))
			}
			return nil, refuseArchive("has a `berth` entry that cannot be read (%s)", text.OneLine(err.Error(), 200))
		}
		if int64(len(binary)) > limits.Binary {
			return nil, refuseArchive("holds a `berth` larger than %s", sizeText(limits.Binary))
		}
	}
	if !found {
		return nil, refuseArchive("has no `berth` entry at its root")
	}
	return binary, nil
}

// sizeText is n in MiB when it is a whole number of them, else in bytes.
func sizeText(n int64) string {
	if n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}
