# Settings

The treerig root, the control home and `settings.toml`: what is true of this machine rather than of a project.

`TREERIG_HOME` is the treerig root — `~/.treerig` by default — holding
`settings.toml`, the shared router's own state and, unless
`TREERIG_CONTROL_HOME` overrides it, the control home, nested under it as
`control-home/`. `TREERIG_CONTROL_HOME` moves only the control home, leaving
everything else where `TREERIG_HOME` puts it. None of these are config-file
settings: the config file is committed and shared, and a path that is only
correct on one machine has no business in it — one developer pointing it at
a synced folder would put the whole team's state on iCloud Drive.

`settings.toml` is the machine's own file, never committed, for what is true
of this machine rather than of a project. Today that is the router's
loopback port and network mode and the range routed services' loopback ports
are allocated from:

```toml
[router]
port = 8080
mode = "host"           # optional: "host" or "bridge"; omit it to detect from docker info

[ports]
range = "20000-29999"   # the default; <low>-<high>, 1024 to 65535
```

Unknown keys are rejected, as in `treerig.toml`; a missing file means the
defaults. The router's port is `TREERIG_ROUTER_PORT` when that is set, else the file's,
else 80. `router status` and `doctor` say which one applied and name the
file; `doctor` warns when the running router serves a different port; and
an `up` that moves the router to a new port says that URLs already open — the
index page included — still use the old one.

Because `worktree remove` deletes inside it, the control home must be empty
or one the tool created earlier — it leaves a `.treerig-control-home` marker to
recognise its own, and refuses a directory whose marker holds anything but
its own format, or is empty beside existing content. The marker says which
format of control home it is (`format 2`). Your home directory, anything
above it, and the filesystem root are always refused, whether reached through
`TREERIG_HOME` or `TREERIG_CONTROL_HOME`.

**Stored-state format.** Everything treerig writes and reads back says which
format wrote it: the control home's marker, the identity markers in the Git
directory, registrations and shared-service records, the router's `state.json`,
a lock's holder record, a worktree's index card, and a `dev.treerig.format` label
on every container, network and image treerig creates for a worktree or a shared
stack (named volumes carry none: Compose would offer to recreate a volume whose
label changed, losing its data). A build that finds a newer format than it
reads refuses, names what it found and what it reads, and says to upgrade
treerig, for example `Refusing to use the router state … : it is format 2,
written by a newer treerig, and this build reads format 1. Upgrade treerig, then
run this again.`; it does not read, repair, rewrite or delete that state. A
container, network or image of a newer format, or without the label, is not
proven to be yours, so `worktree remove`, `shared down` and the network
connects and disconnects refuse to touch it, and the message says what to
remove by hand. State with no recognisable format is treated as not treerig's.
`doctor` reports it for the control home, registrations, shared-service
records, the router state, lock records and a checkout's identity markers; it
does not look at Docker resources. [`design/state.md`](design/state.md)
has the full list.
