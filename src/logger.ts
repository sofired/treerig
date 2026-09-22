import { BRAND } from './brand.ts';
import { printable } from './text.ts';

export interface Logger {
  /** Progress and notices. Goes to stderr, so it never lands inside piped output. */
  info(message: string): void;
  warn(message: string): void;
  error(message: string): void;
  /** Primary command output, written without a prefix so it can be piped. */
  output(message: string): void;
}

/**
 * A logger that prefixes human-facing lines with the command name. The entry
 * point supplies the name it was invoked as; library code never reads argv.
 *
 * Everything the tool says about itself goes to stderr — progress, waiting
 * notices, warnings, failures — so `berth index > page.txt` and
 * `berth worktree list --json | jq` get the output and nothing else. Only
 * `output` writes to stdout, and only it is unfiltered — a caller passing a
 * borrowed value through it is the one responsible for running it through
 * `printable` first, because only the caller knows whether the line is prose,
 * a column in a table, or JSON that has to stay exact.
 *
 * Filtering is per line. Plenty of these messages are deliberately several
 * lines — a list of config problems, a command's captured stderr, a remedy to
 * copy — and the line breaks are this tool's own; what a subprocess or another
 * process's lock record put *inside* a line is what has to be neutralized.
 */
export function createConsoleLogger(commandName: string = BRAND.name): Logger {
  const prefix = `[${commandName}]`;
  // Indented after the first: borrowed text controls where its own line breaks
  // are, and an unindented continuation could read as a line this tool wrote.
  const say = (message: string): void => void console.error(
    message.split('\n')
      .map((line, index) => index === 0 || line === '' ? printable(line) : `  ${printable(line)}`)
      .join('\n')
  );
  return {
    info: (message) => say(`${prefix} ${message}`),
    warn: (message) => say(`${prefix} WARNING: ${message}`),
    error: (message) => say(`${prefix} ERROR: ${message}`),
    output: (message) => void console.log(message)
  };
}

/** A logger that discards everything; for tests, and for any caller that wants silence. */
export const silentLogger: Logger = {
  info() {},
  warn() {},
  error() {},
  output() {}
};
