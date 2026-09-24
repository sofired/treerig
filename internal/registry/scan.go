package registry

import (
	"errors"
	"io/fs"
	"os"

	"github.com/sofired/berth/internal/layout"
)

// RegistryDirectory is where one repository's records live.
func RegistryDirectory(controlHome, repositoryID string) string {
	return layout.RepositoryLayout(controlHome, repositoryID).RegistryDir
}

// WorktreeHome is where one worktree's home lives.
func WorktreeHome(controlHome, repositoryID, worktreeID string) string {
	return layout.WorktreeLayout(controlHome, repositoryID, worktreeID).Home
}

// ScanRegistrations reads every registry on the machine. Never fails for a
// bad record: a file that cannot be read is a Problem, and a hostname
// claimed twice is a Conflict. Conflicting records are still returned — they
// still occupy their hostname, and a lookup that lands on one must see it.
func ScanRegistrations(controlHome string) (RegistryScan, error) {
	repositories := layout.ControlLayout(controlHome).RepositoriesDir
	entries, err := os.ReadDir(repositories)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return RegistryScan{Conflicts: HostnameConflicts{}}, nil
		}
		return RegistryScan{}, err
	}

	var records []RegisteredWorktree
	var problems []RegistryProblem
	for _, entry := range entries {
		if !entry.IsDir() || !uuidPattern.MatchString(entry.Name()) {
			continue
		}
		recs, probs, err := NewStore(controlHome, entry.Name()).Scan()
		if err != nil {
			return RegistryScan{}, err
		}
		records = append(records, recs...)
		problems = append(problems, probs...)
	}

	byHost := map[string][]RegisteredWorktree{}
	for _, record := range records {
		host := layout.WorktreeHost(record.Slug, record.Domain)
		byHost[host] = append(byHost[host], record)
	}
	conflicts := HostnameConflicts{}
	for host, claimants := range byHost {
		if len(claimants) > 1 {
			conflicts[host] = claimants
		}
	}
	return RegistryScan{Records: records, Problems: problems, Conflicts: conflicts}, nil
}
