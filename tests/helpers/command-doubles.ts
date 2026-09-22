import { EventEmitter } from 'node:events';
import type { ProcessControl } from '../../src/commands/context.ts';
import type { Logger } from '../../src/logger.ts';

/**
 * Captures every line; `printed` is what a user would have seen on stdout.
 * Recorded as it is written rather than classified by prefix afterwards, so a
 * forwarded line that happens to start with `error: ` is still output.
 */
export class RecordingLogger implements Logger {
  readonly lines: string[] = [];
  readonly printed: string[] = [];
  info(message: string): void { this.lines.push(`info: ${message}`); }
  warn(message: string): void { this.lines.push(`warn: ${message}`); }
  error(message: string): void { this.lines.push(`error: ${message}`); }
  output(message: string): void {
    this.lines.push(message);
    this.printed.push(message);
  }
}

/** A signal source that records what would have been re-raised instead of killing the test runner. */
export class FakeProcess extends EventEmitter {
  readonly reenacted: NodeJS.Signals[] = [];
  control(): ProcessControl {
    return {
      source: this,
      reenact: (signal) => { this.reenacted.push(signal); },
      forceExit: (error) => { throw error instanceof Error ? error : new Error(String(error)); }
    };
  }
}
