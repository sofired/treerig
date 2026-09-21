import { BRAND } from './brand.ts';

export interface Logger {
  info(message: string): void;
  warn(message: string): void;
  error(message: string): void;
  /** Primary command output, written without a prefix so it can be piped. */
  output(message: string): void;
}

/**
 * A logger that prefixes human-facing lines with the command name. The entry
 * point supplies the name it was invoked as; library code never reads argv.
 */
export function createConsoleLogger(commandName: string = BRAND.name): Logger {
  const prefix = `[${commandName}]`;
  return {
    info: (message) => console.log(`${prefix} ${message}`),
    warn: (message) => console.error(`${prefix} WARNING: ${message}`),
    error: (message) => console.error(`${prefix} ERROR: ${message}`),
    output: (message) => console.log(message)
  };
}

/** A logger that discards everything; for tests and for `--quiet`. */
export const silentLogger: Logger = {
  info() {},
  warn() {},
  error() {},
  output() {}
};
