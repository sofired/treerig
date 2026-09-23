// Oracle for internal/text's collation helper, `text.Collate`, which every
// checkout-index sort (WP4a, #40) needs: the TypeScript build sorts with
// `localeCompare(…, 'en')`, which is ICU under Node's hood, and Go has no
// built-in collator. This is the differential corpus that decides whether
// `golang.org/x/text/collate` can stand in for it. Regenerate with:
//
//   export PATH=$HOME/.local/opt/node-v24.14.1-linux-x64/bin:$PATH
//   node tools/oracle/collation-corpus.ts > internal/text/testdata/collation-corpus.json
//
// Node 24 bundles ICU 78 / CLDR 48; golang.org/x/text/collate's tables are
// generated from CLDR 23 — over a decade older. This corpus is what proves,
// or disproves, that the gap matters for the strings berth actually sorts.
//
// Every case is tagged `realistic` or `wide`. `realistic` is everything a
// Git branch, a config's project name, or a domain label plausibly is: plain
// ASCII, case pairs, punctuation and hyphens against letters, digit runs,
// Latin-1 and accented letters (precomposed and decomposed), and shared
// prefixes. `wide` is what the port's brief also asks this corpus to cover —
// CJK, Hangul, Kana, emoji, and an exhaustive sweep of every combining
// diacritical mark — so a version-tailoring gap there is caught even though
// it does not gate the port: berth's own fixtures never index a Han project
// name or an emoji branch, and the WP4a brief's stop condition is scoped to
// "ASCII and Latin letters, digits, punctuation in names".
//
// `sorted` is each category, independently, in Node's own
// `localeCompare(…, 'en')` order — a stable sort, so two strings the
// collator calls equal keep their input order, exactly as V8's
// Array.prototype.sort and Go's slices.SortStableFunc both promise. `pairs`
// is a curated, deterministic sample (no Math.random, so this file
// reproduces byte-for-byte) of pairwise comparisons, tagged the same way.
//
// The unsorted corpus itself is not recorded separately: it is the same
// multiset as `sorted`, just reordered, so shipping both would double this
// file for no extra signal. The Go test reverses `sorted` (a deterministic
// permutation) before sorting it again with Collate.

type Category = 'realistic' | 'wide';
interface Case { value: string; category: Category }

const cases: Case[] = [];
function add(category: Category, ...values: string[]): void {
  for (const value of values) cases.push({ value, category });
}

// --- empty and near-empty ---
add('realistic', '', ' ', '-', '.', '_', 'a', 'A');

// --- case pairs: ICU's default tertiary weight sorts lower case before
// upper case for two strings that are otherwise identical. ---
add('realistic',
  'apple', 'Apple', 'APPLE', 'aPPLE',
  'main', 'Main', 'MAIN',
  'feature', 'Feature', 'FEATURE',
  'widget', 'Widget', 'widgetA', 'WidgetA',
  'iphone', 'iPhone', 'IPhone', 'IPHONE'
);

// --- punctuation and hyphens against plain letters: ICU's default
// collation treats most punctuation as a low-weight "variable" that is
// ignored at the primary level, so 'a-b' and 'ab' are primary-equal and
// fall apart only at a later level. ---
add('realistic',
  'a-b', 'ab', 'a b', 'a.b', 'a_b', 'a/b', 'a:b', "a'b", 'a,b',
  'coop', 'co-op', 'co op', "co'op",
  'preexisting', 'pre-existing', 'pre existing',
  'nonblocking', 'non-blocking',
  '--', '-a', 'a-', '-a-', 'a--b', 'a-b-c', 'a-bc', 'ab-c',
  'x', 'x-', 'x-1', 'x1', 'x.1', 'x_1'
);

// --- digits and digit runs: ICU's default collation does NOT do numeric
// sorting (that needs the numeric collation option, which localeCompare's
// 'en' locale with no options does not turn on), so '9' sorts before '10'
// only because '1' < '9' as characters, and '10' sorting before '9' would be
// wrong to expect. ---
add('realistic',
  '0', '00', '000', '0-0', '00-0', '0.0',
  '1', '2', '9', '10', '11', '19', '20', '99', '100', '101', '199', '200',
  'v1', 'v2', 'v9', 'v10', 'v11', 'v99', 'v100',
  'release-1', 'release-2', 'release-9', 'release-10', 'release-11',
  'release-1.0', 'release-1.1', 'release-1.10', 'release-1.2', 'release-1.9',
  'feature-1', 'feature-2', 'feature-10', 'feature-a1', 'feature-a10', 'feature-a2',
  'a1', 'a2', 'a9', 'a10', 'a11', 'a100', 'a1a', 'a1b'
);

// --- shared prefixes ---
add('realistic',
  'feature', 'feature-a', 'feature-ab', 'feature-abc', 'feature-b',
  'feature/', 'feature/a', 'feature/a/b', 'feature/ab',
  'feat', 'features', 'featured',
  'test', 'testing', 'tests', 'test-a', 'test-1'
);

// --- Latin-1 and accented letters: ICU's default tailoring treats an
// accented letter as a tertiary variant of its base letter (a primary tie),
// which sorts it right next to the base rather than after every plain
// ASCII letter the way a codepoint (or UTF-16) comparison would. ---
add('realistic',
  'cafe', 'café', 'cafes', 'cafés',
  'resume', 'résumé', 'resumes',
  'naive', 'naïve',
  'uber', 'über', 'Uber', 'Über',
  'nino', 'niño', 'nina', 'niña',
  'facade', 'façade',
  'straße', 'strasse', 'Straße', 'STRASSE',
  'apfel', 'äpfel', 'Apfel', 'Äpfel',
  'oresund', 'öresund', 'Öresund',
  'angstrom', 'ångström', 'Ångström',
  'aaron', 'åaron',
  'a', 'á', 'à', 'â', 'ä', 'ã', 'å', 'æ', 'A', 'Á',
  'e', 'é', 'è', 'ê', 'ë', 'E', 'É',
  'i', 'í', 'ì', 'î', 'ï',
  'o', 'ó', 'ò', 'ô', 'ö', 'õ', 'ø', 'O', 'Ø',
  'u', 'ú', 'ù', 'û', 'ü', 'U', 'Ü',
  'n', 'ñ', 'N', 'Ñ',
  'c', 'ç', 'C', 'Ç',
  'ss', 'ß'
);

// --- combining marks: a precomposed letter and its decomposed form
// (base letter + combining mark) collate identically under ICU, since
// collation normalizes first — this is the main place a byte- or
// codepoint-level comparison would visibly disagree with localeCompare.
// These specific, realistic accents stay in the `realistic` category; the
// exhaustive sweep of every combining mark below is `wide`. ---
add('realistic',
  'é', // é as e + combining acute accent, vs the precomposed 'é' above
  'café', // café decomposed
  'à', // à decomposed
  'ñ', // ñ decomposed
  'ü', // ü decomposed
  'ö', // ö decomposed
  'é́' // two combining marks stacked (not a real letter, but legal Unicode)
);

// --- CJK: ICU's default 'en' collation for unified Han sorts by a stroke-
// or radical-derived weight (an implicit collation for unassigned/Han
// primary weights), never by raw codepoint order, and mixed scripts sort
// by script boundary. Han's implicit weighting was revised more than once
// between CLDR 23 and CLDR 48. ---
add('wide',
  '中文', '中国', '日本語', '日本', '한국어', '한국',
  '你好', '世界', '北京', '上海', '深圳',
  'アニメ', 'カタカナ', 'ひらがな',
  '项目', '项目-1', '项目-a', // "project" plus ASCII suffixes, mixed-script realism
  'main-中文', '中文-main'
);

// --- emoji: astral code points, several with combining ZWJ sequences and
// variation selectors; most were assigned to Unicode after CLDR 23 (2013),
// so x/text/collate's implicit fallback for them need not agree with ICU's
// current explicit weights. ---
add('wide',
  '🚀', '🎉', '😀', '😀😀', '👨‍💻', '👍', '👍🏽', '🏳️‍🌈', '🔥', '💯',
  'feature-🚀', '🚀-feature', 'feature-😀'
);

// --- realistic branch names ---
add('realistic',
  'main', 'master', 'develop', 'dev', 'staging', 'production', 'prod',
  'feature/login', 'feature/login-page', 'feature/user-auth',
  'feature/JIRA-123-fix-login', 'feature/JIRA-1234-fix-login',
  'bugfix/null-pointer', 'hotfix/urgent-security-patch',
  'release/1.0.0', 'release/1.0.1', 'release/1.10.0', 'release/2.0.0',
  'release/v1.0.0', 'release/v1.10.0', 'release/v2.0.0',
  'chore/update-deps', 'chore/update-dependencies',
  'refactor/api-client', 'test/integration-suite',
  'renovate/eslint-9.x', 'renovate/react-18.x', 'dependabot/npm_and_yarn/lodash-4.17.21',
  'user/scott/quick-fix', 'user/scott.escue/feature',
  'feature/über-cool-feature', 'feature/café-menu',
  'wip', 'WIP', 'temp', 'tmp', 'backup', 'old', 'old-2'
);

// --- realistic project names ---
add('realistic',
  'acme', 'acme-widgets', 'acme_widgets', 'Acme Widgets', 'ACME',
  'widgets', 'widgets-api', 'widgets-ui', 'widgets2', 'widgets-v2',
  'my-app', 'myapp', 'my_app', 'MyApp',
  'api-gateway', 'api_gateway', 'API Gateway',
  'super-cool-project-9000', 'super-cool-project-10000'
);

// --- realistic domain labels: SLUG and HOSTNAME are restricted to
// [a-z0-9.-], but the project name shown beside the domain heading is not,
// and the checkout index sorts both. ---
add('realistic',
  'acme.localhost', 'widgets.localhost', 'a.localhost', 'ab.localhost',
  'feature-login.acme.localhost', 'feature-login-1d407e139915.acme.localhost',
  '9-a.localhost', 'a-9.localhost', '0.localhost', '00.localhost',
  'my-app.example.com', 'my-app.staging.example.com', 'my-app.prod.example.com'
);

// --- hostile / control-adjacent, since the index prints whatever a
// branch or project name happens to contain ---
add('realistic', 'a\tb', 'a\nb', 'a​b', 'a­b', 'a‮b');

// The hand-written cases above cover every category the checkout index's
// sort actually exercises; the loops below multiply that coverage into the
// "several thousand strings" this differential corpus is meant to be,
// mechanically rather than by typing each one, so a version-tailoring
// disagreement buried in a numeric range or a wide codepoint sweep is not
// missed for lack of volume.

// Digit runs at every length that matters for "does it sort numerically or
// lexically": 0-999 plain, and the same range behind realistic prefixes.
for (let i = 0; i <= 999; i += 1) {
  add('realistic',
    String(i), `v${i}`, `release-${i}`, `release-1.${i}`,
    `feature-${i}`, `item${i}`, `a${i}b`
  );
}

// Every Latin-1 Supplement letter (accented and otherwise), upper and lower,
// alone and behind a shared ASCII prefix/suffix, so an ICU tailoring that
// treats accents differently from Node's shows up somewhere in this sweep.
for (let cp = 0xc0; cp <= 0xff; cp += 1) {
  if (cp === 0xd7 || cp === 0xf7) continue; // multiplication/division signs, not letters
  const ch = String.fromCodePoint(cp);
  add('realistic', ch, `a${ch}`, `${ch}a`, `feature-${ch}`, `caf${ch}`);
}

// An exhaustive sweep of every combining diacritical mark (not just the
// handful of realistic accents above) applied to a few base letters: `wide`,
// since stacking an IPA tone mark or a Vietnamese-orthography hook on a bare
// letter is not a real branch or project name, but the corpus should still
// say whether the two libraries agree on it.
for (let cp = 0x0300; cp <= 0x036f; cp += 1) {
  const mark = String.fromCodePoint(cp);
  for (const base of ['a', 'e', 'i', 'o', 'u', 'n', 'c']) {
    add('wide', `${base}${mark}`);
  }
}

// CJK Unified Ideographs: a stride through the block (not every one of its
// tens of thousands of code points, which would dwarf everything else here
// for no extra signal), alone and mixed with ASCII the way a project name
// mixing scripts would.
for (let cp = 0x4e00; cp <= 0x9fff; cp += 61) {
  const ch = String.fromCodePoint(cp);
  add('wide', ch, `${ch}${ch}`, `main-${ch}`, `${ch}-main`);
}

// Hiragana, Katakana, and Hangul Syllables: the other scripts a project or
// branch name could carry, same stride treatment.
for (const [start, end] of [[0x3040, 0x309f], [0x30a0, 0x30ff], [0xac00, 0xd7a3]] as const) {
  for (let cp = start; cp <= end; cp += 47) {
    const ch = String.fromCodePoint(cp);
    add('wide', ch, `${ch}${ch}`);
  }
}

// Emoji: a stride through the main emoji block plus a few multi-codepoint
// sequences (skin-tone modifier, ZWJ, variation selector), alone and behind
// an ASCII affix.
for (let cp = 0x1f300; cp <= 0x1f9ff; cp += 23) {
  const ch = String.fromCodePoint(cp);
  add('wide', ch, `feature-${ch}`, `${ch}-feature`);
}

// Shared-prefix runs at increasing lengths, ASCII and accented, since a
// primary-level tie (ICU ignores case and accents at that level) is broken
// by whichever letter differs first, however far into the string that is.
for (let n = 1; n <= 40; n += 1) {
  add('realistic', `feature-${'a'.repeat(n)}`, `feature-${'a'.repeat(n)}b`, `feature-${'A'.repeat(n)}`);
}

function dedupe(values: string[]): string[] {
  return [...new Set(values)];
}

const byCategory = (category: Category): string[] => dedupe(cases.filter((c) => c.category === category).map((c) => c.value));

const realistic = byCategory('realistic');
const wide = byCategory('wide');

interface SortedEntry {
  value: string;
  /**
   * True when this element and the next collate exactly equal under
   * localeCompare('en') — a precomposed letter against its decomposed form,
   * say. The Go test permutes this array before re-sorting it, and a stable
   * sort only promises ties keep whatever order they arrive in, so it needs
   * Node's own tie boundaries (not a boundary it infers from Collate itself,
   * which would beg the question if Collate ever ties two strings
   * differently than localeCompare does) to know which positions may
   * legitimately swap.
   */
  tiedWithNext: boolean;
}

function sortedOf(values: string[]): SortedEntry[] {
  const sorted = [...values].sort((a, b) => a.localeCompare(b, 'en'));
  return sorted.map((value, i) => ({
    value,
    tiedWithNext: i + 1 < sorted.length && value.localeCompare(sorted[i + 1] ?? '', 'en') === 0
  }));
}

function pairsOf(values: string[]): { left: string; right: string; sign: number }[] {
  // Consecutive elements in declared order, so the sample walks across every
  // category boundary as well as within it, plus an all-pairs cross-check
  // over a small set of probes representative of the category.
  const consecutive = values.slice(1).map((value, index) => [values[index] ?? '', value] as const);
  return consecutive.map(([left, right]) => ({ left, right, sign: Math.sign(left.localeCompare(right, 'en')) }));
}

const probes = ['a', 'A', 'á', 'Á', '1', '9', '10', '-', '_', '.', ' ', ''];
const probePairs: { left: string; right: string; sign: number }[] = [];
for (const left of probes) {
  for (const right of probes) {
    probePairs.push({ left, right, sign: Math.sign(left.localeCompare(right, 'en')) });
  }
}

process.stdout.write(JSON.stringify({
  icu: process.versions.icu,
  cldr: process.versions.cldr,
  unicode: process.versions.unicode,
  realistic: { sorted: sortedOf(realistic), pairs: [...pairsOf(realistic), ...probePairs] },
  wide: { sorted: sortedOf(wide), pairs: pairsOf(wide) }
}, null, 2) + '\n');
