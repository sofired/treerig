// Oracle for internal/text's port of src/text.ts. Regenerate with:
//
//   export PATH=$HOME/.local/opt/node-v24.14.1-linux-x64/bin:$PATH
//   node tools/oracle/text-corpus.ts > internal/text/testdata/corpus.json
//
// internal/text reproduces three pieces of JavaScript string behaviour that
// Go has no built-in equivalent for: printable()'s \p{Cf} test (Go's
// unicode.Cf table and Node's ICU-backed \p{Cf} can, in principle, disagree
// about which Unicode version's Cf set applies), safeJson()'s escaping of
// every UTF-16 code unit from U+007F up, and toLowerCase()'s unconditional
// special casing (İ, U+0130, most visibly). Rather than trust that Go's and
// Node's Unicode tables agree, this walks every code point checked against
// the actual `printable` from src/text.ts, plus a set of hostile strings
// through `safeJson`, plus a lower-casing corpus through `.toLowerCase()`.
import { printable, safeJson } from '../../src/text.ts';

// --- printable: every C0 control, DEL, every C1 control, both paragraph
// separators, and every code point in the whole plane 0-0x10FFFF that
// Node's own \p{Cf} Unicode property escape matches (Format characters:
// soft hyphen, joiners, bidi controls, language tags, variation selectors,
// …), plus a few specific astral and multi-code-point cases the ported
// tests already care about.
const CF_PATTERN = /\p{Cf}/u;

interface CodePointCase {
  name: string;
  input: string;
  codePoint: number;
}

const codePointCases: CodePointCase[] = [];

function addCodePoint(name: string, codePoint: number): void {
  codePointCases.push({ name, input: String.fromCodePoint(codePoint), codePoint });
}

for (let cp = 0x00; cp <= 0x1f; cp += 1) addCodePoint(`C0 control U+${cp.toString(16).padStart(4, '0')}`, cp);
addCodePoint('DEL U+007f', 0x7f);
for (let cp = 0x80; cp <= 0x9f; cp += 1) addCodePoint(`C1 control U+${cp.toString(16).padStart(4, '0')}`, cp);
addCodePoint('LINE SEPARATOR U+2028', 0x2028);
addCodePoint('PARAGRAPH SEPARATOR U+2029', 0x2029);

// Every Cf code point in the Unicode range, as Node's own regex engine sees
// it — this is the ground truth internal/text.Printable's unicode.Cf check
// is verified against, not a hand-picked sample.
let cfCount = 0;
for (let cp = 0x0; cp <= 0x10ffff; cp += 1) {
  if (cp >= 0xd800 && cp <= 0xdfff) continue; // surrogate halves are not code points on their own
  const ch = String.fromCodePoint(cp);
  if (CF_PATTERN.test(ch)) {
    addCodePoint(`Cf U+${cp.toString(16).padStart(4, '0')}`, cp);
    cfCount += 1;
  }
}

// The two joiners printable() deliberately keeps despite being Cf.
addCodePoint('ZERO WIDTH NON-JOINER U+200c (kept)', 0x200c);
addCodePoint('ZERO WIDTH JOINER U+200d (kept)', 0x200d);

const printableResults = codePointCases.map((c) => ({
  name: c.name,
  input: c.input,
  want: printable(c.input)
}));

// A few composite strings beyond single code points: an astral character
// alongside ASCII, an emoji ZWJ sequence, mixed control and joiner.
interface StringCase {
  name: string;
  input: string;
}

const stringCases: StringCase[] = [
  { name: 'astral character amid ASCII', input: 'a\u{1F468}b' },
  { name: 'emoji ZWJ sequence kept whole', input: '\u{1F468}‍\u{1F4BB}' },
  { name: 'CSI without ESC (U+009B) amid text', input: 'a\u009bb' },
  { name: 'RTL override reversing a line', input: 'a‮b' },
  { name: 'soft hyphen amid text', input: 'a­b' },
  { name: 'tab, newline, CR mixed with text', input: 'one\ntwo\tthree\r' },
  { name: 'language tag character (astral Cf)', input: 'a\u{E0041}b' }
];

const printableStringResults = stringCases.map((c) => ({
  name: c.name,
  input: c.input,
  want: printable(c.input)
}));

// --- safeJson: hostile strings a config, branch name or process record
// could actually contain.
interface JSONCase {
  name: string;
  value: unknown;
}

const jsonCases: JSONCase[] = [
  { name: 'astral character', value: { emoji: '\u{1F468}' } },
  { name: 'astral character alone (surrogate pair)', value: '\u{1F468}' },
  { name: 'U+007F DEL', value: '\u007f' },
  { name: 'U+009B CSI', value: '\u009b' },
  { name: 'U+00FF Latin-1 supplement boundary', value: 'ÿ' },
  { name: 'U+FFFF top of BMP', value: '￿' },
  { name: 'angle brackets and ampersand', value: '<script>&amp;</script>' },
  { name: 'RTL override plus ZWSP plus soft hyphen', value: 'feature/‮x​note­' },
  { name: 'control characters', value: 'a\u0000b\u0007c\u001bd' },
  { name: 'empty array', value: [] },
  { name: 'empty object', value: {} },
  { name: 'nested empty array and object', value: { list: [], obj: {} } },
  { name: 'undefined', value: undefined },
  { name: 'plain ASCII object indents', value: { a: 1 } },
  // Key order here is alphabetical on purpose: the Go side decodes this
  // fixture into a map, which Go's own encoding/json re-serializes with keys
  // sorted, so the fixture's insertion order has to already agree with that
  // or the comparison would fail for a reason that has nothing to do with
  // safeJson's own escaping.
  { name: 'mixed emoji, note, control in one object', value: { branch: 'feature/‮x', emoji: '\u{1F468}‍\u{1F4BB}', note: 'a​b\u007f\u009b' } }
];

const safeJsonResults = jsonCases.map((c) => ({
  name: c.name,
  // JSON-encode the fixture itself for the Go side to decode back into
  // an any (undefined becomes the JSON string "undefined", handled
  // specially in the Go test rather than round-tripped through JSON).
  valueJSON: c.value === undefined ? null : JSON.stringify(c.value),
  isUndefined: c.value === undefined,
  want: safeJson(c.value)
}));

// --- toLowerCase: a corpus broad enough to catch a divergence between
// Node's ICU-backed String.prototype.toLowerCase and Go's unicode.ToLower,
// centered on the case JSLower exists for (U+0130) plus enough of the
// Unicode casing table that a silent gap would show up.
const lowerCases: { name: string; input: string }[] = [
  { name: 'ASCII upper and lower mixed', input: 'Hello, World! 123' },
  { name: 'Turkish dotted capital I (U+0130)', input: 'İ' },
  { name: 'Turkish dotted capital I amid text', input: 'feature/İssue-42' },
  { name: 'Turkish dotless lowercase i (U+0131) unaffected', input: 'ıstanbul' },
  { name: 'German sharp s (already lower-case)', input: 'straße' },
  { name: 'Latin-1 supplement uppercase', input: 'ÀÉÎÕÜ' },
  { name: 'Latin Extended-A uppercase', input: 'ĀĒĪŌŪ' },
  { name: 'Cyrillic uppercase', input: 'АБВГД' },
  { name: 'Greek uppercase (non-final)', input: 'ΑΒΓΔΕ' },
  { name: 'Kelvin sign (U+212A) — canonically equivalent to K', input: 'K' },
  { name: 'Angstrom sign (U+212B)', input: 'Å' },
  { name: 'CJK is case-invariant', input: '日本語' },
  { name: 'emoji is case-invariant', input: '\u{1F468}' },
  { name: 'repeated U+0130', input: 'İİİ' }
];

const lowerResults = lowerCases.map((c) => ({
  name: c.name,
  input: c.input,
  want: c.input.toLowerCase()
}));

process.stdout.write(JSON.stringify({
  cfCodePointCount: cfCount,
  printableCodePoints: printableResults,
  printableStrings: printableStringResults,
  safeJson: safeJsonResults,
  lower: lowerResults
}, null, 2) + '\n');
