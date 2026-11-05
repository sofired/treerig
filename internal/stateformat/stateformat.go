// Package stateformat is the one place that says which format version each
// kind of stored state is at, and how a store reacts to a version it does not
// know (docs/design/state.md).
//
// Every store that treerig writes and reads back carries a format version: a
// whole number, 1 for every store but the control home, whose marker says
// "format 2". A store reads a version in one of three ways:
//
//   - the version it writes: read on;
//   - a larger one: written by a newer treerig, which this build cannot
//     interpret, so it refuses with [Newer] and leaves the state untouched;
//   - none, or anything else: not state treerig wrote, so the store treats it
//     as malformed or foreign, the way it treats any other unreadable file.
//
// There is no migration code here: a change to a store's shape raises its
// version, and the build that does so also reads the previous one
// (docs/design/state.md, "Adding a migration").
package stateformat

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
)

// The format version each store writes today. Raising one is a change to that
// store's shape and goes with a migration (docs/design/state.md); the
// numbers are pinned by this package's tests, because they are storage
// contracts like internal/brand's names.
const (
	// ControlHome is the version in the control home's marker file.
	ControlHome = 2
	// IdentityMarker is the version in a checkout's repository-id and
	// worktree-id markers.
	IdentityMarker = 1
	// Registration is the version in a worktree's registry record.
	Registration = 1
	// SharedRecord is the version in a shared service's record.
	SharedRecord = 1
	// RouterState is the version in the router's state.json.
	RouterState = 1
	// LockRecord is the version in a lock's .holder record.
	LockRecord = 1
	// IndexCard is the version in a worktree's index card.
	IndexCard = 1
	// DockerLabel is the value of the format label (brand.FormatLabel) on
	// every container, network and image treerig creates for a worktree or a
	// shared stack. Named volumes carry none (docs/design/state.md).
	DockerLabel = 1
	// TeardownPin is the version in the teardown pin, teardown/pin.json
	// under the control home (docs/design/teardown.md).
	TeardownPin = 1
	// RemovalMarker is the version in a worktree's removal marker, written
	// beside its registration when `worktree remove` starts
	// (docs/design/state.md).
	RemovalMarker = 1
)

// Verdict is how a stored version compares with the one this build writes.
type Verdict int

const (
	// Foreign is no version, an unreadable one, or one this build never
	// wrote (below the current version): not state this build recognises.
	Foreign Verdict = iota
	// Current is exactly the version this build writes.
	Current
	// FromNewer is a version above the one this build knows.
	FromNewer
)

// maxExactVersion is the largest version number a JSON float64 holds exactly.
// A larger one is not a version anyone wrote.
const maxExactVersion = 1 << 53

// ofNumber classifies a version found as a number.
func ofNumber(found float64, supported int) (Verdict, string) {
	if found != math.Trunc(found) || found < 1 || found > maxExactVersion {
		return Foreign, ""
	}
	text := strconv.FormatFloat(found, 'f', -1, 64)
	switch {
	case found == float64(supported):
		return Current, text
	case found > float64(supported):
		return FromNewer, text
	}
	return Foreign, ""
}

// JSON classifies the top-level "version" member of a JSON document against
// supported, and returns the version as written (for a message) when it is
// Newer. It looks at that member alone, ignoring every other key: a newer
// build may have changed the rest of the shape, and a document whose shape
// this build cannot read must still be recognised as newer rather than
// reported as malformed. A document that is not a JSON object is Foreign.
func JSON(data []byte, supported int) (Verdict, string) {
	var probe struct {
		Version *float64 `json:"version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil || probe.Version == nil {
		return Foreign, ""
	}
	return ofNumber(*probe.Version, supported)
}

// Label classifies the value of the format label (brand.FormatLabel) a Docker
// resource carries against supported, the way JSON does a document. An absent
// label is the empty string and is Foreign.
func Label(value string, supported int) (Verdict, string) {
	number, err := strconv.ParseUint(value, 10, 53)
	if err != nil || strconv.FormatUint(number, 10) != value {
		return Foreign, ""
	}
	return ofNumber(float64(number), supported)
}

// Line classifies a "format <n>" line, the form the control home's marker and
// the identity markers use, against supported. The line is given without its
// newline.
func Line(line string, supported int) (Verdict, string) {
	value, ok := strings.CutPrefix(line, "format ")
	if !ok {
		return Foreign, ""
	}
	return Label(value, supported)
}

// FormatLine is the line a store writes for version.
func FormatLine(version int) string { return "format " + strconv.Itoa(version) }

// Newer is the refusal every store gives a version it does not know. verb is
// what the build declined to do ("use" a store, "touch" a Docker resource),
// subject names it, with its path or ID; found is the version it declares, as
// the classifier returned it. The message names the store, the version found
// and the one supported, and says what to do: upgrade treerig. It exits like any
// expected failure, and errors.Is(err, ErrNewer) tells it from every other.
func Newer(verb, subject, found string, supported int) error {
	return &newerError{refusal: clierr.Newf("Refusing to %s %s: it is %s.", verb, subject, Detail(found, supported))}
}

// ErrNewer is what errors.Is finds in every error Newer returns, for a caller
// that must tell a newer treerig's state from a damaged file: the advice for the
// first is to upgrade, never to move it aside.
var ErrNewer = errors.New("state written by a newer treerig")

// newerError is Newer's error: the plain refusal a user is shown, under a type
// of its own that also answers errors.Is(ErrNewer). The refusal is not its
// cause, so a report shows no "caused by" line.
type newerError struct{ refusal *clierr.Error }

// Error is the refusal's message.
func (e *newerError) Error() string { return e.refusal.Error() }

// ExitStatus is the refusal's exit code, so the entry point reports it as an
// expected failure.
func (e *newerError) ExitStatus() int { return e.refusal.ExitStatus() }

// As lets errors.As find the refusal as a *clierr.Error without making it a
// link in the error chain (Unwrap answers nil), so a report prints the message
// once, as lock.BusyError's does.
func (e *newerError) As(target any) bool {
	if ce, ok := target.(**clierr.Error); ok {
		*ce = e.refusal
		return true
	}
	return false
}

// Is reports whether target is ErrNewer.
func (e *newerError) Is(target error) bool { return target == ErrNewer }

// DescribeResource is how a refusal names a Docker resource: the kind with an
// article, its ID, and the Compose service it belongs to where one is known.
func DescribeResource(kind, id, service string) string {
	if service == "" {
		return "the " + kind + " " + id
	}
	return "the " + kind + " " + id + " (service " + service + ")"
}

// UnprovenReason is why treerig cannot prove a Docker resource is what claim
// says ("it is this worktree's"): its format label (label, the full name) is
// missing or not a format number.
func UnprovenReason(label, claim string) string {
	return fmt.Sprintf("its %s label is missing or not recognised, so %s cannot prove %s", label, brand.Name, claim)
}

// Unproven is the rest of a refusal for such a resource: UnprovenReason, and
// the action, since nothing else will remove it.
func Unproven(kind, id, label, claim string) string {
	return UnprovenReason(label, claim) + fmt.Sprintf(". After checking that nothing needs it, remove it with `docker %s rm %s`.", kind, id)
}

// Detail is what Newer says about the state itself, without the verb and
// subject: the version found and the one supported, and what to do. A refusal
// that has its own opening (a lock's) builds on it.
func Detail(found string, supported int) string {
	return fmt.Sprintf("format %s, written by a newer %s, and this build reads format %d. Upgrade %s, then run this again",
		found, brand.Name, supported, brand.Name)
}
