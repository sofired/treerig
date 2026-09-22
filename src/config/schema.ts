import { z } from 'zod';

// The committed configuration file. This is half of the public contract — the
// other half is the hook protocol — so every field here is a compatibility
// promise. Keep it small.

const SLUG = /^[a-z0-9][a-z0-9-]*$/;
/** One DNS label or nothing: no leading or trailing hyphen. */
const SUBDOMAIN = /^([a-z0-9]([a-z0-9-]*[a-z0-9])?)?$/;
const HOSTNAME = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$/;
/** Shared with the registry and index cards, which store the namespace a worktree's resources carry. */
export const LABEL_NAMESPACE = /^[a-z0-9]+(\.[a-z0-9-]+)+$/;
/**
 * A URL path prefix: RFC 3986 path characters, `%` only as a proper escape,
 * and no `$`. It ends up verbatim inside a Traefik rule, where only a
 * backtick could do harm; in a printed URL, where `?` and `#` would change
 * its meaning; and in a Compose label value, where `$` is interpolated.
 */
const URL_PATH = /^\/(?:[A-Za-z0-9._~!&'()*+,;=:@/-]|%[0-9A-Fa-f]{2})*$/;
const DNS_LABEL_MAX = 63;
/** A routed hostname is `<subdomain>.<slug>.<domain>`; with two 63-character labels ahead of it, the domain gets the rest of 253. */
const DOMAIN_MAX = 253 - (DNS_LABEL_MAX + 1) * 2;

const hostnameSchema = z.string()
  .regex(HOSTNAME, 'must be a valid hostname')
  .max(DOMAIN_MAX, `must be at most ${DOMAIN_MAX} characters, so that <subdomain>.<slug>.<domain> fits in a hostname`)
  .refine((value) => value.split('.').every((label) => label.length <= DNS_LABEL_MAX), {
    message: `each label must be at most ${DNS_LABEL_MAX} characters`
  });

export const serviceSchema = z.object({
  /** Compose service name. */
  name: z.string().regex(SLUG, 'must be lower-case letters, digits, and hyphens'),
  /** Subdomain under the worktree host; empty routes at the host root. */
  subdomain: z.string()
    .regex(SUBDOMAIN, 'must be lower-case letters, digits, and hyphens, not starting or ending with a hyphen')
    .max(DNS_LABEL_MAX, `must be at most ${DNS_LABEL_MAX} characters`)
    .default(''),
  /**
   * Routes only this path prefix, so several services can share one host;
   * also where the printed URL points. Omit to route the whole host.
   */
  path: z.string().regex(URL_PATH, 'must be a URL path starting with /, without spaces, quotes, backticks, $, ? or #; % must be followed by two hex digits').optional(),
  /** Container port to route to; defaults to the first port the service exposes or publishes. */
  port: z.number().int().min(1).max(65_535).optional(),
  /** Presence on the shared checkout index; omit to keep the service internal. */
  index: z.object({
    label: z.string().min(1).max(512),
    purpose: z.string().max(512).default('')
  }).strict().optional()
}).strict();

export const hooksSchema = z.object({
  /** Services are healthy; nothing has been seeded. Migrations belong here. */
  prepare: z.string().min(1).optional(),
  /** Runs only after `prepare` exits zero. */
  seed: z.string().min(1).optional(),
  // The three below are accepted and validated now, and nothing invokes them
  // yet: they are the contract a future `doctor`, `backup` and `restore` will
  // call, declared early so a repository can commit them once.
  /** Accepted and validated now; invoked by a future `doctor`. */
  verify: z.string().min(1).optional(),
  /** Accepted and validated now; invoked by a future `backup`, with a bundle directory as its first argument. */
  backup: z.string().min(1).optional(),
  /** Accepted and validated now; invoked by a future `restore`, with a bundle directory as its first argument. */
  restore: z.string().min(1).optional()
}).strict();

export const configSchema = z.object({
  project: z.object({
    name: z.string().regex(SLUG, 'must be lower-case letters, digits, and hyphens'),
    /** Worktrees are routed as `<slug>.<domain>`. */
    domain: hostnameSchema,
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
  const routes = new Set<string>();
  config.service.forEach((service, index) => {
    if (names.has(service.name)) {
      context.addIssue({
        code: 'custom',
        path: ['service', index, 'name'],
        message: `duplicate service name "${service.name}"`
      });
    }
    names.add(service.name);
    // A route is identified by subdomain and path together: two services may share a host if their paths differ.
    const route = `${service.subdomain}\0${service.path ?? ''}`;
    if (routes.has(route)) {
      const host = service.subdomain === '' ? 'the host root' : `subdomain "${service.subdomain}"`;
      const where = service.path === undefined ? host : `${host} at path "${service.path}"`;
      context.addIssue({
        code: 'custom',
        path: ['service', index, service.path === undefined ? 'subdomain' : 'path'],
        message: `${where} is already claimed by another service`
      });
    }
    routes.add(route);
  });
});

export type ProjectConfig = z.infer<typeof configSchema>;
export type ServiceConfig = z.infer<typeof serviceSchema>;
export type HooksConfig = z.infer<typeof hooksSchema>;
export type HookStage = keyof HooksConfig;

export const HOOK_STAGES = ['prepare', 'seed', 'verify', 'backup', 'restore'] as const satisfies readonly HookStage[];
