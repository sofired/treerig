package cli

// Ported from node_modules/commander/lib/suggestSimilar.js.
//
// (The MIT License)
//
// Copyright (c) 2011 TJ Holowaychuk <tj@vision-media.ca>
//
// Permission is hereby granted, free of charge, to any person obtaining
// a copy of this software and associated documentation files (the
// 'Software'), to deal in the Software without restriction, including
// without limitation the rights to use, copy, modify, merge, publish,
// distribute, sublicense, and/or sell copies of the Software, and to
// permit persons to whom the Software is furnished to do so, subject to
// the following conditions:
//
// The above copyright notice and this permission notice shall be
// included in all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED 'AS IS', WITHOUT WARRANTY OF ANY KIND,
// EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
// MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.
// IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY
// CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT,
// TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE
// SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

import (
	"sort"
	"strings"
)

const maxDistance = 3

// editDistance is the optimal string alignment (Damerau-Levenshtein, no
// substring edited more than once) distance between a and b, operating on
// runes rather than JavaScript's UTF-16 code units — every candidate word in
// this tree's command and flag names is ASCII, so the two agree here.
func editDistance(a, b []rune) int {
	if abs(len(a)-len(b)) > maxDistance {
		return max(len(a), len(b))
	}

	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
		d[i][0] = i
	}
	for j := 0; j <= len(b); j++ {
		d[0][j] = j
	}

	for j := 1; j <= len(b); j++ {
		for i := 1; i <= len(a); i++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(a)][len(b)]
}

// suggestSimilar is suggestSimilar.js's function of the same name: a
// "(Did you mean X?)" or "(Did you mean one of X, Y?)" hint, or "" when
// nothing is close enough, prefixed with a newline as commander's is.
func suggestSimilar(word string, candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	candidates = dedupe(candidates)

	searchingOptions := strings.HasPrefix(word, "--")
	if searchingOptions {
		word = word[2:]
		for i, c := range candidates {
			candidates[i] = strings.TrimPrefix(c, "--")
		}
	}

	wordRunes := []rune(word)
	var similar []string
	bestDistance := maxDistance
	const minSimilarity = 0.4
	for _, candidate := range candidates {
		if len([]rune(candidate)) <= 1 {
			continue // no one character guesses
		}
		candidateRunes := []rune(candidate)
		distance := editDistance(wordRunes, candidateRunes)
		length := max(len(wordRunes), len(candidateRunes))
		similarity := float64(length-distance) / float64(length)
		if similarity > minSimilarity {
			switch {
			case distance < bestDistance:
				bestDistance = distance
				similar = []string{candidate}
			case distance == bestDistance:
				similar = append(similar, candidate)
			}
		}
	}

	sort.Strings(similar)
	if searchingOptions {
		for i, c := range similar {
			similar[i] = "--" + c
		}
	}

	switch len(similar) {
	case 0:
		return ""
	case 1:
		return "\n(Did you mean " + similar[0] + "?)"
	default:
		return "\n(Did you mean one of " + strings.Join(similar, ", ") + "?)"
	}
}

// dedupe keeps the first occurrence of each string, the way JS's
// `Array.from(new Set(candidates))` does.
func dedupe(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
