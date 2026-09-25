package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/shape"
)

// FindConfigFile walks upward from startDir looking for berth.toml, stopping
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
	obj, err := decodeOrderedTOML(string(contents))
	if err != nil {
		return Config{}, clierr.Newf("%s is not valid TOML: %s", file, err.Error())
	}

	value, issues := shape.Parse(rootSchema(), obj)
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

// Load reads and validates one file.
func Load(file string) (*Loaded, error) {
	resolved, err := layout.Resolve(file)
	if err != nil {
		return nil, err
	}
	contents, err := os.ReadFile(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, clierr.Newf("No %s at %s.", brand.ConfigFile, resolved)
		}
		return nil, err
	}
	cfg, err := Parse(contents, resolved)
	if err != nil {
		return nil, err
	}
	return &Loaded{Config: cfg, File: resolved, Root: filepath.Dir(resolved)}, nil
}

// LoadNearest loads the nearest berth.toml above startDir, or fails with a
// hint that names something that exists.
func LoadNearest(startDir string) (*Loaded, error) {
	file, found, err := FindConfigFile(startDir)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, clierr.Newf(
			"No %s found in %s or any parent directory. "+
				"Create it at the root of the repository you want to manage, or point at an existing one with --config <path>. "+
				"The Quick start in %s's README has a minimal template, and %s in "+
				"%s's own source tree is the annotated version.",
			brand.ConfigFile, startDir, brand.Name, brand.ExampleFile, brand.Name,
		)
	}
	return Load(file)
}
