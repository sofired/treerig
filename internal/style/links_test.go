package style

import (
	"bytes"
	"fmt"
	"html"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"

	"github.com/sofired/treerig/internal/brand"
)

// A Markdown link points at a file, and often at a heading in it. The checks
// here fail when a relative link, a reference definition or a heading anchor
// no longer resolves, so moving text between pages cannot silently break one.
// Links in tracked files that are not Markdown (Go comments, .goreleaser.yaml)
// are checked when they name the README or a page under docs/.
//
// Anchor matching is deliberately case-sensitive: GitHub writes every heading
// anchor in lower case, so a link with capitals is a typo to fix, not a
// spelling to forgive.

var (
	// linkDefinition matches the start of a reference definition, [id]:, and
	// captures the id; the target after it is read with scanTarget.
	linkDefinition = regexp.MustCompile(`^ {0,3}\[([^\]^][^\]]*)\]:[ \t]*`)
	// refUse matches the id of a [text][id] reference link, from the closing
	// bracket of the text. Only the full form is matched: the bare [id] and
	// the collapsed [text][] are left unchecked, because prose uses brackets
	// alone. A use is checked only when its id holds a run of two letters
	// (see idWord), so that array indexing in prose, matrix[i][j] or a[0][1],
	// is not taken for a reference.
	refUse = regexp.MustCompile(`\]\[([^\]]+)\]`)
	// idWord matches the run of letters that makes a [text][id] id look like
	// a reference and not like an index.
	idWord = regexp.MustCompile(`\p{L}{2}`)
	// htmlLink matches the href or src attribute of an <a> or <img> tag.
	htmlLink = regexp.MustCompile(`(?i)<(?:a|img)\b[^>]*?\s(?:href|src)\s*=\s*["']([^"']*)["']`)
	// quotePrefix matches the block quote markers at the start of a line.
	quotePrefix = regexp.MustCompile(`^(?: {0,3}>[ ]?)+`)
	// atxHeading matches a heading line and captures its text.
	atxHeading = regexp.MustCompile(`^ {0,3}#{1,6}(?:[ \t]+(.*?))?[ \t]*$`)
	// setextUnderline matches the === or --- line under a setext heading.
	setextUnderline = regexp.MustCompile(`^ {0,3}(?:=+|-+)[ \t]*$`)
	// listItem matches the start of a list item.
	listItem = regexp.MustCompile(`^\s*(?:[-*+]|\d{1,9}[.)])(?:\s|$)`)
	// headingClosing matches the optional closing run of # in a heading.
	headingClosing = regexp.MustCompile(`(?:^|[ \t]+)#+$`)
	// fenceLine matches a line that opens or closes a fenced code block.
	fenceLine = regexp.MustCompile("^\\s*(`{3,}|~{3,})(.*)$")
	// htmlAnchor matches an explicit <a id="x"> or <a name="x"> anchor.
	htmlAnchor = regexp.MustCompile(`(?i)<a\s[^>]*?\b(?:id|name)\s*=\s*["']([^"']+)["']`)
	// headingLink and headingRefLink unwrap links in heading text, and
	// htmlTag drops tags, the way GitHub renders a heading before slugging it.
	headingLink    = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	headingRefLink = regexp.MustCompile(`\[([^\]]*)\]\[[^\]]*\]`)
	htmlTag        = regexp.MustCompile(`<[^>]+>`)
	// scheme matches a URL scheme such as https: or mailto:.
	scheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
	// sourceLine matches a #L12 or #L12-L20 line anchor, which names a line
	// of a source view and not a heading.
	sourceLine = regexp.MustCompile(`^L\d+(?:-L\d+)?$`)
	// pageLink finds a README or docs page followed by an anchor in text that
	// is not Markdown. The leading group keeps a longer path from matching.
	pageLink = regexp.MustCompile(`(^|[^A-Za-z0-9_./-])(README\.md|docs/[A-Za-z0-9_./-]+\.md)#([\p{L}\p{M}\p{N}_%-]+)`)
	// repoURL finds a URL of the repository on GitHub that names a page of
	// it: either an anchor, which GitHub resolves against the README, or a
	// blob or tree path with an optional anchor.
	repoURL = regexp.MustCompile(`https://github\.com/` + regexp.QuoteMeta(brand.Repository) +
		`(?:/(?:blob|tree)/([^/\s"'<>()#?]+)/([^\s"'<>()#?]*))?(?:/?#([\p{L}\p{M}\p{N}_%-]+))?`)
)

// mdLink is one link target found in a Markdown file.
type mdLink struct {
	line   int
	target string
}

// mdDoc is what the checker needs to know about one Markdown file: the
// anchors its headings and HTML anchors define, the reference ids it defines,
// the links it makes and the [text][id] references it uses.
type mdDoc struct {
	anchors map[string]bool
	defs    map[string]bool
	links   []mdLink
	refs    []mdLink
}

// linkReport is the outcome of one run of the checker.
type linkReport struct {
	problems      []string
	markdownFiles int
	links         int
}

// mapCodeSpans replaces each inline code span of a line, backticks included,
// with what f returns for the span and for its content. A run of backticks
// with no closing run of the same length stays as text.
func mapCodeSpans(line string, f func(span, inner string) string) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		if line[i] != '`' {
			b.WriteByte(line[i])
			i++
			continue
		}
		start := i
		for i < len(line) && line[i] == '`' {
			i++
		}
		n := i - start
		closeStart, closeEnd := -1, -1
		for j := i; j < len(line); {
			if line[j] != '`' {
				j++
				continue
			}
			k := j
			for k < len(line) && line[k] == '`' {
				k++
			}
			if k-j == n {
				closeStart, closeEnd = j, k
				break
			}
			j = k
		}
		if closeStart < 0 {
			b.WriteString(line[start:i])
			continue
		}
		b.WriteString(f(line[start:closeEnd], line[i:closeStart]))
		i = closeEnd
	}
	return b.String()
}

// blankCodeSpans replaces inline code spans in a line with spaces, so a link
// written inside one is not read as a link.
func blankCodeSpans(line string) string {
	return mapCodeSpans(line, func(span, _ string) string { return strings.Repeat(" ", len(span)) })
}

// isWordRune reports whether r is a letter or a digit.
func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// isASCIIPunct reports whether r is an ASCII punctuation or symbol character,
// the characters a backslash can escape.
func isASCIIPunct(r rune) bool {
	return r < unicode.MaxASCII && (unicode.IsPunct(r) || unicode.IsSymbol(r))
}

// stripEmphasis removes the markers of emphasis from heading text: every
// "*", and each "_" that starts or ends emphasis. An underscore inside a word
// is not a marker, so snake_case keeps its underscores, and neither is one
// without a partner, so _config keeps its own. A backslash before ASCII
// punctuation is an escape: it is dropped and the character after it is kept
// as it is.
func stripEmphasis(text string) string {
	rs := []rune(text)
	out := make([]rune, 0, len(rs))
	var marks []int // indexes in out of underscores that could be markers
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\\' && i+1 < len(rs) && isASCIIPunct(rs[i+1]):
			i++
			out = append(out, rs[i])
		case r == '*':
		case r == '_':
			if !(i > 0 && i+1 < len(rs) && isWordRune(rs[i-1]) && isWordRune(rs[i+1])) {
				marks = append(marks, len(out))
			}
			out = append(out, r)
		default:
			out = append(out, r)
		}
	}
	drop := map[int]bool{}
	for _, m := range marks[:len(marks)/2*2] {
		drop[m] = true
	}
	var b strings.Builder
	for i, r := range out {
		if !drop[i] {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// headingSlug turns heading text into the anchor GitHub gives it. Code spans
// keep their text, tags outside them and emphasis markers are removed, the
// text is lower-cased, only letters, marks, numbers, connector punctuation
// and "-" are kept, and each space becomes "-". Numbering of duplicates is
// the caller's job.
func headingSlug(text string) string {
	const first = 0xE000 // private-use runes stand in for code spans
	var spans []string
	text = mapCodeSpans(text, func(_, inner string) string {
		spans = append(spans, inner)
		return string(rune(first + len(spans) - 1))
	})
	text = headingRefLink.ReplaceAllString(headingLink.ReplaceAllString(text, "$1"), "$1")
	text = html.UnescapeString(stripEmphasis(htmlTag.ReplaceAllString(text, "")))
	var b strings.Builder
	for _, r := range text {
		if n := int(r) - first; n >= 0 && n < len(spans) {
			b.WriteString(spans[n])
			continue
		}
		b.WriteRune(r)
	}
	var out strings.Builder
	for _, r := range strings.ToLower(b.String()) {
		switch {
		case r == ' ':
			out.WriteByte('-')
		case r == '-' || unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsNumber(r) || unicode.Is(unicode.Pc, r):
			out.WriteRune(r)
		}
	}
	return out.String()
}

// scanTarget reads a link target that starts at s[i], just after "](": an
// <angle> form that may hold spaces, or text up to whitespace with its
// parentheses balanced, then an optional title in quotes or parentheses. It
// returns the target and the index after what it consumed.
func scanTarget(s string, i int) (string, int) {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	var target string
	if i < len(s) && s[i] == '<' {
		j := strings.IndexByte(s[i+1:], '>')
		if j < 0 {
			return "", i
		}
		target, i = s[i+1:i+1+j], i+j+2
	} else {
		start, depth := i, 0
	read:
		for ; i < len(s); i++ {
			switch s[i] {
			case ' ', '\t':
				break read
			case '(':
				depth++
			case ')':
				if depth == 0 {
					break read
				}
				depth--
			}
		}
		target = s[start:i]
	}
	j := i
	for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
		j++
	}
	if j > i && j < len(s) {
		closer := map[byte]byte{'"': '"', '\'': '\'', '(': ')'}[s[j]]
		if closer != 0 {
			if k := strings.IndexByte(s[j+1:], closer); k >= 0 {
				i = j + k + 2
			}
		}
	}
	return target, i
}

// inlineTargets returns the target of every [text](target) in a line.
func inlineTargets(text string) []string {
	var out []string
	for from := 0; ; {
		i := strings.Index(text[from:], "](")
		if i < 0 {
			return out
		}
		target, end := scanTarget(text, from+i+2)
		if target != "" {
			out = append(out, target)
		}
		from = max(end, from+i+2)
	}
}

// refKey normalises a reference id: case-insensitive, whitespace collapsed.
func refKey(id string) string {
	return strings.ToLower(strings.Join(strings.Fields(id), " "))
}

// leadingIndent counts the columns of indentation of a line, a tab being four.
func leadingIndent(line string) int {
	n := 0
	for _, r := range line {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}

// parseMarkdown reads a Markdown file's heading anchors, reference
// definitions and links, leaving out fenced and indented code blocks and
// inline code spans. Block quote markers are ignored, so a heading or a fence
// inside a quote counts.
func parseMarkdown(data []byte) mdDoc {
	doc := mdDoc{anchors: map[string]bool{}, defs: map[string]bool{}}
	seen := map[string]int{}
	addHeading := func(text string) {
		slug := headingSlug(headingClosing.ReplaceAllString(strings.TrimSpace(text), ""))
		anchor := slug
		for n := seen[slug]; doc.anchors[anchor]; n++ {
			anchor = fmt.Sprintf("%s-%d", slug, n+1)
			seen[slug] = n + 1
		}
		doc.anchors[anchor] = true
	}
	var (
		fence     string
		para      []string
		inList    bool
		prevBlank = true
		prevCode  bool
	)
	for i, raw := range strings.Split(string(data), "\n") {
		line := quotePrefix.ReplaceAllString(strings.TrimSuffix(raw, "\r"), "")
		blank := strings.TrimSpace(line) == ""
		wasBlank := prevBlank
		prevBlank = blank
		if fence != "" {
			if m := fenceLine.FindStringSubmatch(line); m != nil && m[1][0] == fence[0] && len(m[1]) >= len(fence) && strings.TrimSpace(m[2]) == "" {
				fence = ""
			}
			continue
		}
		if blank {
			para = nil
			continue
		}
		indent := leadingIndent(line)
		item := listItem.MatchString(line)
		if code := indent >= 4 && !inList && (wasBlank || prevCode); code {
			prevCode = true
			continue
		}
		prevCode = false
		if m := fenceLine.FindStringSubmatch(line); m != nil {
			if m[1][0] != '`' || !strings.Contains(m[2], "`") {
				fence = m[1]
			}
			para = nil
			continue
		}
		if indent == 0 || item {
			inList = item
		}
		if setextUnderline.MatchString(line) && len(para) > 0 {
			addHeading(strings.Join(para, " "))
			para = nil
			continue
		}
		if m := atxHeading.FindStringSubmatch(line); m != nil {
			addHeading(m[1])
			para = nil
		} else if trimmed := strings.TrimSpace(line); item || strings.HasPrefix(trimmed, "|") || strings.HasPrefix(trimmed, "<") || linkDefinition.MatchString(line) {
			para = nil
		} else {
			para = append(para, strings.TrimSpace(line))
		}

		text := blankCodeSpans(line)
		for _, m := range htmlAnchor.FindAllStringSubmatch(text, -1) {
			doc.anchors[m[1]] = true
		}
		if m := linkDefinition.FindStringSubmatch(text); m != nil {
			doc.defs[refKey(m[1])] = true
			if target, _ := scanTarget(text, len(m[0])); target != "" {
				doc.links = append(doc.links, mdLink{i + 1, target})
			}
		}
		for _, t := range inlineTargets(text) {
			doc.links = append(doc.links, mdLink{i + 1, t})
		}
		for _, m := range htmlLink.FindAllStringSubmatch(text, -1) {
			if m[1] != "" {
				doc.links = append(doc.links, mdLink{i + 1, m[1]})
			}
		}
		for _, m := range refUse.FindAllStringSubmatch(text, -1) {
			doc.refs = append(doc.refs, mdLink{i + 1, m[1]})
		}
	}
	return doc
}

// linkChecker resolves link targets against the tracked files of a tree.
type linkChecker struct {
	root  string
	files map[string]bool
	dirs  map[string]bool
	docs  map[string]mdDoc
}

// resolve reports why a link target does not resolve, or "" when it does.
// The target is a path, an anchor, or both, and it is read as written in the
// file src: relative to src's directory, or to the repository root when it
// starts with "/". A src of "" stands for the root, for a target that is
// relative to it rather than to the file that cites it. An anchor is checked
// only when the target is a tracked Markdown file.
func (c *linkChecker) resolve(src, target string) string {
	pathPart, frag, _ := strings.Cut(target, "#")
	if i := strings.IndexByte(pathPart, '?'); i >= 0 {
		pathPart = pathPart[:i]
	}
	pathPart, err := url.PathUnescape(pathPart)
	if err != nil {
		return fmt.Sprintf("bad escape in the path: %v", err)
	}
	frag, err = url.PathUnescape(frag)
	if err != nil {
		return fmt.Sprintf("bad escape in the anchor: %v", err)
	}
	file := src
	if pathPart != "" {
		if rooted := strings.TrimPrefix(pathPart, "/"); rooted != pathPart {
			file = path.Clean(rooted)
		} else {
			file = path.Clean(path.Join(path.Dir(src), pathPart))
		}
		if file == ".." || strings.HasPrefix(file, "../") {
			return "points outside the repository"
		}
		if file != "." && !c.files[file] && !c.dirs[file] {
			if _, err := os.Stat(filepath.Join(c.root, filepath.FromSlash(file))); err == nil {
				return "exists but is not tracked"
			}
			return "no such file"
		}
	}
	if frag == "" || sourceLine.MatchString(frag) {
		return ""
	}
	doc, ok := c.docs[file]
	if !ok {
		return ""
	}
	if !doc.anchors[frag] {
		return fmt.Sprintf("no heading #%s in %s", frag, file)
	}
	return ""
}

// repoTarget maps a match of repoURL to the repository path it names, with
// its anchor: the README for the bare form, the named file for a blob or tree
// URL. It reports false for a URL that names neither, such as the plain
// repository address, and for a blob or tree URL at a ref other than main or
// master: a tag or another branch may legitimately point at an older tree, in
// which a page or heading has not moved yet, so the working tree says nothing
// about it.
func repoTarget(m []string) (string, bool) {
	if m[1] != "" && m[1] != "main" && m[1] != "master" {
		return "", false
	}
	pagePath := strings.TrimRight(m[2], ".,;:")
	if pagePath == "" && m[3] == "" {
		return "", false
	}
	if pagePath == "" {
		pagePath = "README.md"
	}
	if m[3] != "" {
		pagePath += "#" + m[3]
	}
	return pagePath, true
}

// markdownTarget returns the target to resolve for a link found in Markdown,
// or "" when it is not checked: protocol-relative URLs and URLs with a scheme
// are skipped, except a URL of the repository's own pages, which is mapped to
// the tracked path it names. Root-absolute paths are resolved from the root.
func markdownTarget(target string) string {
	if strings.HasPrefix(target, "https://") {
		if m := repoURL.FindStringSubmatch(target); m != nil && strings.HasPrefix(target, m[0]) {
			t, _ := repoTarget(m)
			return t
		}
	}
	if target == "" || strings.HasPrefix(target, "//") || scheme.MatchString(target) {
		return ""
	}
	return target
}

// checkLinks checks the links of every tracked file under root. tracked holds
// slash-separated paths relative to root, as git ls-files prints them; a
// tracked path missing from the working tree is ignored.
func checkLinks(root string, tracked []string) (linkReport, error) {
	c := &linkChecker{root: root, files: map[string]bool{}, dirs: map[string]bool{}, docs: map[string]mdDoc{}}
	var rep linkReport
	var problems []string
	contents := map[string][]byte{}
	for _, name := range tracked {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			continue
		}
		c.files[name] = true
		for d := path.Dir(name); d != "."; d = path.Dir(d) {
			c.dirs[d] = true
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return rep, fmt.Errorf("read %s: %w", name, err)
		}
		contents[name] = data
		if strings.HasSuffix(name, ".md") {
			c.docs[name] = parseMarkdown(data)
		}
	}

	names := make([]string, 0, len(contents))
	for name := range contents {
		names = append(names, name)
	}
	sort.Strings(names)
	report := func(src string, line int, shown, reason string) {
		rep.links++
		if reason != "" {
			problems = append(problems, fmt.Sprintf("%s:%d: %s: %s", src, line, shown, reason))
		}
	}
	for _, name := range names {
		if doc, ok := c.docs[name]; ok {
			rep.markdownFiles++
			for _, l := range doc.links {
				if target := markdownTarget(l.target); target != "" {
					report(name, l.line, l.target, c.resolve(name, target))
				}
			}
			for _, r := range doc.refs {
				if !idWord.MatchString(r.target) {
					continue
				}
				reason := ""
				if !doc.defs[refKey(r.target)] {
					reason = "no reference definition in this file"
				}
				report(name, r.line, "["+r.target+"]", reason)
			}
			continue
		}
		data := contents[name]
		if bytes.IndexByte(data, 0) >= 0 {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, m := range pageLink.FindAllStringSubmatch(line, -1) {
				report(name, i+1, m[2]+"#"+m[3], c.resolve("", m[2]+"#"+m[3]))
			}
			for _, m := range repoURL.FindAllStringSubmatch(line, -1) {
				if target, ok := repoTarget(m); ok {
					report(name, i+1, m[0], c.resolve("", target))
				}
			}
		}
	}
	sort.Strings(problems)
	rep.problems = problems
	return rep, nil
}

// TestMarkdownLinksResolve fails when a tracked Markdown file links to a
// file that is missing or untracked, or to a heading that does not exist, and
// when a tracked file of any other kind cites an anchor of the README or of a
// page under docs/ that is gone. Every problem is reported at once.
func TestMarkdownLinksResolve(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := checkLinks(root, trackedFiles(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if rep.markdownFiles == 0 {
		t.Fatal("no tracked Markdown file was found; the check is not seeing the repository")
	}
	if rep.links == 0 {
		t.Fatal("no link was found in the tracked Markdown files; the check is not reading them")
	}
	if len(rep.problems) > 0 {
		t.Fatalf("%d broken link(s) in %d links:\n  %s", len(rep.problems), rep.links, strings.Join(rep.problems, "\n  "))
	}
}

// checkTree writes files under a temporary root, with "@" standing for "#" in
// link fragments so this file does not itself cite a page anchor, and checks
// them. Files named in untracked are written but not listed as tracked.
func checkTree(t *testing.T, files map[string]string, untracked ...string) linkReport {
	t.Helper()
	root := t.TempDir()
	var tracked []string
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(strings.ReplaceAll(body, "@", "#")), 0o600); err != nil {
			t.Fatal(err)
		}
		skip := false
		for _, u := range untracked {
			skip = skip || u == name
		}
		if !skip {
			tracked = append(tracked, name)
		}
	}
	rep, err := checkLinks(root, tracked)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// wantProblems fails unless the report holds exactly the wanted problems, in
// sorted order, with "@" standing for "#" as in checkTree.
func wantProblems(t *testing.T, rep linkReport, want ...string) {
	t.Helper()
	got, wanted := strings.Join(rep.problems, "\n  "), strings.ReplaceAll(strings.Join(want, "\n  "), "@", "#")
	if got != wanted {
		t.Errorf("problems:\n  %s\nwant:\n  %s", got, wanted)
	}
}

// TestLinkChecker proves the checker sees what it looks for, so a clean
// repository means clean and not a pattern that stopped matching.
func TestLinkChecker(t *testing.T) {
	const readme, guide = "README" + ".md", "docs/guide" + ".md"
	repoURL := "https://github.com/" + brand.Repository

	t.Run("links that resolve pass", func(t *testing.T) {
		rep := checkTree(t, map[string]string{
			readme: "@ Top\n\n## Setup & run\n\n## Dup\n\n## Dup\n\n## `code` heading ##\n\n" +
				"[a](docs/guide.md@intro) [b](@setup--run) [c](@dup) [d](@dup-1) [e](@code-heading)\n" +
				"[f](docs/) [g](docs/guide.md) [h](https://example.com/x@nope) [i](mailto:a@b.c)\n" +
				"[j](docs/guide.md@named) [k][ref]\n\n[ref]: docs/guide.md@intro \"title\"\n",
			guide:        "@ Intro\n\n<a id=\"named\"></a>\n\nBack to [the top](../README.md@top).\n",
			"main.go":    "// See " + readme + "@setup--run and " + guide + "@intro and " + repoURL + "@top.\n",
			"docs/x.txt": "plain\n",
		})
		wantProblems(t, rep)
		if rep.markdownFiles != 2 || rep.links != 14 {
			t.Errorf("saw %d Markdown files and %d links, want 2 and 14", rep.markdownFiles, rep.links)
		}
	})

	t.Run("a missing file is reported", func(t *testing.T) {
		rep := checkTree(t, map[string]string{readme: "@ Top\n\n[x](docs/gone.md)\n"})
		wantProblems(t, rep, "README.md:3: docs/gone.md: no such file")
	})

	t.Run("an untracked file is reported", func(t *testing.T) {
		rep := checkTree(t, map[string]string{readme: "[x](notes.md)\n", "notes.md": "@ Notes\n"}, "notes.md")
		wantProblems(t, rep, "README.md:1: notes.md: exists but is not tracked")
	})

	t.Run("a path outside the repository is reported", func(t *testing.T) {
		rep := checkTree(t, map[string]string{readme: "[x](../elsewhere.md)\n"})
		wantProblems(t, rep, "README.md:1: ../elsewhere.md: points outside the repository")
	})

	t.Run("a wrong anchor is reported", func(t *testing.T) {
		rep := checkTree(t, map[string]string{
			readme: "[x](docs/guide.md@nope) [y](@nowhere) [z][r]\n\n[r]: docs/guide.md@gone\n",
			guide:  "@ Intro\n",
		})
		wantProblems(t, rep,
			"README.md:1: @nowhere: no heading #nowhere in README.md",
			"README.md:1: docs/guide.md@nope: no heading #nope in docs/guide.md",
			"README.md:3: docs/guide.md@gone: no heading #gone in docs/guide.md",
		)
	})

	t.Run("a duplicate heading gets numbered anchors", func(t *testing.T) {
		rep := checkTree(t, map[string]string{
			readme: "@ Top\n\n## Dup\n\n## Dup\n\n[ok](@dup-1) [bad](@dup-2)\n",
		})
		wantProblems(t, rep, "README.md:7: @dup-2: no heading #dup-2 in README.md")
	})

	t.Run("links in code are ignored", func(t *testing.T) {
		rep := checkTree(t, map[string]string{
			readme: "@ Top\n\n```\n[a](missing.md)\n## Not a heading\n```\n\n~~~sh\n[b](missing.md)\n~~~\n\n" +
				"````\n```\n[c](missing.md)\n````\n\n" +
				"Inline `[d](missing.md)` and ``[e](`missing.md)`` and `x` [ok](@top).\n\n[f](@not-a-heading)\n",
		})
		wantProblems(t, rep, "README.md:19: @not-a-heading: no heading #not-a-heading in README.md")
		if rep.links != 2 {
			t.Errorf("saw %d links, want 2", rep.links)
		}
	})

	t.Run("a broken page anchor in a Go comment is reported", func(t *testing.T) {
		rep := checkTree(t, map[string]string{
			readme:      "@ Top\n",
			guide:       "@ Intro\n",
			"main.go":   "package main\n\n// See " + readme + "@gone, " + guide + "@gone and " + repoURL + "@gone.\n",
			"deep/y.go": "// " + guide + "@gone\n",
			"deep/x.go": "// ../" + readme + "@ignored and mydocs/guide.md@ignored\n",
			"bin.dat":   "\x00" + readme + "@gone",
		})
		wantProblems(t, rep,
			"deep/y.go:1: "+guide+"@gone: no heading #gone in docs/guide.md",
			"main.go:3: "+readme+"@gone: no heading #gone in README.md",
			"main.go:3: "+guide+"@gone: no heading #gone in docs/guide.md",
			"main.go:3: "+repoURL+"@gone: no heading #gone in README.md",
		)
	})

	t.Run("a Go comment naming a missing page is reported", func(t *testing.T) {
		rep := checkTree(t, map[string]string{readme: "@ Top\n", "main.go": "// docs/moved.md@x\n"})
		wantProblems(t, rep, "main.go:1: docs/moved.md@x: no such file")
	})

	t.Run("a parenthesised or angle-bracket target is read whole", func(t *testing.T) {
		rep := checkTree(t, map[string]string{
			readme: "[a](docs/we(ird).md) [b](<docs/my file.md@gone>) [c](docs/guide.md \"title\") [d](docs/guide.md 'it') [e](docs/guide.md (it))\n" +
				"[f](docs/we(ird).md@nope)\n",
			guide:             "@ Intro\n",
			"docs/we(ird).md": "@ Odd\n",
			"docs/my file.md": "@ Mine\n",
		})
		wantProblems(t, rep,
			"README.md:1: docs/my file.md@gone: no heading #gone in docs/my file.md",
			"README.md:2: docs/we(ird).md@nope: no heading #nope in docs/we(ird).md",
		)
	})

	t.Run("an undefined reference is reported", func(t *testing.T) {
		rep := checkTree(t, map[string]string{
			readme: "[ok][Ref One] [bad][nope] [bare] ![img][Ref One]\n\n[ref  one]: docs/guide.md\n",
			guide:  "@ Intro\n",
		})
		wantProblems(t, rep, "README.md:1: [nope]: no reference definition in this file")
	})

	t.Run("HTML links are checked", func(t *testing.T) {
		rep := checkTree(t, map[string]string{
			readme: "<a href=\"docs/guide.md@intro\">x</a> <img src='docs/pic.png'>\n" +
				"<a href=\"docs/guide.md@gone\">y</a> <img alt=\"z\" src=\"docs/missing.png\"> <a href=\"\">\n",
			guide:          "@ Intro\n",
			"docs/pic.png": "x",
		})
		wantProblems(t, rep,
			"README.md:2: docs/guide.md@gone: no heading #gone in docs/guide.md",
			"README.md:2: docs/missing.png: no such file",
		)
	})

	t.Run("a root-absolute path is resolved from the root", func(t *testing.T) {
		rep := checkTree(t, map[string]string{
			readme: "[a](/docs/guide.md@intro) [b](/docs/gone.md) [c](//example.com/x) [d](/docs/guide.md@gone)\n",
			guide:  "@ Intro\n",
		})
		wantProblems(t, rep,
			"README.md:1: /docs/gone.md: no such file",
			"README.md:1: /docs/guide.md@gone: no heading #gone in docs/guide.md",
		)
	})

	t.Run("a blob or tree URL of the repository is checked", func(t *testing.T) {
		rep := checkTree(t, map[string]string{
			readme: "[a](" + repoURL + "/blob/main/docs/guide.md@intro) [b](" + repoURL + "/blob/main/docs/guide.md@gone)\n" +
				"[c](" + repoURL + "/tree/main/docs) [d](" + repoURL + "/blob/main/docs/gone.md) [e](" + repoURL + ") [f](" + repoURL + "/issues/3)\n",
			guide: "@ Intro\n",
			"main.go": "// " + repoURL + "/blob/master/docs/guide.md@gone.\n" +
				"// " + repoURL + "/blob/v1.2.3/docs/guide.md@gone and " + repoURL + "/tree/dev/docs/gone.md\n",
		})
		wantProblems(t, rep,
			"README.md:1: "+repoURL+"/blob/main/docs/guide.md@gone: no heading #gone in docs/guide.md",
			"README.md:2: "+repoURL+"/blob/main/docs/gone.md: no such file",
			"main.go:1: "+repoURL+"/blob/master/docs/guide.md@gone: no heading #gone in docs/guide.md",
		)
	})

	t.Run("setext, quoted and HTML anchors count", func(t *testing.T) {
		rep := checkTree(t, map[string]string{
			readme: "Setext title\n============\n\nSecond `<T>` one\n---\n\n> ## Quoted heading\n\n" +
				"<a name=\"explicit\"></a>\n\n---\n\n" +
				"[a](@setext-title) [b](@second-t-one) [c](@quoted-heading) [d](@explicit) [e](@hr)\n",
		})
		wantProblems(t, rep, "README.md:13: @hr: no heading #hr in README.md")
	})

	t.Run("indented code and quoted fences are ignored", func(t *testing.T) {
		rep := checkTree(t, map[string]string{
			readme: "@ Top\n\n    [a](missing.md)\n    ## Not a heading\n\n> ```\n> [b](missing.md)\n> ```\n\n" +
				"- item\n\n    [c](gone.md)\n",
		})
		wantProblems(t, rep, "README.md:12: gone.md: no such file")
	})

	t.Run("index expressions in prose are not references", func(t *testing.T) {
		rep := checkTree(t, map[string]string{
			readme: "Use a[0][1] and x[i][j] here, but not [text][undefined-id].\n",
		})
		wantProblems(t, rep, "README.md:1: [undefined-id]: no reference definition in this file")
	})

	t.Run("a definition may have an angle-bracket target", func(t *testing.T) {
		rep := checkTree(t, map[string]string{
			readme:            "[a][ra] [b][rb]\n\n[ra]: <docs/my file.md>\n[rb]: <docs/gone file.md>\n",
			"docs/my file.md": "@ Mine\n",
		})
		wantProblems(t, rep, "README.md:4: docs/gone file.md: no such file")
	})

	t.Run("a tag or HTML line does not start a setext heading", func(t *testing.T) {
		rep := checkTree(t, map[string]string{
			readme: "[ref]: docs/guide.md\n---\n\n<b>Bold</b>\n---\n\n[a](@ref-docsguidemd) [b](@bold)\n",
			guide:  "@ Intro\n",
		})
		wantProblems(t, rep,
			"README.md:7: @bold: no heading #bold in README.md",
			"README.md:7: @ref-docsguidemd: no heading #ref-docsguidemd in README.md",
		)
	})

	t.Run("anchor matching is case-sensitive", func(t *testing.T) {
		rep := checkTree(t, map[string]string{readme: "@ Top\n\n[a](@top) [b](@Top)\n"})
		wantProblems(t, rep, "README.md:3: @Top: no heading #Top in README.md")
	})
}

// TestHeadingSlug pins the anchor GitHub gives each kind of heading text.
func TestHeadingSlug(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"Hello, World!", "hello-world"},
		{"`treerig up`", "treerig-up"},
		{"What it does not protect against", "what-it-does-not-protect-against"},
		{"A & B", "a--b"},
		{"[Link](x.md) text", "link-text"},
		{"Über café", "über-café"},
		{"snake_case and kebab-case", "snake_case-and-kebab-case"},
		{"Step 1: <b>bold</b>", "step-1-bold"},
		{"_Foo_ bar", "foo-bar"},
		{"`<T>` x", "t-x"},
		{"`<T>` type", "t-type"},
		{"**Bold** text", "bold-text"},
		{"½ cup", "½-cup"},
		{`Foo\_bar baz`, "foo_bar-baz"},
		{"_config file", "_config-file"},
		{"a &amp; b", "a--b"},
		{`Escaped \*star\* and \_x\_`, "escaped-star-and-_x_"},
		{"`a_b` and *c*", "a_b-and-c"},
	} {
		if got := headingSlug(tc.text); got != tc.want {
			t.Errorf("headingSlug(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}
