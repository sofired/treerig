package teardown

import (
	"maps"

	"github.com/sofired/treerig/internal/semver"
)

// Kind is what an update does to the existing pin.
type Kind int

const (
	// Written is the first pin: there was none.
	Written Kind = iota
	// Merged keeps the existing pin and adds the incoming platform entry: the
	// same verified checksums.txt, another platform of the same release.
	Merged
	// Replaced puts the incoming pin in place of the existing one.
	Replaced
)

// Outcome is the result of Update.
type Outcome struct {
	// Pin is what to write.
	Pin Pin
	// Kind says which rule applied.
	Kind Kind
	// Older is true when the incoming release ranks below the existing pin's,
	// a downgrade, which is allowed; the plan names it as "(current pin: 1.3.0;
	// this is older)". Never true for a merge, or when either pin is a file pin
	// (labels are not compared), or when either release does not parse.
	Older bool
}

// Update applies the rules of "Updating an existing pin"
// (docs/design/teardown.md) to the pin on disk (existing, found false for none;
// a newer or foreign one is ReadPin's refusal, before this) and the pin a
// command has just verified (incoming). It is pure: it reads and writes
// nothing. In order:
//
//  1. no pin: incoming is written;
//  2. the same non-empty checksums_sha256 (the same release, another platform):
//     the pins are merged, see merge;
//  3. anything else: incoming replaces the pin, a downgrade included. A file
//     pin's checksums_sha256 is empty, so it never merges.
func Update(existing Pin, found bool, incoming Pin) Outcome {
	if !found {
		return Outcome{Pin: incoming, Kind: Written}
	}
	if existing.ChecksumsSHA256 != "" && existing.ChecksumsSHA256 == incoming.ChecksumsSHA256 {
		return Outcome{Pin: merge(existing, incoming), Kind: Merged}
	}
	return Outcome{Pin: incoming, Kind: Replaced, Older: older(existing, incoming)}
}

// merge is existing with incoming folded in, platform by platform:
//
//  1. a platform only incoming names is added;
//  2. a platform both name (they carry the same archive digest, since one
//     verified checksums.txt lists one) takes incoming's binary_sha256, and
//     keeps the existing one when incoming recorded none;
//  3. platforms incoming omits, and the existing release, source and signer,
//     are kept;
//  4. pinned_at is refreshed.
//
// Two pins with the same checksums_sha256 and different archive digests for a
// platform cannot come from one verified checksums.txt, so that case is
// unreachable from a command; if it were reached, the incoming entry would
// replace the existing one whole.
func merge(existing, incoming Pin) Pin {
	merged := existing
	merged.PinnedAt = incoming.PinnedAt
	merged.Platforms = maps.Clone(existing.Platforms)
	for key, entry := range incoming.Platforms {
		current, ok := merged.Platforms[key]
		if ok && current.ArchiveSHA256 == entry.ArchiveSHA256 && entry.BinarySHA256 == "" {
			continue
		}
		merged.Platforms[key] = entry
	}
	return merged
}

// older reports whether incoming's release ranks below existing's. File labels
// are not versions and are not compared.
func older(existing, incoming Pin) bool {
	if existing.IsFile() || incoming.IsFile() {
		return false
	}
	current, err := semver.Parse(existing.Release)
	if err != nil {
		return false
	}
	next, err := semver.Parse(incoming.Release)
	if err != nil {
		return false
	}
	return semver.Compare(next, current) < 0
}
