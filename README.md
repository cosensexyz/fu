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

**Thirteen commands ship today:** `init`, `new`, `list`, `show`, `status`,
`restore`, `revert`, `enable`, `disable`, `add`, `adopt`, `rm`, `gc`.

`add` installs a skill from a git URL or a local directory and records the
locked source; `adopt` takes skills that already live in an agent's directory
into the store, switching them to fu links; `rm` unregisters a skill and
removes it from every agent; `gc` safely prunes completed transaction
journals; `status` reports drift between `fu.yaml` and disk without changing
the store; `restore` rebuilds agent links from `fu.yaml` and reports any
uncommitted store worktree content without discarding it; `--hard` discards
the part of that content which is tracked, and never touches untracked files;
`revert` rolls the store back a given number of operations,
converging the store's worktree to an earlier commit's tree and republishing
that as a new commit.
Still designed but not built: `update`, `outdated`,
`clone`, `push`, `pull`, `log`, `commit`,
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
make test           # not `go test ./...` — see below
```

Use `make test` rather than `go test ./...`. The `internal/engine` suite runs
for several minutes on its own, and Go's default per-package timeout is 600
seconds, so a bare `go test ./...` on a loaded machine fails with a panic dump
that looks like a real breakage and is not one. `make test` sets
`-timeout 30m` (and `-count=1`).

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

When something looks wrong, `fu status` says what differs between `fu.yaml`
and disk. It changes nothing, and finding a difference is not a failure:

```sh
$ fu status
store
  uncommitted  skills/pdf-tools/SKILL.md
agents
  missing link   claude/pdf-tools
  unmanaged      codex/scratch-notes
recovery
  1 waiting on an unfinished write (run `fu restore`, then `fu gc`)
```

`fu restore` repairs the link layer and reports the store worktree rather
than touching it:

```sh
$ fu restore
restored agent links
the store worktree was left alone; these changes are not committed:
  skills/pdf-tools/SKILL.md
record them with a write command, which commits pending hand edits first, or discard them with `fu restore --hard`
```

`fu revert` undoes operations. A pending hand edit is committed first, so
nothing you wrote is lost:

```sh
$ fu revert 1
changed 1 path(s) in the store worktree:
  skills/pdf-tools/SKILL.md
reverted 1 operation(s)
```

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
| `fu status` | Report how `fu.yaml`'s expectations and what's on disk differ, plus the store worktree's state, any unfinished transaction, and what `recovery/` and `staging/` are holding; read-only, and finding a difference is not a failure — it exits 0 with the report. Failing to read the store at all is still an error. Read-only means it writes no store content, takes no lock and creates no agent directory; opening `$FU_HOME` does recreate `staging/` and `recovery/` if they have gone missing, which every command does alike. |
| `fu restore [--hard]` | Rebuild agent links from `fu.yaml`; an uncommitted store worktree is reported and never touched. `--hard` also resets the tracked part of it back to the last commit — those edits are then gone for good, in neither git history nor `recovery/`. Untracked files are outside its reach and are reported either way — but note that a `.gitignore`d file is only untracked until fu first records it: sweeps commit ignored content deliberately, and once committed such a file is tracked and `--hard` resets it like any other. |
| `fu revert <n>` | Roll the store back `n` operations: any pending hand edit is committed first, then the store worktree converges to the tree from `n` operations ago and that becomes a new commit. |
| `fu enable <name> [--agent <a>]` | Turn a skill on, globally or for one agent. |
| `fu disable <name> [--agent <a>]` | Turn a skill off, globally or for one agent. |

`--agent` takes `claude` or `codex`.

Exit codes: `0` success, `1` the operation failed, `2` you used the command
wrongly — the wrong number of arguments, an unknown flag, a malformed flag
value (`--agent ""`, an empty `--ref`), or a malformed positional argument
(`fu revert abc`, `fu revert 0`).

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
disk under `recovery/`, and nothing will ever reclaim that space — not fu, and
not you either: the paragraph below explains why these particular entries must
be left alone. `fu status`
counts these payloads, kept apart from the ones `fu gc` collects, so there is
a report for them but still no collector.

Residue from a fu older than this is not collected either: an earlier `fu gc`
pruned the journals that described it, so no manifest is left to verify a
deletion against, and fu will not delete by name what it cannot prove. That
residue is the `removed-<skill>-<commit>` directories — the copies old `rm`
runs set aside — together with any `.fu-retired-dir-*` left beside them, which
is the half-removed form of the same thing. Those are the only entries under
`recovery/` you may remove by hand to reclaim space. When a command prints a remedy of its own,
follow that message instead: it names the exact files it found damaged and
what to do with them, which is sometimes to move a whole transaction family
aside. The rest sit there just as idle and must be left alone:
`adopt-archive-*` holds the originals an adopt replaced, `.fu-archive-*` holds
what a `new`, `add`, or `adopt` rolled back, and the `adopt-link-*.json`
records described below are the authority that automatically restoring an
archived directory or symlink would need. Delete an `adopt-archive-*` and that
operation has lost the directory it would put back, while those records
preserve only the symlink side of the same adopt. For the `removed-*`
directories themselves the timing still matters: run any write command, which
settles an interrupted transaction, and then `fu gc`; whichever ones survive
that, with no command reporting a pending or conflicting transaction, belong to
no journal fu can still act on.

`staging/` is the other machine-local directory `fu status` accounts for, and
it works differently: `fu gc` never looks at it at all. Every cleanup path
there is an in-process one, so a process killed mid-write can strand a name
that no later run will ever enumerate and collect. `fu status` reports what it
finds in three groups — entries a recovery pass settles (run any write command,
or `fu restore`), entries nothing collects yet, and entries fu holds no pending
record for. Only the last has a remedy, and it is not fu's to perform: those
names belong to whoever put them there, and they are what makes `fu new` or
`fu add` refuse to reuse a name.

`rollback-*` does not sit idle with the rest of that list: `new`, `add`, and
`adopt` all produce it (`adopt` only in its `-uncommitted` form), and
`fu status` reports it one of two ways instead of lumping it in. One still
claimed by a pending transaction shows up under unfinished transactions, the
same as any other pending operation, and is settled the same way: run any
write command, which finishes or rolls the transaction back and moves the
payload on to `.fu-archive-`. One whose owning transaction's journal is
already gone has nothing left to settle it, so `fu status` counts it among
what no command collects yet.

The same directory also contains content-addressed `adopt-link-*.json` records.
They preserve the exact identity, mode, path, and raw target of symlinks removed
during adopt, and recovery validates them before it may unlink a retired link.
A crash before the corresponding journal revision can leave an orphan record;
`fu gc` intentionally retains it because current code cannot prove it is safe
to collect. Do not delete these records by hand. They contain enough
authority to automatically restore an archived directory or symlink, but
this release does not ship a command that does so.

Agent-link retirement has one smaller crash residue outside `recovery/`.
Reconcile first renames an approved fu-owned link to an unpredictable
`.fu-retired-*` sibling, validates that moved inode and raw target, and then
unlinks it. A process crash between the rename and unlink can leave that link
behind after the live name is recreated. It does not contain user data and
does not block later commands, but this release deliberately does not collect
an unjournalled name automatically. `fu status` reports it the same way it
reports any other unmanaged entry, under whichever agent it is sitting in;
it is not part of the `recovery/` inventory described above.

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
nothing you wrote is silently swallowed into an unrelated change. That sweep
is deliberately indiscriminate: it records untracked files, and `.gitignore`d
ones too, so nothing you left in the store can be lost by a later fu operation
that knew nothing about it. Read-only commands (`list`, `show`, `status`) take
no lock and do not sweep, so an edit stays uncommitted until you next run a
write command.

**`fu restore --hard` discards instead of reporting.** Left at its default,
`restore` only reports an uncommitted change in the store's own worktree, the
same way `list`, `show`, and `status` never touch anything. Add `--hard` and
it resets every tracked path back to the last commit, the way
`git reset --hard` does: content in a file fu does not track is left exactly
as it is, and none of it is archived first, because none of it is ever touched.
The path set is `union(index, HEAD)` and nothing else — being `.gitignore`d
does not keep a file out of it. That is where fu departs from git, which never
tracks an ignored file: fu's sweep commits ignored content on purpose, so once
a write command has recorded one, `--hard` will discard an uncommitted edit to
it just as it would to any tracked file.

**`fu revert` does not refuse a dirty worktree, the way `git revert` does.**
git refuses when local changes could conflict with the change it is
reverting. fu's revert cannot conflict with anything, because it converges
the worktree straight to a past commit's tree instead of applying a patch —
so instead of refusing, it commits any pending hand edit to its own
`external: manual modifications` commit first, the same rule every write
command already follows, and then proceeds. That edit is not lost — it is
its own commit in the store's git history (`fu log` is not built yet, but
`git -C ~/.fu/store log` shows it) — but it does not reappear in the
worktree after the revert; only the reverted operation's target content
does.

**fu will not touch what it did not create.** If something already occupies the
path where a symlink would go — a real directory, or a link you made yourself —
fu reports it and leaves it alone rather than replacing it. The same applies in
reverse: it only removes links spelled the way fu spells its own. That is a
test of the link's shape, not proof of who wrote it: a symlink you created by
hand whose target happens to match exactly what fu would have written is
indistinguishable from fu's own, and is treated as fu's.

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
| `log` | Browse history; `git -C ~/.fu/store log` works today. |
| `commit` | Record edits deliberately. |
| `remote`, `agent` | Configure the store's remote, and inspect or configure agent adapters. |

[SPEC.md](SPEC.md) states the product in full; [DESIGN.md](DESIGN.md) is the
implementation design, including the known gaps. Both are written in Chinese.

## License

MIT — see [LICENSE](LICENSE).
