import { stripVTControlCharacters } from 'node:util';
import { describe, expect, it } from 'vitest';
import { describePlan } from '../../src/commands/remove.ts';
import { confirm, confirmationMatches, confirmationPhrase, confirmationQuestion } from '../../src/prompts.ts';

describe('typed confirmation prompt', () => {
  it('sets the required phrase apart from the explanation and input cursor', () => {
    const question = stripVTControlCharacters(confirmationQuestion(
      'This removes the environment and its data.',
      'remove feature-login'
    ));

    expect(question).toBe(
      'This removes the environment and its data.\n\n' +
      'Type this exact phrase to continue:\n\n' +
      '  remove feature-login\n\n' +
      '> '
    );
  });

  it('keeps a multi-line plan multi-line, and filters inside each line', () => {
    // The plan is an itemised list, and this is the screen a reader checks
    // before something is destroyed: it must not become one run-on sentence,
    // and a registry record must not be able to rewrite it.
    const question = stripVTControlCharacters(confirmationQuestion(
      'This will permanently remove x.localhost:\n  2 containers\n  its \u001b[2Kregistration',
      'x.localhost'
    ));

    expect(question.split('\n').slice(0, 3)).toEqual([
      'This will permanently remove x.localhost:',
      '  2 containers',
      '  its \uFFFD[2Kregistration'
    ]);
  });

  it('will not let a hostname write its own line into the plan', () => {
    // A registry record's domain is not hostname-validated, and this list is
    // what a reader checks before something is permanently removed.
    const question = stripVTControlCharacters(confirmationQuestion(
      describePlan('api.acme\nnothing will be removed from this machine', [], true),
      'api.acme'
    ));

    // Folded into the line it was borrowed into, not a line of its own.
    const lines = question.split('\n');
    expect(lines[0]).toBe('This will permanently remove api.acme nothing will be removed from this machine:');
    expect(lines.filter((line) => line.startsWith('This will'))).toHaveLength(1);
  });

  it('asks for the phrase it showed, so an odd hostname is still removable', () => {
    const shown = stripVTControlCharacters(confirmationQuestion('plan', 'api.acme\u001b[2K'));
    expect(shown).toContain('api.acme\uFFFD[2K');
    // What is displayed is what matches; the raw form is unreachable by typing.
    expect(confirmationPhrase('api.acme\u001b[2K')).toBe('api.acme\uFFFD[2K');
    expect(confirmationMatches('api.acme\u001b[2K', 'api.acme\uFFFD[2K')).toBe(true);
    expect(confirmationMatches('api.acme\u001b[2K', 'api.acme\u001b[2K')).toBe(false);
  });

  it('refuses without prompting when the session is non-interactive', async () => {
    await expect(confirm({ nonInteractive: true }, 'Remove?', 'remove it'))
      .rejects.toThrow('Interactive confirmation required: "remove it".');
  });

  it('skips the prompt entirely when the caller has already approved', async () => {
    await expect(confirm({ nonInteractive: true }, 'Remove?', 'remove it', true))
      .resolves.toBeUndefined();
  });
});
