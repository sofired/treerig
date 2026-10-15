package releasefetch_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/releasefetch"
	"github.com/sofired/berth/internal/releasefetch/releasetest"
)

func TestExtractBerth(t *testing.T) {
	binary := []byte("the berth binary")
	good := releasetest.Entry{Name: "berth", Mode: 0o755, Body: binary}
	limits := releasefetch.DefaultLimits

	t.Run("returns the berth entry and skips the rest, completions/berth included", func(t *testing.T) {
		archive := releasetest.Archive(
			releasetest.Entry{Name: "LICENSE", Body: []byte("license")},
			releasetest.Entry{Name: "completions/", Type: tar.TypeDir},
			releasetest.Entry{Name: "completions/berth", Body: []byte("not the binary")},
			good,
		)
		got, err := releasefetch.ExtractBerth(archive, limits)
		if err != nil || !bytes.Equal(got, binary) {
			t.Fatalf("ExtractBerth = %q, %v", got, err)
		}
	})

	t.Run("skips a pax global header, which git archive and some tars write first", func(t *testing.T) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "abc"}}); err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: "berth", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(binary))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(binary)
		_ = tw.Close()
		_ = gz.Close()
		got, err := releasefetch.ExtractBerth(buf.Bytes(), limits)
		if err != nil || !bytes.Equal(got, binary) {
			t.Fatalf("ExtractBerth = %q, %v", got, err)
		}
	})

	t.Run("accepts ./berth", func(t *testing.T) {
		got, err := releasefetch.ExtractBerth(releasetest.Archive(releasetest.Entry{Name: "./berth", Body: binary}), limits)
		if err != nil || !bytes.Equal(got, binary) {
			t.Fatalf("ExtractBerth = %q, %v", got, err)
		}
	})

	t.Run("accepts an empty berth for the caller's executable check to refuse", func(t *testing.T) {
		got, err := releasefetch.ExtractBerth(releasetest.Archive(releasetest.Entry{Name: "berth"}), limits)
		if err != nil || len(got) != 0 {
			t.Fatalf("ExtractBerth = %q, %v", got, err)
		}
	})

	for name, tc := range map[string]struct {
		entries []releasetest.Entry
		reason  string
	}{
		"a missing berth":                      {[]releasetest.Entry{{Name: "LICENSE", Body: []byte("x")}}, "has no `berth` entry at its root"},
		"berth only below the root":            {[]releasetest.Entry{{Name: "dist/berth", Body: binary}}, "has no `berth` entry at its root"},
		"berth only as completions/berth":      {[]releasetest.Entry{{Name: "completions/berth", Body: binary}}, "has no `berth` entry at its root"},
		"berth that is a directory":            {[]releasetest.Entry{{Name: "berth/", Type: tar.TypeDir}}, "has no `berth` entry at its root"},
		"a duplicate berth":                    {[]releasetest.Entry{good, good}, "has more than one `berth` entry"},
		"berth and ./berth":                    {[]releasetest.Entry{good, {Name: "./berth", Body: binary}}, "has more than one `berth` entry"},
		"berth as a symlink":                   {[]releasetest.Entry{{Name: "berth", Type: tar.TypeSymlink, Linkname: "/bin/sh"}}, "has a link entry"},
		"a symlink beside a good berth":        {[]releasetest.Entry{good, {Name: "x", Type: tar.TypeSymlink, Linkname: "berth"}}, "has a link entry"},
		"a symlink before a good berth":        {[]releasetest.Entry{{Name: "x", Type: tar.TypeSymlink, Linkname: "/etc/passwd"}, good}, "has a link entry"},
		"a hard link":                          {[]releasetest.Entry{good, {Name: "x", Type: tar.TypeLink, Linkname: "berth"}}, "has a link entry"},
		"an absolute name":                     {[]releasetest.Entry{good, {Name: "/etc/passwd", Body: []byte("x")}}, "has an entry with an absolute name"},
		"an absolute berth":                    {[]releasetest.Entry{{Name: "/berth", Body: binary}}, "has an entry with an absolute name"},
		"a name that starts with ..":           {[]releasetest.Entry{good, {Name: "../evil", Body: []byte("x")}}, "has an entry with a `..` in its name"},
		"a name with .. in the middle":         {[]releasetest.Entry{good, {Name: "a/../../evil", Body: []byte("x")}}, "has an entry with a `..` in its name"},
		"a .. entry that is a directory":       {[]releasetest.Entry{good, {Name: "../", Type: tar.TypeDir}}, "has an entry with a `..` in its name"},
		"a device":                             {[]releasetest.Entry{good, {Name: "dev", Type: tar.TypeChar}}, "has an entry that is not a regular file or a directory"},
		"a fifo":                               {[]releasetest.Entry{good, {Name: "fifo", Type: tar.TypeFifo}}, "has an entry that is not a regular file or a directory"},
		"a hostile name after a long run":      {[]releasetest.Entry{good, {Name: "pad", Body: bytes.Repeat([]byte("p"), 1<<16)}, {Name: "x/../../y", Body: []byte("x")}}, "has an entry with a `..` in its name"},
		"a berth larger than the binary bound": {[]releasetest.Entry{{Name: "berth", Body: bytes.Repeat([]byte("b"), 101)}}, "holds a `berth` larger than 100 bytes"},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			bound := limits
			bound.Binary = 100
			got, err := releasefetch.ExtractBerth(releasetest.Archive(tc.entries...), bound)
			var refusal *releasefetch.ArchiveError
			if !errors.As(err, &refusal) || refusal.Reason != tc.reason || got != nil {
				t.Fatalf("ExtractBerth = %q, %v\nwant reason %q", got, err, tc.reason)
			}
		})
	}

	t.Run("accepts a berth exactly at the binary bound", func(t *testing.T) {
		bound := limits
		bound.Binary = 100
		got, err := releasefetch.ExtractBerth(releasetest.Archive(releasetest.Entry{Name: "berth", Body: bytes.Repeat([]byte("b"), 100)}), bound)
		if err != nil || len(got) != 100 {
			t.Fatalf("ExtractBerth = %d bytes, %v", len(got), err)
		}
	})

	t.Run("refuses a stream that expands past the bound, whichever entry does it", func(t *testing.T) {
		bound := releasefetch.Limits{Expanded: 64 << 10, Binary: 64 << 10}
		zeros := bytes.Repeat([]byte{0}, 1<<20) // compresses to about a kilobyte
		for name, entries := range map[string][]releasetest.Entry{
			"in a skipped entry":    {good, {Name: "padding", Body: zeros}},
			"before berth":          {{Name: "padding", Body: zeros}, good},
			"in berth itself":       {{Name: "berth", Body: zeros}},
			"in many small entries": manyEntries(1500),
		} {
			t.Run(name, func(t *testing.T) {
				archive := releasetest.Archive(entries...)
				if name != "in many small entries" && len(archive) > 8<<10 {
					t.Fatalf("the fixture is not a bomb: %d compressed bytes", len(archive))
				}
				_, err := releasefetch.ExtractBerth(archive, bound)
				var refusal *releasefetch.ArchiveError
				if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "expands to more than 65536 bytes") && !strings.Contains(refusal.Reason, "larger than") {
					t.Fatalf("err = %v", err)
				}
			})
		}
	})

	t.Run("refuses what is not a gzip file or not a tar file inside one", func(t *testing.T) {
		var gzOfText bytes.Buffer
		w := gzip.NewWriter(&gzOfText)
		_, _ = w.Write(bytes.Repeat([]byte("this is not a tar file. "), 100))
		_ = w.Close()
		truncated := releasetest.Archive(good)
		truncated = truncated[:len(truncated)/2]
		for name, data := range map[string][]byte{"plain text": []byte("hello"), "empty": nil, "gzip of text": gzOfText.Bytes(), "truncated": truncated} {
			t.Run(name, func(t *testing.T) {
				if _, err := releasefetch.ExtractBerth(data, limits); err == nil {
					t.Fatal("accepted")
				}
			})
		}
	})
}

// manyEntries is n small regular files, whose total size is past a small bound
// without any one entry being large.
func manyEntries(n int) []releasetest.Entry {
	entries := make([]releasetest.Entry, 0, n+1)
	for i := 0; i < n; i++ {
		entries = append(entries, releasetest.Entry{Name: "f" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + strings.Repeat("y", i%5), Body: []byte("x")})
	}
	return entries
}
