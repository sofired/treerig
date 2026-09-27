package text

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"

	"golang.org/x/text/collate"
)

// collationFixture is the collation gate (#40): it checks that
// golang.org/x/text/collate's `en` table (CLDR 23 / Unicode 6.2) agrees with
// Node 24's ICU (CLDR 48 / Unicode 17) on
// internal/text/testdata/collation-corpus.json, a hand-maintained fixture
// generated from real `localeCompare(…, 'en')` calls; its origin is in git
// history.
//
// The corpus is split in two. `realistic` is everything a Git branch, a
// config's project name, or a domain label plausibly is: ASCII, case pairs,
// punctuation and hyphens against letters, digit runs, Latin-1 and accented
// letters (precomposed and decomposed), and shared prefixes — several
// thousand strings, and Collate must reproduce Node's order and every
// pairwise sign on all of them, exactly. `wide` is CJK, Hangul, Kana, emoji,
// and an exhaustive sweep of every combining diacritical mark: real
// disagreements exist there (see TestCollateWideScriptsDiverge), because
// x/text/collate's tables predate most of it. Domain, project, and slug are
// ASCII only when `up` writes them from a validated config — the card
// schema itself accepts any non-empty text for
// all three, so a hand-edited or third-party-written card can carry any
// script in any field. In practice, branch names are what actually carry
// these scripts: they come straight from Git, entirely unconstrained, and
// the index's sort key falls back to the branch before the slug. Collate
// orders every comparison the checkout index makes, in both RenderIndexHTML
// (the HTML page) and FormatIndexText (the `berth index` text output), since
// both read the same sorted entries and call the same sections: row order
// within a table (by project, then by branch or slug); the project names
// domainHeading joins into a table's heading; and the tables themselves,
// since sections sorts by heading. A table's checkouts-N id, assigned by its
// position among the sorted tables, can as a result also differ — in the
// HTML page only, since the text output carries no such id. Each of these
// differs only where the specific strings being compared carry a wide
// script; no row, name, or table is ever added, dropped, or renamed.
//
// The fixture records only `sorted`, not the unsorted corpus too — the same
// multiset, just reordered, would double the file for no extra signal.
// `tiedIndices` names every index i where sorted[i] and sorted[i+1] collate
// exactly equal under Node's own localeCompare, e.g. a precomposed letter
// against its decomposed form — a plain []int rather than a parallel array
// of {value, tiedWithNext} objects, which cost more once JSON-serialized
// than the corpus itself. sortMismatches reverses `sorted` (a deterministic
// permutation with no unsorted array to keep in step) and sorts it again
// with Collate; since a stable sort only promises that tied elements keep
// whatever relative order they arrive in, and reversing deliberately
// scrambles that order, two elements in the same tied run may swap places
// without counting as a disagreement — anything else must land back exactly
// where Node put it.
type collationFixture struct {
	ICU       string          `json:"icu"`
	CLDR      string          `json:"cldr"`
	Unicode   string          `json:"unicode"`
	Realistic collationSubset `json:"realistic"`
	Wide      collationSubset `json:"wide"`
}

type collationSubset struct {
	Sorted      []string               `json:"sorted"`
	TiedIndices []int                  `json:"tiedIndices"`
	Pairs       []collationFixturePair `json:"pairs"`
}

type collationFixturePair struct {
	Left  string `json:"left"`
	Right string `json:"right"`
	Sign  int    `json:"sign"`
}

// loadCollationFixture reads and parses testdata/collation-corpus.json,
// failing the test if it is missing, malformed, or empty.
func loadCollationFixture(t *testing.T) collationFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/collation-corpus.json")
	if err != nil {
		t.Fatalf("reading fixture data: %v", err)
	}
	var fixture collationFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("parsing fixture data: %v", err)
	}
	if len(fixture.Realistic.Sorted) == 0 || len(fixture.Wide.Sorted) == 0 {
		t.Fatal("empty fixture corpus")
	}
	return fixture
}

// TestCollateRealisticSortOrder sorts a reversed copy of the realistic
// subset with Collate and requires the exact order Node's own
// localeCompare('en') produced, up to the reordering of ties that reversal
// itself introduces (see sortMismatches). A failure here means
// x/text/collate cannot stand in for localeCompare on the strings berth
// actually sorts, and the checkout-index renderer must not rely on Collate
// until that is resolved.
func TestCollateRealisticSortOrder(t *testing.T) {
	fixture := loadCollationFixture(t)
	t.Logf("Node ICU %s / CLDR %s / Unicode %s; golang.org/x/text/collate CLDR %s / Unicode %s",
		fixture.ICU, fixture.CLDR, fixture.Unicode, collate.CLDRVersion, collate.UnicodeVersion)

	mismatches, detail := sortMismatches(fixture.Realistic.Sorted, fixture.Realistic.TiedIndices)
	for _, line := range detail {
		t.Error(line)
	}
	if mismatches > 0 {
		t.Fatalf("%d of %d realistic positions disagree with Node's localeCompare('en') order", mismatches, len(fixture.Realistic.Sorted))
	}
}

// TestCollateRealisticPairs checks the pairwise sign Node's
// localeCompare('en') gave for every consecutive pair of the realistic
// corpus, plus an all-pairs cross-check over a handful of single-character
// probes, independent of overall sort-order agreement.
func TestCollateRealisticPairs(t *testing.T) {
	fixture := loadCollationFixture(t)
	checkPairs(t, fixture.Realistic.Pairs)
}

// TestCollateWideScriptsDiverge pins the accepted difference: for CJK,
// Hangul, Kana, emoji, and the full sweep of combining diacritical marks,
// x/text/collate's CLDR-23 tables do not always agree with Node's CLDR-48
// ICU. The test does not require agreement — it requires that the *known*
// disagreement is what still disagrees, so a future x/text/collate update
// that closes the gap is noticed (the mismatch count drops to zero) rather
// than silently kept.
func TestCollateWideScriptsDiverge(t *testing.T) {
	fixture := loadCollationFixture(t)

	sortMismatchCount, _ := sortMismatches(fixture.Wide.Sorted, fixture.Wide.TiedIndices)
	if sortMismatchCount == 0 {
		t.Fatal("x/text/collate now reproduces Node's order for CJK/Hangul/Kana/emoji/combining-mark strings; " +
			"promote the wide corpus into the realistic gate")
	}
	t.Logf("%d of %d wide-script positions disagree with Node's localeCompare('en') order (expected)",
		sortMismatchCount, len(fixture.Wide.Sorted))

	pairMismatches := 0
	for _, pair := range fixture.Wide.Pairs {
		if sign(Collate(pair.Left, pair.Right)) != pair.Sign {
			pairMismatches++
		}
	}
	if pairMismatches == 0 {
		t.Fatal("x/text/collate now agrees with Node on every sampled wide-script pair; " +
			"promote the wide corpus into the realistic gate")
	}
	t.Logf("%d of %d wide-script pairs disagree with Node's localeCompare('en') sign (expected)",
		pairMismatches, len(fixture.Wide.Pairs))
}

// sortMismatches reverses want (a deterministic permutation) and sorts the
// reversal with Collate, then compares the result back against want run by
// run, where a run is a maximal stretch of indices tiedIndices marks as each
// tied with the next. Elements may land anywhere within their own run
// without counting as a mismatch — a stable sort only promises that tied
// elements keep whatever relative order they arrive in, and reversal
// deliberately inverts that order for every tie — but every run's
// *membership*, and every non-tied element's exact position, must match.
// detail holds up to 25 human-readable descriptions of the runs that
// disagree.
func sortMismatches(want []string, tiedIndices []int) (mismatches int, detail []string) {
	tied := make(map[int]bool, len(tiedIndices))
	for _, index := range tiedIndices {
		tied[index] = true
	}

	got := reversedCopy(want)
	slices.SortStableFunc(got, Collate)

	i := 0
	for i < len(want) {
		j := i + 1
		for tied[j-1] {
			j++
		}
		wantRun := want[i:j]
		var gotRun []string
		if j <= len(got) {
			gotRun = got[i:j]
		}
		if !sameMultiset(gotRun, wantRun) {
			mismatches += j - i
			if len(detail) < 25 {
				detail = append(detail, fmt.Sprintf("positions %d..%d: Collate gives %q, localeCompare('en') gives %q", i, j-1, gotRun, wantRun))
			}
		}
		i = j
	}
	return mismatches, detail
}

// reversedCopy is a new slice holding values back to front, leaving values
// itself untouched.
func reversedCopy(values []string) []string {
	reversed := make([]string, len(values))
	for i, v := range values {
		reversed[len(values)-1-i] = v
	}
	return reversed
}

// sameMultiset reports whether a and b hold the same elements the same
// number of times, regardless of order — exactly what "may swap places
// within a tied run" means.
func sameMultiset(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, v := range a {
		counts[v]++
	}
	for _, v := range b {
		counts[v]--
	}
	for _, c := range counts {
		if c != 0 {
			return false
		}
	}
	return true
}

// checkPairs requires Collate's sign to match Node's localeCompare('en')
// sign for every pair, reporting up to 25 disagreements before failing.
func checkPairs(t *testing.T, pairs []collationFixturePair) {
	t.Helper()
	mismatches := 0
	for _, pair := range pairs {
		got := sign(Collate(pair.Left, pair.Right))
		if got != pair.Sign {
			mismatches++
			if mismatches <= 25 {
				t.Errorf("Collate(%q, %q) sign = %d, localeCompare('en') sign = %d", pair.Left, pair.Right, got, pair.Sign)
			}
		}
	}
	if mismatches > 0 {
		t.Fatalf("%d of %d pairs disagree with Node's localeCompare('en') sign", mismatches, len(pairs))
	}
}

// sign is JavaScript's Math.sign, clamped to -1, 0, or 1.
func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}
