package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/shape"
)

// FindConfigFile walks upward from startDir looking for treerig.toml, stopping
// at the filesystem root. It reports false rather than failing, so callers
// can give a command-specific hint.
func FindConfigFile(startDir string) (string, bool, error) {
	current, err := layout.Resolve(startDir)
	if err != nil {
		return "", false, err
	}
	for {
		candidate := filepath.Join(current, brand.ConfigFile)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", false, nil
		}
		current = parent
	}
}

// Parse validates contents, reporting problems against file.
func Parse(contents []byte, file string) (Config, error) {
	obj, err := DecodeOrderedTOML(string(contents))
	if err != nil {
		return Config{}, clierr.Newf("%s is not valid TOML: %s", file, err.Error())
	}

	value, issues := shape.Parse(rootSchema(filepath.Dir(file)), obj)
	if err := issuesError(file, issues); err != nil {
		return Config{}, err
	}

	return decodeConfig(value.(*shape.Obj)), nil
}

// issuesError formats a batch of issues the way a schema failure is
// reported: "<file> is not a valid <ConfigFile>:" followed by one indented
// "path: message" line per issue, in order. Parse's own schema issues and
// StartIssues (start.go) — a separate check callers run after a load that
// already succeeded — share this, so the two kinds of problem read
// identically. Reports nil for an empty batch.
func issuesError(file string, issues []shape.Issue) error {
	if len(issues) == 0 {
		return nil
	}
	lines := make([]string, len(issues))
	for i, issue := range issues {
		lines[i] = fmt.Sprintf("  %s: %s", shape.PathString(issue.Path), issue.Message)
	}
	return clierr.Newf("%s is not a valid %s:\n%s", file, brand.ConfigFile, strings.Join(lines, "\n"))
}

// MaxFileSize is the most a treerig.toml, or the Compose file `status` hands to
// Compose for another worktree, may hold. It is far more than any real file,
// so only something that is not a configuration file at all reaches it.
const MaxFileSize = 1 << 20

// ReadRegularFile reads path, which must be a regular file (after following
// symbolic links) of at most limit bytes: a device, a pipe or a directory is
// refused before anything is read from it, and a file that grows past limit
// while it is read is refused too. It opens without blocking, since opening a
// pipe that has no writer would otherwise wait for one forever; the type is
// then checked on the open descriptor, and only a regular file is read.
func ReadRegularFile(path string, limit int64) ([]byte, error) {
	file, err := openWithoutBlocking(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err := checkRegular(file, path, limit); err != nil {
		return nil, err
	}
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, tooLarge(path, limit)
	}
	return contents, nil
}

// CheckRegularFile is ReadRegularFile's check without the read: path must be a
// regular file of at most limit bytes.
func CheckRegularFile(path string, limit int64) error {
	file, err := openWithoutBlocking(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return checkRegular(file, path, limit)
}

// openWithoutBlocking opens path for reading with O_NONBLOCK, so that a pipe
// with no writer opens at once and checkRegular can refuse it.
func openWithoutBlocking(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// checkRegular refuses an open file that is not a regular file, or is larger
// than limit.
func checkRegular(file *os.File, path string, limit int64) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	switch {
	case !info.Mode().IsRegular():
		return clierr.Newf("%s is not a regular file.", path)
	case info.Size() > limit:
		return tooLarge(path, limit)
	}
	return nil
}

// tooLarge is the refusal of a file past the limit.
func tooLarge(path string, limit int64) error {
	return clierr.Newf("%s is larger than %d bytes, which is more than a configuration file holds.", path, limit)
}

// Load reads and validates one file.
func Load(file string) (*Loaded, error) {
	resolved, err := layout.Resolve(file)
	if err != nil {
		return nil, err
	}
	contents, err := ReadRegularFile(resolved, MaxFileSize)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			missing := clierr.Newf("No %s at %s.", brand.ConfigFile, resolved)
			missing.Kind = os.ErrNotExist
			return nil, missing
		}
		return nil, err
	}
	cfg, err := Parse(contents, resolved)
	if err != nil {
		return nil, err
	}
	return &Loaded{Config: cfg, File: resolved, Root: filepath.Dir(resolved)}, nil
}

// exampleFileURL is where the missing-config hint points for the annotated
// example: a released binary carries no source tree of its own, so the hint
// names something a binary-only install still has access to.
const exampleFileURL = "https://github.com/" + brand.Repository + "/blob/main/" + brand.ExampleFile

// LoadNearest loads the nearest treerig.toml above startDir, or fails with a
// hint that names something that exists. The hint names the tool by brand.Name,
// not by the name it was invoked as: this package is not handed one.
func LoadNearest(startDir string) (*Loaded, error) {
	file, found, err := FindConfigFile(startDir)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, clierr.Newf(
			"No %s found in %s or any parent directory. "+
				"Run '%s init' at the root of the repository you want to manage to write one, or point at an existing one with --config <path>. "+
				"For reference, the annotated version, %s, is at %s.",
			brand.ConfigFile, startDir, brand.Name, brand.ExampleFile, exampleFileURL,
		)
	}
	return Load(file)
}
