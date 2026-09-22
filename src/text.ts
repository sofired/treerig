// What a config file, a branch name, another process's holder record, or a
// subprocess's stderr put in a string, made safe to print. Everything this tool
// composes for a human goes through here — `logger.output` is the caller's own
// responsibility, and a hook's or Compose's output is inherited rather than
// composed, so it reaches the terminal as those programs wrote it. Escape
// sequences can drive a terminal's cursor and
// overwrite a line that said `Stopped` with one that says `Running`, a
// right-to-left override reverses the rest of a line, and a zero-width or soft
// hyphen makes two different checkouts look identical.

/**
 * Whitespace becomes a space rather than a replacement character: messages
 * from Git and Docker are routinely multi-line, and a line break is a place to
 * fold, not something to hide. The zero-width joiners are kept — they carry
 * meaning in ordinary text, holding an emoji together and standing in for
 * required orthography in Persian and several Indic scripts.
 *
 * Deliberately narrower than `\p{C}`: private-use, unassigned and lone
 * surrogate code points are left alone. None of them can drive a terminal, and
 * a filter that replaced them would mangle text that is merely unusual.
 */
export function printable(value: string): string {
  return [...value].map((character) => {
    const code = character.codePointAt(0) ?? 0;
    if (code === 0x09 || code === 0x0a || code === 0x0d) return ' ';
    const control = code < 0x20 || code === 0x7f || (code >= 0x80 && code <= 0x9f) ||
      code === 0x2028 || code === 0x2029;
    const invisible = /\p{Cf}/u.test(character) && character !== '\u200C' && character !== '\u200D';
    return control || invisible ? '\uFFFD' : character;
  }).join('');
}

/** `printable` per line, so the caller's own line breaks survive it. */
export function printableLines(value: string): string {
  return value.split('\n').map(printable).join('\n');
}

/**
 * The same, clamped to 200 code points — which is a terminal line for text
 * that is one column per character, and fewer for text that is not.
 */
export function oneLine(value: string, limit = 200): string {
  const printableValue = printable(value);
  return [...printableValue].length <= limit
    ? printableValue
    : `${[...printableValue].slice(0, limit - 1).join('')}…`;
}

/**
 * JSON whose every non-ASCII code unit is escaped. The values stay exactly
 * what they were — `JSON.parse` gives them back byte for byte — and nothing a
 * branch name or a service label carries can drive the terminal printing it.
 */
export function safeJson(value: unknown): string {
  // `JSON.stringify` answers `undefined` for a value with no JSON form; the
  // caller asked for text, so it gets the text JSON uses for absence.
  return (JSON.stringify(value, null, 2) ?? 'null').replaceAll(
    /[\u007f-\uffff]/g,
    (character) => `\\u${character.charCodeAt(0).toString(16).padStart(4, '0')}`
  );
}
