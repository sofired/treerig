import { EventEmitter } from 'node:events';
import type { ProcessControl } from '../../src/commands/context.ts';
import type { Logger } from '../../src/logger.ts';

/** Captures every line; `printed` is what a user would have seen on stdout. */
export class RecordingLogger implements Logger {
  readonly lines: string[] = [];
  info(message: string): void { this.lines.push(`info: ${message}`); }
  warn(message: string): void { this.lines.push(`warn: ${message}`); }
  error(message: string): void { this.lines.push(`error: ${message}`); }
  output(message: string): void { this.lines.push(message); }
  get printed(): string[] { return this.lines.filter((line) => !/^(info|warn|error): /.test(line)); }
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
