import { stripVTControlCharacters } from 'node:util';
import { describe, expect, it } from 'vitest';
import { confirm, confirmationQuestion } from '../../src/prompts.ts';

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

  it('refuses without prompting when the session is non-interactive', async () => {
    await expect(confirm({ nonInteractive: true }, 'Remove?', 'remove it'))
      .rejects.toThrow('Interactive confirmation required: "remove it".');
  });

  it('skips the prompt entirely when the caller has already approved', async () => {
    await expect(confirm({ nonInteractive: true }, 'Remove?', 'remove it', true))
      .resolves.toBeUndefined();
  });
});
