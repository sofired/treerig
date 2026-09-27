package text

import (
	"sync"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// enCollatorOnce and enCollatorVal build the en collator once, on first use,
// rather than at package initialization: every command imports internal/text,
// but only the checkout index (internal/checkoutindex) actually calls
// Collate, and golang.org/x/text/collate's tables add about 1.6 MB to a
// binary that would otherwise pay for them on every startup, including
// `--help`.
//
// The closure that builds the collator is created inside Collate itself,
// not captured by a package-level var: a package-level `var x =
// sync.OnceValue(func() { ... })` still takes that closure's address at
// package initialization — which every command reaches, since every command
// imports this package — so the linker keeps it, and the collate tables it
// references, whether or not Collate is ever called. Deferring the closure
// into Collate's body means the tables are linked in only for a binary that
// actually reaches Collate.
//
// golang.org/x/text's Collator keeps its comparison state (a pair of
// iterators) on the struct itself, so a single instance is not safe for
// concurrent use; enCollateMu serialises access rather than paying for a
// fresh Collator on every pairwise comparison a sort makes.
var (
	enCollatorOnce sync.Once
	enCollatorVal  *collate.Collator
	enCollateMu    sync.Mutex
)

// Collate orders two strings the way the TypeScript build's
// `a.localeCompare(b, 'en')` does: 0 if they collate equal, negative if a
// sorts before b, positive if it sorts after. Every checkout-index sort
// (domain, project, branch, slug) uses it, because the fields it orders
// carry whatever a Git branch or a config's project name allows — accented
// letters, CJK, emoji — not just the restricted alphabet [a-z0-9.-] that
// `internal/registry`'s `sort.Strings` is proven against in
// TestListSortMatchesLocale.
//
// golang.org/x/text/collate's tables (CLDRVersion, UnicodeVersion) are
// generated from CLDR 23 / Unicode 6.2, over a decade behind the CLDR 48 /
// Unicode 17 that Node 24's ICU carries.
// internal/text/testdata/collation-corpus.json is the differential,
// hand-maintained fixture that checks the two still agree on the strings
// berth actually sorts — see TestCollateOracle.
func Collate(a, b string) int {
	enCollatorOnce.Do(func() { enCollatorVal = collate.New(language.English) })
	enCollateMu.Lock()
	defer enCollateMu.Unlock()
	return enCollatorVal.CompareString(a, b)
}
