# fu (符)

A local skill manager for AI coding agents. Keep one copy of each skill, use it
from every agent, and turn it on or off with one command.

```
$ fu list
SKILL          GLOBAL  claude  codex
code-review    on      on      off*
pdf-tools      on      on      on
release-notes  off     off     off
```

## The problem

`SKILL.md` has become a shared format — Claude Code, Codex and others read the
same skill directory without modification. But each agent still keeps its own
directory (`~/.claude/skills/`, `~/.codex/skills/`), so in practice you end up
copying every skill into every agent, editing several copies when one changes,
and moving files in and out of directories when you want to turn something off.
Nothing records what you did, so there is nothing to undo.

fu keeps the single copy in a git repository and projects it into each agent's
directory with symlinks. Enabling and disabling a skill is a switch, not a file
move, and every change is a commit.

## Status

**Ten commands ship today:** `init`, `new`, `list`, `show`, `enable`, `disable`,
`add`, `adopt`, `rm`, `gc`.

`add` installs a skill from a git URL or a local directory and records the
locked source; `adopt` takes skills that already live in an agent's directory
into the store, switching them to fu links; `rm` unregisters a skill and
removes it from every agent; `gc` safely prunes completed transaction journals.
Still designed but not built: `update`, `outdated`,
`clone`, `push`, `pull`, `log`, `revert`, `restore`, `commit`, `status`,
`remote`, `agent`. See [Roadmap](#roadmap).

**macOS and Linux.** fu relies on POSIX directory-relative syscalls and does not
build on Windows.

Pre-1.0: the CLI surface may still change.

## Install

```sh
go install github.com/cosensexyz/fu/cmd/fu@latest
```

Requires Go 1.25 or newer. The binary lands in `$(go env GOPATH)/bin`, which
needs to be on your `PATH`.

To build from source instead:

```sh
git clone https://github.com/cosensexyz/fu.git
cd fu
make build          # produces bin/fu
```

## Quick start

Create the store. fu detects your agents by looking for `~/.claude` and
`~/.codex`, so install your agents first.

```sh
$ fu init
initialized store at /Users/you/.fu
```

Write a skill. `new` scaffolds it, registers it, and links it into every
detected agent straight away:

```sh
$ fu new pdf-tools
created pdf-tools

$ fu list
SKILL      GLOBAL  claude  codex
pdf-tools  on      on      on
```

The skill itself lives in the store, and that is the copy you edit:

```sh
$ $EDITOR ~/.fu/store/skills/pdf-tools/SKILL.md
```

Turn it off for one agent while leaving it on for the others:

```sh
$ fu disable pdf-tools --agent codex
disabled pdf-tools for codex; takes effect in new agent sessions

$ fu list
SKILL      GLOBAL  claude  codex
pdf-tools  on      on      off*
```

The `*` marks a per-agent override — this agent is not following the global
switch. Turn the skill off everywhere:

```sh
$ fu disable pdf-tools
disabled pdf-tools globally; takes effect in new agent sessions

$ fu list
SKILL      GLOBAL  claude  codex
pdf-tools  off     off     off*
```

The override on `codex` is still there, which is why it still carries a `*`
even though it now agrees with the global switch. See
[Behaviour worth knowing](#behaviour-worth-knowing).

## How it works

One copy in the store, projected outward by symlink:

```
~/.fu/store/skills/pdf-tools/     ← the skill; this is what you edit
        │
        ├──→ ~/.claude/skills/pdf-tools   (symlink, exists while enabled)
        └──→ ~/.codex/skills/pdf-tools    (symlink, exists while enabled)
```

Each skill has a **global switch** and an optional **per-agent override**. The
global switch is the default every agent follows; an override applies to one
agent only. Writing an override that matches the current global value removes it
instead, so the agent goes back to following the default:

```sh
$ fu disable pdf-tools --agent codex   # global on  → codex gets an override
$ fu enable  pdf-tools --agent codex   # matches global → override removed
```

`fu list` marks an override with `*`. `fu show` spells it out:

```sh
$ fu show pdf-tools
name:        pdf-tools
description: Extract and convert PDF content
digest:      sha256:8a41bb85…
global:      on
claude: on (follows global)
codex: off (override)
```

Enabling a skill creates the symlink; disabling removes it. Nothing is copied,
so a skill is never out of date in one agent and current in another.

## Commands

| Command | What it does |
|---|---|
| `fu init` | Create the store at `$FU_HOME` (default `~/.fu`). |
| `fu new <name>` | Scaffold a skill in the store, enabled everywhere. |
| `fu add [--all] [--ref <ref>] <source>` | Install from a git URL (including SCP forms) or local directory; `--ref` explicitly selects a git branch or tag. A source holding several skills prompts for a comma-separated selection (or `all`); `--all` installs every valid skill without prompting. Submitting an empty selection is an intentional successful cancellation (nothing is installed); input ending before any choice is an error. |
| `fu adopt [--agent <a>]` | Take existing skill entries into the store, switching them to fu links; an explicitly empty agent value is rejected. |
| `fu rm <name>` | Unregister a skill and remove it from every agent. |
| `fu gc` | Safely prune completed transaction journal revisions and markers, and reclaim the bookkeeping a finished `rm` or `fu.yaml` rewrite no longer needs; the originals an `adopt` replaced are never deleted. |
| `fu list` | Show every skill and the full switch matrix. |
| `fu show <name>` | Show one skill's frontmatter, digest and per-agent state. |
| `fu enable <name> [--agent <a>]` | Turn a skill on, globally or for one agent. |
| `fu disable <name> [--agent <a>]` | Turn a skill off, globally or for one agent. |

`--agent` takes `claude` or `codex`.

Exit codes: `0` success, `1` the operation failed, `2` you used the command
wrongly (unknown command, missing argument, bad flag).

## Where things live

```
~/.fu/                       $FU_HOME — override with the environment variable
├── store/                   a git repository; this is the part worth syncing
│   ├── fu.yaml              the skill registry and switch matrix
│   └── skills/<name>/       the skills themselves
├── staging/                 machine-local: work in progress
├── recovery/                machine-local: the journal, and archived originals
└── fu.lock                  machine-local: write mutex
```

Only `store/` is a git repository, and only `store/` is meant to be synced. The
other three are per-machine bookkeeping.

`recovery/` holds more than the write-ahead journal. `fu gc` prunes completed
transaction revision families, using a crash-resumable prune record before it
deletes the first journal file. It also reclaims two things a finished
operation no longer needs: the copy an `rm` set aside before it removed a
skill, and the records a `fu.yaml` rewrite wrote to make the swap recoverable.
Each is normally reclaimed by the command that created it, the moment that
command's transaction is durably complete, so `fu gc` is there for the ones a
crash stranded. It only ever deletes a payload it can still check against the
manifest in that payload's own journal, and never one an unfinished
transaction may still need. The removed skill itself stays recoverable from
the store's git history regardless.

What `fu gc` still never deletes includes every original an `adopt` replaced.
So if an adopt took in a directory you wanted back, the content is still on
disk under `recovery/`, and reclaiming that space is manual. A `fu status`
that reports those payloads, and a collector for them, are designed but not
built.

Residue from a fu older than this is not collected either: an earlier `fu gc`
pruned the journals that described it, so no manifest is left to verify a
deletion against, and fu will not delete by name what it cannot prove. That
residue is the `removed-<skill>-<commit>` directories — the copies old `rm`
runs set aside — and they are the only entries under `recovery/` you may
remove by hand to reclaim space. When a command prints a remedy of its own,
follow that message instead: it names the exact files it found damaged and
what to do with them, which is sometimes to move a whole transaction family
aside. The rest sit there just as idle and must be left alone:
`adopt-archive-*` holds the originals an adopt replaced, `rollback-*` and
`.fu-archive-*` hold what a `new` or `add` rolled back, and the
`adopt-link-*.json` records described below are the authority a restore would
read. Delete an `adopt-archive-*` and a future restore has lost the directory
it would put back, while those records preserve only the symlink side of the
same adopt. For the `removed-*` directories themselves the timing still
matters: run any write command, which settles an interrupted transaction, and
then `fu gc`; whichever ones survive that, with no command reporting a pending
or conflicting transaction, belong to no journal fu can still act on.

The same directory also contains content-addressed `adopt-link-*.json` records.
They preserve the exact identity, mode, path, and raw target of symlinks removed
during adopt, and recovery validates them before it may unlink a retired link.
A crash before the corresponding journal revision can leave an orphan record;
`fu gc` intentionally retains it because current code cannot prove it is safe
to collect. Do not delete these records by hand. They contain enough authority
for a future restore operation, but this release does not ship a command that
automatically restores an archived directory or symlink.

Agent-link retirement has one smaller crash residue outside `recovery/`.
Reconcile first renames an approved fu-owned link to an unpredictable
`.fu-retired-*` sibling, validates that moved inode and raw target, and then
unlinks it. A process crash between the rename and unlink can leave that link
behind after the live name is recreated. It does not contain user data and
does not block later commands, but this release deliberately does not collect
an unjournalled name automatically. The planned `fu status` will report these
orphan links alongside retained recovery payloads.

## Behaviour worth knowing

**Changes apply to the next agent session.** Agents read their skills directory
when a session starts, so an `enable` or `disable` does not affect a
conversation already in progress. fu says so on every toggle.

**A global toggle never clears an override.** Turning a skill off globally
leaves an existing per-agent override in place, even when it now holds the same
value — so `fu list` can show `off*` beside a global `off`. This is deliberate:
clearing overrides on a global switch would silently discard a choice you made
per agent, and you would not get it back by toggling the global switch again.
The override disappears when you write it to match the global value yourself.

**Hand-editing the store is expected.** Edit `~/.fu/store/skills/<name>/`
directly whenever you like. The next fu *write* command notices and commits it
as an `external: manual modifications` commit before doing its own work, so
nothing you wrote is silently swallowed into an unrelated change. Read-only
commands (`list`, `show`) take no lock and do not sweep, so an edit stays
uncommitted until you next run a write command.

**fu will not touch what it did not create.** If something already occupies the
path where a symlink would go — a real directory, or a link you made yourself —
fu reports it and leaves it alone rather than replacing it. The same applies in
reverse: it only removes links whose target it can prove it wrote.

**Durable mutations recover on their own.** Write commands enter recovery before
ordinary work. If a command is killed part-way, the next write command finishes
the recorded mutation, rolls it back, or reports a genuine conflict. Unknown or
replaced files are preserved rather than guessed at or deleted.

An error after the Git commit is durable is still a non-zero exit, but it is not
reported as though nothing happened: fu prints the committed mutation and names
whether post-commit work or WAL recovery remains pending.

**`store/` is an ordinary git repository.** If you prefer, use `git` in it
directly. fu is built to tolerate that rather than to own the repository
exclusively.

## Roadmap

Designed in [DESIGN.md](DESIGN.md), not yet built:

| | |
|---|---|
| `update`, `outdated` | Track upstream versions and upgrade against a recorded commit. |
| `clone`, `push`, `pull` | Move the store between machines. |
| `log`, `revert`, `restore` | Browse history, undo a committed change, repair an uncommitted one. |
| `commit`, `status` | Record edits deliberately, inspect pending state. |
| `remote`, `agent` | Configure the store's remote, and inspect or configure agent adapters. |

[SPEC.md](SPEC.md) states the product in full; [DESIGN.md](DESIGN.md) is the
implementation design, including the known gaps. Both are written in Chinese.

## License

MIT — see [LICENSE](LICENSE).
