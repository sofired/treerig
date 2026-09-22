import path from 'node:path';

// Two kinds of name live here, and the distinction matters.
//
// PRESENTATION — what people see: the command name in help text, the log
// prefix, "run `X urls`" hints. This follows whatever the binary was installed
// or symlinked as, so a project that ships the tool as `duke` sees `duke`
// everywhere. Nothing is configured; see `cliName()`.
//
// STORAGE IDENTITY — the config filename, the environment variable, the
// control-home directory, the Docker label namespace, and the marker directory
// inside `.git`. These are on-disk contracts. They deliberately do NOT follow
// the binary name, because renaming a symlink must never orphan state.
//
// Renaming the product means changing `BRAND` and nothing else in `src/` — but
// it is not only a code change. Every installation already has a control home,
// a config file, identity markers inside `.git`, and Docker resources labelled
// under the old namespace; a new `BRAND` cannot see any of them. A rename is a
// migration that has to move or adopt that state, and the tool has no verb for
// it yet. `label-namespace` in config exists to adopt one predecessor's
// resources, which is the closest thing available.

export const BRAND = {
  /** Lower-case product name; every storage identifier derives from it. */
  name: 'berth',
  /** Committed per-repository configuration file, searched upward from cwd. */
  configFile: 'berth.toml',
  /** The annotated copy in this repository, which the missing-config error points at. */
  exampleFile: 'berth.example.toml',
  /** Environment variable that overrides the control home. */
  homeEnv: 'BERTH_HOME',
  /** Control-home directory name under `$HOME` when the override is unset. */
  defaultHomeDir: '.berth',
  /** Directory inside a Git dir that holds the identity markers. */
  markerDir: 'berth',
  /**
   * Docker label namespace this tool owns. Every resource it creates carries
   * `<namespace>.managed`, `<namespace>.repository-uuid`, and
   * `<namespace>.worktree-uuid`. Consumers may override it in config solely to
   * adopt resources created by a predecessor tool.
   */
  labelNamespace: 'dev.berth',
  /** Prefix for the environment variables handed to hooks, e.g. `BERTH_STAGE`. */
  envPrefix: 'BERTH'
} as const;

/**
 * The name this CLI was invoked as.
 *
 * Reads the basename of `argv[1]`, which is the symlink an installation is
 * reached through — and would be the binary if there were one. Running the
 * source by path (`node src/cli/main.ts`) leaves no meaningful name, so the
 * product name is used instead.
 */
export function cliName(argv: readonly string[]): string {
  const script = argv[1];
  if (!script) return BRAND.name;
  const invoked = path.basename(script).replace(/\.(m?js|ts)$/, '');
  return invoked && invoked !== 'main' ? invoked : BRAND.name;
}
