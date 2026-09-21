import { BRAND } from '../brand.ts';
import type { HookStage } from '../config/schema.ts';

// The other half of the public contract. Hooks are ordinary executables; the
// tool never learns what they do. Everything a hook needs to know arrives as
// environment variables, and its exit code is its only answer.

/** The environment every hook receives, in addition to the parent's. */
export interface HookEnvironment {
  /** Which stage is running: prepare, seed, verify, backup, or restore. */
  STAGE: HookStage;
  PROJECT: string;
  REPOSITORY_ID: string;
  WORKTREE_ID: string;
  /** Unique, routable name for this worktree. */
  SLUG: string;
  /** The worktree's routed hostname, `<slug>.<domain>`. */
  HOST: string;
  /**
   * The worktree's private directory under the control home. Deliberately not
   * `HOME`: `BERTH_HOME` is the user's control-home override and must pass
   * through to hooks untouched, or a hook that calls the tool would resolve
   * the wrong control home.
   */
  WORKTREE_HOME: string;
  /** Absolute path of the config file the tool loaded. */
  CONFIG: string;
  /** Directory the consuming repository's config lives in. */
  ROOT: string;
}

/**
 * `HOME` → `BERTH_HOME`, etc. The prefix is a storage-contract name, not the
 * invoked CLI name, so hook scripts keep working however the binary is called.
 */
export function hookEnvironmentVariables(environment: HookEnvironment): NodeJS.ProcessEnv {
  const variables: NodeJS.ProcessEnv = {};
  for (const key of Object.keys(environment) as (keyof HookEnvironment)[]) {
    variables[`${BRAND.envPrefix}_${key}`] = environment[key];
  }
  return variables;
}

/**
 * The order stages run in, and the rule that makes the sequencer worth having:
 * `seed` never runs unless `prepare` exited zero. The lifecycle stops at the
 * first failure and reports which stage failed.
 */
export const LIFECYCLE_ORDER = ['prepare', 'seed', 'verify'] as const satisfies readonly HookStage[];
export type LifecycleStage = (typeof LIFECYCLE_ORDER)[number];

/** Stages that receive a bundle directory as their first argument. */
export const BUNDLE_STAGES = ['backup', 'restore'] as const satisfies readonly HookStage[];
