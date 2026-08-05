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

**Six commands ship today:** `init`, `new`, `list`, `show`, `enable`, `disable`.

That is enough to write your own skills and manage which agents see them. It is
not enough to install skills from anywhere else — `add`, `adopt`, `update`,
`clone`, `push`, `pull`, `log`, `revert` and the rest are designed but not
built. See [Roadmap](#roadmap).

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
├── recovery/                machine-local: the write-ahead journal
└── fu.lock                  machine-local: write mutex
```

Only `store/` is a git repository, and only `store/` is meant to be synced. The
other three are per-machine bookkeeping.

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
directly whenever you like. The next fu command notices and commits it as an
`external: manual modifications` commit before doing its own work, so nothing
you wrote is silently swallowed into an unrelated change.

**fu will not touch what it did not create.** If something already occupies the
path where a symlink would go — a real directory, or a link you made yourself —
fu reports it and leaves it alone rather than replacing it. The same applies in
reverse: it only removes links whose target it can prove it wrote.

**Interruptions recover on their own.** Every write is journaled before it
happens. If a command is killed part-way, the next one finishes it, rolls it
back, or reports a genuine conflict — you do not need to know where it stopped
or repair anything by hand.

**`store/` is an ordinary git repository.** If you prefer, use `git` in it
directly. fu is built to tolerate that rather than to own the repository
exclusively.

## Roadmap

Designed in [DESIGN.md](DESIGN.md), not yet built:

| | |
|---|---|
| `add`, `adopt` | Install a skill from a git URL; take existing scattered skills into the store. |
| `update`, `outdated` | Track upstream versions and upgrade against a recorded commit. |
| `clone`, `push`, `pull` | Move the store between machines. |
| `log`, `revert`, `restore` | Browse history, undo a committed change, repair an uncommitted one. |
| `commit`, `rm`, `status` | Record edits deliberately, remove a skill, inspect pending state. |

[SPEC.md](SPEC.md) states the product in full; [DESIGN.md](DESIGN.md) is the
implementation design, including the known gaps. Both are written in Chinese.

## License

MIT — see [LICENSE](LICENSE).
