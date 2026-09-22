import { describe, expect, it } from 'vitest';
import { oneLine, printable, printableLines, safeJson } from '../../src/text.ts';

// One filter for everything the tool prints, and one writer for the JSON it
// prints: what a config file, a branch name or another process's record put in
// a string must not be able to drive the terminal reading it.

describe('printable', () => {
  it('replaces what a terminal or a reader would be fooled by', () => {
    expect(printable('a\u001b[2Kb')).toBe('a\uFFFD[2Kb');
    expect(printable('a\u202eb\u200bc\u00adx')).toBe('a\uFFFDb\uFFFDc\uFFFDx');
    expect(printable('a\u{E0041}b')).toBe('a\uFFFDb');
    // DEL, the 8-bit control range (U+009B is CSI with no ESC in front of it),
    // and the two separators no newline split can see.
    expect(printable('a\u007fb\u009bc\u0085d\u2028e\u2029f')).toBe('a\uFFFDb\uFFFDc\uFFFDd\uFFFDe\uFFFDf');
  });

  it('keeps whitespace as whitespace, and the joiners that carry meaning', () => {
    expect(printable('one\ntwo\tthree\r')).toBe('one two three ');
    expect(printable('\u{1F468}\u200D\u{1F4BB} m\u200Ci')).toBe('\u{1F468}\u200D\u{1F4BB} m\u200Ci');
  });
});

describe('printableLines', () => {
  it('keeps the line breaks it was given and nothing else that breaks a line', () => {
    expect(printableLines('a\nb\u001bc')).toBe('a\nb\uFFFDc');
    // Carriage return and the Unicode separators are not line breaks here.
    expect(printableLines('a\rb\u2028c')).toBe('a b\uFFFDc');
  });
});

describe('oneLine', () => {
  it('clamps to something a terminal can show, counting characters not units', () => {
    expect(oneLine('short')).toBe('short');
    // Exactly the limit is not too long.
    expect(oneLine('x'.repeat(200))).toBe('x'.repeat(200));
    expect(oneLine('x'.repeat(201))).toBe(`${'x'.repeat(199)}…`);
    expect(oneLine('x'.repeat(250))).toBe(`${'x'.repeat(199)}…`);
    // An astral character is one character, not two.
    expect([...oneLine('\u{1F468}'.repeat(250))]).toHaveLength(200);
  });

  it('filters before it clamps', () => {
    expect(oneLine('a\u001bb')).toBe('a\uFFFDb');
  });
});

describe('safeJson', () => {
  it('escapes everything a terminal could act on, and parses back to what it was', () => {
    const value = { branch: 'feature/\u202ex', note: 'a\u200bb\u007f\u009b', emoji: '\u{1F468}\u200D\u{1F4BB}' };
    const json = safeJson(value);

    // Nothing above ASCII survives, which is exactly the range safeJson escapes.
    expect(json).not.toMatch(/[\u007f-\uffff]/);
    expect(json).toContain('\\u202e');
    // DEL and the 8-bit control range are inside the escaped span too.
    expect(json).toContain('\\u007f');
    expect(json).toContain('\\u009b');
    expect(JSON.parse(json)).toEqual(value);
  });

  it('keeps an astral character whole across the escape', () => {
    const json = safeJson({ emoji: '\u{1F468}' });
    // A surrogate pair escapes as a pair, and comes back as one character.
    expect(json).toContain('\\ud83d\\udc68');
    expect((JSON.parse(json) as { emoji: string }).emoji).toBe('\u{1F468}');
  });

  it('answers for a value JSON has no text for', () => {
    expect(safeJson(undefined)).toBe('null');
  });

  it('indents the way a reader expects', () => {
    expect(safeJson({ a: 1 })).toBe('{\n  "a": 1\n}');
  });
});
