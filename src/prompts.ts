import { createInterface } from 'node:readline/promises';
import { stdin, stderr } from 'node:process';
import { styleText } from 'node:util';
import { CliError } from './errors.ts';

export interface ConfirmationContext {
  nonInteractive: boolean;
}

export function confirmationQuestion(prompt: string, expected: string): string {
  const phrase = styleText(['bold', 'cyan'], expected, { stream: stderr });
  const input = styleText('bold', '> ', { stream: stderr });
  return `${prompt}\n\nType this exact phrase to continue:\n\n  ${phrase}\n\n${input}`;
}

/**
 * Requires the user to type an exact phrase. Destructive operations use this
 * so that a stray `y` cannot approve them; `approved` is the escape hatch for
 * an explicit CLI flag.
 */
export async function confirm(
  context: ConfirmationContext,
  prompt: string,
  expected: string,
  approved = false
): Promise<void> {
  if (approved) return;
  if (context.nonInteractive || !stdin.isTTY) {
    throw new CliError(`Interactive confirmation required: "${expected}".`);
  }
  const reader = createInterface({ input: stdin, output: stderr });
  try {
    const answer = await reader.question(confirmationQuestion(prompt, expected));
    if (answer !== expected) throw new CliError('Confirmation did not match; operation cancelled.');
  } finally {
    reader.close();
  }
}
