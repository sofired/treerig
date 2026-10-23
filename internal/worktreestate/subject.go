package worktreestate

import (
	"fmt"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/checkoutindex/card"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/text"
)

// Namespaces is every label namespace a command must ask Docker about to see
// registrations' containers: this build's own, plus one per registration,
// recorded on the registration itself, on its card (cards[i], nil when there is
// none, which may be more current if the registration predates namespace
// tracking), or found on disk wherever the worktree's home currently is.
func Namespaces(controlHome string, registrations []registry.RegisteredWorktree, cards []*card.IndexCard) []string {
	seen := map[string]struct{}{}
	var namespaces []string
	add := func(namespace string) {
		if namespace == "" {
			return
		}
		if _, ok := seen[namespace]; !ok {
			seen[namespace] = struct{}{}
			namespaces = append(namespaces, namespace)
		}
	}
	add(brand.LabelNamespace)
	for i, record := range registrations {
		switch {
		case record.LabelNamespace != nil:
			add(*record.LabelNamespace)
		case cards[i] != nil:
			add(cards[i].LabelNamespace)
		default:
			if namespace := card.RecordedLabelNamespace(controlHome, card.Registration{
				RepositoryID: record.RepositoryID, WorktreeID: record.WorktreeID, LabelNamespace: record.LabelNamespace,
			}); namespace != nil {
				add(*namespace)
			}
		}
	}
	return namespaces
}

// SubjectOf is what the Gatherer is told about a registration: the services
// its card declares, where the card last saw it, and, when the card could not
// be read (readErr, not a card that was never written), that the state cannot
// be told. Every service the card names counts, whether or not it is routed to
// a link: the state is about containers, and does not depend on the router's
// port. The card only holds the services `up` routed, so a service that is not
// routed and was never started is not known here (see docs/commands.md).
func SubjectOf(record registry.RegisteredWorktree, indexCard *card.IndexCard, readErr error) Subject {
	subject := Subject{Registration: record}
	if readErr != nil {
		subject.Unreadable = fmt.Sprintf("Its index card couldn't be read: %s.", text.OneLine(readErr.Error(), 200))
		return subject
	}
	if indexCard == nil {
		return subject
	}
	subject.RecordedRoot = indexCard.Root
	for _, service := range indexCard.Services {
		subject.Declared = append(subject.Declared, service.Name)
	}
	return subject
}
