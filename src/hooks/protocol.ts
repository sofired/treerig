import { BRAND } from '../brand.ts';
import type { HookStage } from '../config/schema.ts';
import { worktreeHost, worktreeLayout } from '../layout.ts';
import type { WorktreeRef } from '../runtime/runtime.ts';

// The other half of the public contract. Hooks are ordinary executables; the
// tool never learns what they do. Everything a hook needs to know arrives as
// environment variables, and its exit code is its only answer.

/**
 * What a worktree looks like from outside the tool. Hooks receive this plus a
 * stage; the Compose file receives exactly this, so `${BERTH_WORKTREE_HOME}`
 * means the same thing in a bind mount as it does in a seed script.
 */
export interface WorktreeEnvironment {
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

/** What it takes to describe a worktree to the outside: the project, plus where things live. */
export interface WorktreeEnvironmentSource {
  projectName: string;
  domain: string;
  controlHome: string;
  /** Absolute path of the loaded config file. */
  configFile: string;
  /** Directory containing the config file. */
  projectRoot: string;
}

/** One builder, so hooks and the Compose file can never disagree about a value. */
export function worktreeEnvironment(source: WorktreeEnvironmentSource, ref: WorktreeRef): WorktreeEnvironment {
  return {
    PROJECT: source.projectName,
    REPOSITORY_ID: ref.repositoryId,
    WORKTREE_ID: ref.worktreeId,
    SLUG: ref.slug,
    HOST: worktreeHost(ref.slug, source.domain),
    WORKTREE_HOME: worktreeLayout(source.controlHome, ref.repositoryId, ref.worktreeId).home,
    CONFIG: source.configFile,
    ROOT: source.projectRoot
  };
}

/** The environment every hook receives, in addition to the parent's. */
export interface HookEnvironment extends WorktreeEnvironment {
  /** Which stage is running: prepare, seed, verify, backup, or restore. */
  STAGE: HookStage;
}

/**
 * `HOME` → `BERTH_HOME`, etc. The prefix is a storage-contract name, not the
 * invoked CLI name, so hook scripts keep working however the binary is called.
 */
export function hookEnvironmentVariables(
  environment: WorktreeEnvironment | HookEnvironment
): NodeJS.ProcessEnv {
  const variables: NodeJS.ProcessEnv = {};
  for (const [key, value] of Object.entries(environment)) {
    variables[`${BRAND.envPrefix}_${key}`] = String(value);
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

/**
 * The stages something actually calls today. Everything else in `HOOK_STAGES`
 * is accepted and validated so a repository can commit it once, and is inert
 * until a command invokes it; `config validate` reports the difference.
 */
export const INVOKED_STAGES: readonly HookStage[] = ['prepare', 'seed'];

/** Stages that receive a bundle directory as their first argument. */
export const BUNDLE_STAGES = ['backup', 'restore'] as const satisfies readonly HookStage[];
