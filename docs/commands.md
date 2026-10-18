# Status words

What the status words that `status`, `worktree list` and the checkout index print mean.

`status` reports each declared service as `running`, `starting` (Compose says
`restarting`, or `running` with its health check still pending), `stopped`
(Compose says `created`, `exited`, `dead`, `paused` or `removing` — the
container exists but nothing is serving), `absent` (no container at all, the
state after `down`), or `unknown` for any state Compose adds that this list
does not name. `unknown` carries Compose's own word in parentheses, and a
running service that is failing its health check carries `(unhealthy)`.

Those five words are about one service. Two other vocabularies are about a
whole checkout. `worktree list` reports each registration as `ready`,
`pending` (registered, but its first `up` never finished), or `missing`
(registered, but the checkout is gone). Only this repository's checkouts can
be called missing — that needs Git, and `--all` lists records from other
repositories as the registry knows them. The index page answers a different
question — whether a checkout's services are up — with `Running`, `Partial`,
`Stopped`, `Unknown`, and the same `Missing`, which means the same thing in
both places. None of the three vocabularies is frozen before 1.0. Their
machine-readable forms are in [Machine-readable output](json-output.md).
