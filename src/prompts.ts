import { createInterface } from 'node:readline/promises';
import { stdin, stderr } from 'node:process';
import { styleText } from 'node:util';
import { CliError } from './errors.ts';
import { printable, printableLines } from './text.ts';

export interface ConfirmationContext {
  nonInteractive: boolean;
}

/**
 * Both halves are filtered: the hostname in them comes from a registry record,
 * and this is the one screen a reader checks before something is destroyed.
 * Per line, because the plan is an itemised list — a caller that interpolates
 * a borrowed value into a line filters it there, or the value can add lines of
 * its own to the list of what is about to go.
 */
export function confirmationQuestion(prompt: string, expected: string): string {
  const phrase = styleText(['bold', 'cyan'], confirmationPhrase(expected), { stream: stderr });
  const input = styleText('bold', '> ', { stream: stderr });
  return `${printableLines(prompt)}\n\nType this exact phrase to continue:\n\n  ${phrase}\n\n${input}`;
}

/**
 * What the reader is shown, and therefore what they have to type. A phrase
 * with something unprintable in it is displayed filtered, so the filtered form
 * is the one that can be matched — asking for the raw one would leave no way
 * through but `--yes`, which skips the plan as well as the phrase.
 */
export function confirmationPhrase(expected: string): string {
  return printable(expected);
}

/** Whether what was typed is what was asked for. */
export function confirmationMatches(expected: string, answer: string): boolean {
  return answer === confirmationPhrase(expected);
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
    throw new CliError(`Interactive confirmation required: "${confirmationPhrase(expected)}".`);
  }
  const reader = createInterface({ input: stdin, output: stderr });
  try {
    const answer = await reader.question(confirmationQuestion(prompt, expected));
    if (!confirmationMatches(expected, answer)) {
      throw new CliError('Confirmation did not match; operation cancelled.');
    }
  } finally {
    reader.close();
  }
}
