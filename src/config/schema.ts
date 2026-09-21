import { z } from 'zod';

// The committed configuration file. This is half of the public contract — the
// other half is the hook protocol — so every field here is a compatibility
// promise. Keep it small.

const SLUG = /^[a-z0-9][a-z0-9-]*$/;
const SUBDOMAIN = /^[a-z0-9-]*$/;
const HOSTNAME = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$/;
const LABEL_NAMESPACE = /^[a-z0-9]+(\.[a-z0-9-]+)+$/;

export const serviceSchema = z.object({
  /** Compose service name. */
  name: z.string().regex(SLUG, 'must be lower-case letters, digits, and hyphens'),
  /** Subdomain under the worktree host; empty routes at the host root. */
  subdomain: z.string().regex(SUBDOMAIN, 'must be lower-case letters, digits, and hyphens').default(''),
  /** Path prefix appended to the routed URL. */
  path: z.string().startsWith('/').optional(),
  /** Presence on the shared checkout index; omit to keep the service internal. */
  index: z.object({
    label: z.string().min(1),
    purpose: z.string().default('')
  }).strict().optional()
}).strict();

export const hooksSchema = z.object({
  /** Services are healthy; nothing has been seeded. Migrations belong here. */
  prepare: z.string().min(1).optional(),
  /** Runs only after `prepare` exits zero. */
  seed: z.string().min(1).optional(),
  /** Folded into `doctor`. */
  verify: z.string().min(1).optional(),
  /** Receives a bundle directory as its first argument. */
  backup: z.string().min(1).optional(),
  /** Receives a bundle directory as its first argument. */
  restore: z.string().min(1).optional()
}).strict();

export const configSchema = z.object({
  project: z.object({
    name: z.string().regex(SLUG, 'must be lower-case letters, digits, and hyphens'),
    /** Worktrees are routed as `<slug>.<domain>`. */
    domain: z.string().regex(HOSTNAME, 'must be a valid hostname'),
    // The control home is deliberately NOT configurable here. This file is
    // committed and shared; a per-machine path belongs in the environment.
    /**
     * Docker label namespace. Only set this to adopt resources created by a
     * previous tool; new projects should leave it unset.
     */
    'label-namespace': z.string().regex(LABEL_NAMESPACE, 'must look like a reverse domain, e.g. dev.example').optional()
  }).strict(),
  compose: z.object({
    /** Relative to the config file. */
    file: z.string().min(1)
  }).strict(),
  service: z.array(serviceSchema).default([]),
  hooks: hooksSchema.default({})
}).strict().superRefine((config, context) => {
  const names = new Set<string>();
  const subdomains = new Set<string>();
  config.service.forEach((service, index) => {
    if (names.has(service.name)) {
      context.addIssue({
        code: 'custom',
        path: ['service', index, 'name'],
        message: `duplicate service name "${service.name}"`
      });
    }
    names.add(service.name);
    if (subdomains.has(service.subdomain)) {
      const label = service.subdomain === '' ? 'the host root' : `subdomain "${service.subdomain}"`;
      context.addIssue({
        code: 'custom',
        path: ['service', index, 'subdomain'],
        message: `${label} is already claimed by another service`
      });
    }
    subdomains.add(service.subdomain);
  });
});

export type ProjectConfig = z.infer<typeof configSchema>;
export type ServiceConfig = z.infer<typeof serviceSchema>;
export type HooksConfig = z.infer<typeof hooksSchema>;
export type HookStage = keyof HooksConfig;

export const HOOK_STAGES = ['prepare', 'seed', 'verify', 'backup', 'restore'] as const satisfies readonly HookStage[];
