package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// toml11Uses reports where src uses TOML 1.1 syntax that TOML 1.0 readers
// (Taplo, Python's tomllib) reject, one "line N: what" per finding: a line
// break or a trailing comma inside an inline table, and the \e and \xHH
// escapes. treerig's own parser, BurntSushi/toml, accepts TOML 1.1, so parsing
// an example with it cannot tell the two apart; this reads the source.
//
// It is a best-effort scanner, not a parser: it knows comments, the four
// kinds of string, and which of [ and { is innermost, and assumes src is
// otherwise valid. It does not catch every 1.1 addition: a time without
// seconds (07:32) is 1.1 and passes, as does anything else that is a matter
// of a value's spelling rather than of structure. It finds the mistakes an
// author is likely to make when writing an example, no more.
func toml11Uses(src string) []string {
	var found []string
	var stack []byte // open '[' and '{' brackets, innermost last
	line := 1
	lastComma := false // the last token outside comments and blanks was a comma
	note := func(what string) { found = append(found, fmt.Sprintf("line %d: %s", line, what)) }

	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\n':
			if n := len(stack); n > 0 && stack[n-1] == '{' {
				note("line break inside an inline table")
			}
			line++
		case c == ' ' || c == '\t' || c == '\r':
		case c == '#':
			for i+1 < len(src) && src[i+1] != '\n' {
				i++
			}
		case c == '"' || c == '\'':
			lastComma = false
			delim := string(c)
			if strings.HasPrefix(src[i:], strings.Repeat(delim, 3)) {
				delim = strings.Repeat(delim, 3)
			}
			i += len(delim)
			for i < len(src) && !strings.HasPrefix(src[i:], delim) {
				switch {
				case src[i] == '\n':
					line++
				case c == '"' && src[i] == '\\' && i+1 < len(src):
					if next := src[i+1]; next == 'e' || next == 'x' {
						note(`the \` + string(next) + ` escape`)
					}
					if src[i+1] == '\n' {
						line++
					}
					i++
				}
				i++
			}
			// A multi-line string may close with up to two extra quotes.
			i += len(delim) - 1
			for len(delim) == 3 && i+1 < len(src) && src[i+1] == c && !strings.HasPrefix(src[i+1:], delim) {
				i++
			}
		case c == '[' || c == '{':
			lastComma = false
			stack = append(stack, c)
		case c == ']' || c == '}':
			if c == '}' && lastComma {
				note("trailing comma in an inline table")
			}
			lastComma = false
			if n := len(stack); n > 0 {
				stack = stack[:n-1]
			}
		case c == ',':
			lastComma = true
		default:
			lastComma = false
		}
	}
	return found
}

// TestToml11Uses checks the scanner behind TestExamplesAreToml10.
func TestToml11Uses(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want []string
	}{
		{"one-line inline table", "a = { b = 1, c = [1,\n 2] }\n", nil},
		{"array of inline tables over several lines", "a = [\n  { b = 1 },\n  { b = 2 },\n]\n", nil},
		{"braces in strings and comments", "a = \"{\\\"\"\nb = '{'\n# {\nc = \"\"\"\n{\n\"\"\"\n", nil},
		{"multi-line inline table", "a = {\n  b = 1,\n}\n", []string{"line 1: line break inside an inline table", "line 2: line break inside an inline table", "line 3: trailing comma in an inline table"}},
		{"trailing comma", "a = { b = 1, }\n", []string{"line 1: trailing comma in an inline table"}},
		{"array trailing comma is fine", "a = [1, 2,]\n", nil},
		{"escapes", "a = \"\\e\\x1b\\n\"\nb = '\\e'\n", []string{`line 1: the \e escape`, `line 1: the \x escape`}},
		{"table headers", "[a.b]\n[[c]]\nd = 1\n", nil},
		{"multi-line string ending in quotes", "a = \"\"\"x\"\"\"\"\nb = { c = 1 }\n", nil},
		{"literal multi-line string ending in quotes", "a = '''x'''''\nb = { c = 1 }\n", nil},
		{"quoted key with a bracket", "\"a[b\" = 1\n'c{d' = { e = 2 }\n", nil},
		{"line break after a quoted key with a brace", "\"a{\" = { b = 1,\n c = 2 }\n", []string{"line 1: line break inside an inline table"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := toml11Uses(tc.src)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("toml11Uses = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExamplesAreToml10 holds treerig.example.toml and every toml block of the
// README and of docs/, at any depth, to TOML 1.0 syntax, as the README
// promises, so Taplo and Python's tomllib read them. A block must also parse,
// unless its fence says ```toml invalid: that is for a block that shows a
// mistake or elides part of a file, and it still has to keep to 1.0 syntax.
func TestExamplesAreToml10(t *testing.T) {
	root := filepath.Join("..", "..")
	example, err := os.ReadFile(filepath.Join(root, "treerig.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("treerig.example.toml", func(t *testing.T) {
		for _, use := range toml11Uses(string(example)) {
			t.Errorf("uses TOML 1.1 syntax: %s", use)
		}
		if _, err := DecodeOrderedTOML(string(example)); err != nil {
			t.Errorf("does not parse: %v", err)
		}
	})

	files := []string{filepath.Join(root, "README.md")}
	err = filepath.WalkDir(filepath.Join(root, "docs"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	blocks := 0
	for _, file := range files {
		contents, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, block := range tomlBlocks(string(contents)) {
			blocks++
			name := fmt.Sprintf("%s:%d", filepath.Base(file), block.line)
			t.Run(name, func(t *testing.T) {
				for _, use := range toml11Uses(block.text) {
					t.Errorf("uses TOML 1.1 syntax: %s", use)
				}
				if block.invalid {
					return
				}
				if _, err := DecodeOrderedTOML(block.text); err != nil {
					t.Errorf("does not parse: %v", err)
				}
			})
		}
	}
	if blocks == 0 {
		t.Error("found no ```toml block in the README or docs; the extraction is broken")
	}
}

// tomlBlock is one ```toml fence of a Markdown file.
type tomlBlock struct {
	line    int    // the line of the fence
	text    string // what is inside it
	invalid bool   // the fence is ```toml invalid: exempt from parsing
}

// tomlBlocks are the ```toml and ```toml invalid fences of a Markdown file,
// in order.
func tomlBlocks(markdown string) []tomlBlock {
	var blocks []tomlBlock
	var current *tomlBlock
	for n, line := range strings.Split(markdown, "\n") {
		switch {
		case current == nil && (strings.TrimSpace(line) == "```toml" || strings.TrimSpace(line) == "```toml invalid"):
			current = &tomlBlock{line: n + 1, invalid: strings.TrimSpace(line) == "```toml invalid"}
		case current != nil && strings.TrimSpace(line) == "```":
			blocks = append(blocks, *current)
			current = nil
		case current != nil:
			current.text += line + "\n"
		}
	}
	return blocks
}

// TestTomlBlocks checks the fence extraction, including the opt-out.
func TestTomlBlocks(t *testing.T) {
	md := "text\n```toml\na = 1\n```\n```bash\nx\n```\n```toml invalid\nb = = 2\n```\n"
	blocks := tomlBlocks(md)
	if len(blocks) != 2 || blocks[0].text != "a = 1\n" || blocks[0].invalid || blocks[1].text != "b = = 2\n" || !blocks[1].invalid || blocks[1].line != 8 {
		t.Errorf("tomlBlocks = %+v", blocks)
	}
}
