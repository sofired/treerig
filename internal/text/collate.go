package text

import (
	"sync"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// enCollator is shared across every call to Collate. golang.org/x/text's
// Collator keeps its comparison state (a pair of iterators) on the struct
// itself, so a single instance is not safe for concurrent use; enCollateMu
// serialises access rather than paying for a fresh Collator — and the
// MatchLang lookup and table load it repeats — on every pairwise comparison
// a sort makes.
var (
	enCollator  = collate.New(language.English)
	enCollateMu sync.Mutex
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
// Unicode 17 that Node 24's ICU carries. internal/text/testdata/collation-
// corpus.json (tools/oracle/collation-corpus.ts) is the differential corpus
// that checks the two still agree on the strings berth actually sorts —
// see TestCollateOracle.
func Collate(a, b string) int {
	enCollateMu.Lock()
	defer enCollateMu.Unlock()
	return enCollator.CompareString(a, b)
}
