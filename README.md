# hv

`hv` watches agent coding sessions, lets you pick the ones you care about, and
hands the changes they produced to [demux](../demux) for planning.

Demux deliberately stopped at the seam: it consumes `matcher.HunkLink` evidence
but never learned to produce it, because gx's session-log parsers and its
exact/fuzzy/temporal matcher were not extracted. `hv` is that missing half.

```
session logs (claude/codex/cursor)
   -> parse    normalize JSONL into capture.SessionEvent
   -> watch    tail live sessions, emit events as they land
   -> sessions index events into selectable Session records
   -> select   pick sessions (by repo, recency, tool, id)
   -> match    link working-tree hunks to the selected sessions
   -> plan     demux.Planner.ProposeFromDiff with the links as evidence
```

## Status

Working end to end against real session logs. `hv merge` finds the sessions
that edited this repo, asks which to merge, attributes their edits to hunks,
and prints demux's revision plan with provenance stamped on each revision.

## The browser

Bare `hv` opens a fullscreen session browser (Bubble Tea + Lipgloss). It shows
**every session on the machine**, whatever directory you started it in - it is
a hypervisor, not a repository tool. `r` narrows to the repo you are standing
in when you want that; `-repo` starts narrowed.

```
 hv  21 sessions  ·  3 active  ·  every repo
────────────────────────────────────────────────────────────────────────────────
❯   ● cli hv        How demux works                0be860ce      now
    ● app console   Matias Bug                     0ef82004       8m
    ○ app yeet      benchmarks                     07696390      24m  1 merged 1 open
    ○ cod topo      (untitled)                     c687636d       3d
────────────────────────────────────────────────────────────────────────────────
 benchmarks (user-named)
 id 07696390-a38b-4346-a415-056548f21fd1
 repo ~/git/yeet   branch claude/gx-review-code-bench-f18173
 edits 425 in 217 files   model claude-opus-5   prs #1 open

 ↑↓ move  ·  space mark  ·  enter merge  ·  o open  ·  / filter  ·  a archived  ·  q quit
```

Colors carry state rather than decorate it: a filled green dot for active
against a hollow grey one, purple for app sessions and blue for CLI, PR badges
colored by state. The palette is adaptive, so it renders into the terminal
theme already in use instead of assuming a dark background.

`enter` plans the marked sessions and opens the plan on screen:

```
 plan  ~/git/fake/alpha  ·  2 revisions  ·  3 hunks  ·  ready_to_apply
────────────────────────────────────────────────────────────────────────────────
❯ u1  internal/alpha.go                     → feature/internal    rate limiter
  u2  internal/limiter.go, limiter_test.go  → feature/internal    rate limiter, tests
────────────────────────────────────────────────────────────────────────────────
 diff --git a/internal/alpha.go b/internal/alpha.go
 @@ -1,3 +1,3 @@
  package alpha
 -func Version() string { return "0.1" }
 +func Version() string { return "0.2" }

 ↑↓ move  ·  tab diff  ·  esc back  ·  enter print & exit  ·  q quit   nothing applied
```

Each revision names the files it owns, the branch it would be routed to, and
the sessions whose edits are in it; the pane below is that revision's exact
patch. `tab` moves the keys into the diff to scroll it, `esc` goes back to the
sessions with your marks intact, `[` and `]` switch repositories when the
selection spanned several. `enter` leaves with the plan on stdout.

`enter` commits the plan. `u` on the result undoes it.

## Committing

`hv merge` commits. So does `enter` on the browser's plan screen. Both write
real branches and commits:

```
  978973a1  feature/internal   update internal/alpha.go
  276fc77f  feature/internal   update internal/limiter

2 commits on 1 branch (1 new, 0 extended)
  you are still on main, which did not move
  your uncommitted changes are still uncommitted - the commits are copies of them
  see them with: git log feature/internal
undo with: hv undo -C ~/git/fake/alpha
```

Nothing is ever checked out. Each revision's patch goes into a temporary index
(`read-tree`, `apply --cached`, `write-tree`, `commit-tree`) and the branch ref
moves last, which has three consequences worth relying on:

- **nothing is checked out and your branch does not move**. The commits are
  copies: the same edits are now on their branches *and* still sitting
  uncommitted in your tree, exactly as they were;
- a dirty tree is not an obstacle, and it always is one here - committing part
  of a mixed working tree is the whole point;
- **undo is only ref surgery**, because refs are the only thing that changed.

Revisions routed to the same branch become successive commits on it, in plan
order. An existing branch is extended from its own tip rather than rewritten:
hv adds commits, it never discards anyone else's. Session provenance is written
into the commit as `hv-session:` trailers, so it survives outside hv.

### Conflicts

Two sessions that touched the same lines are found with `git merge-tree
--write-tree`, which merges two commits in memory - no index, no checkout, no
working tree - and reports conflicted paths as base/ours/theirs blobs. Its exit
status is the verdict, so the analysis is deterministic and side-effect free.

```bash
hv conflicts -ours sess-a -theirs sess-b \
  -ours-session a1b2c3d4 -theirs-session b3c4d5e6
```

```
1 file(s) conflict between rate limiter and beta retry bug, from base 65180800

1. 3 files merged with no overlap: README.md, internal/limiter.go, …
2. CHOOSE  internal/alpha.go region 1
   rate limiter:    func Version() string { return "0.2-limiter" }
   beta retry bug:  func Version() string { return "0.2-retry" }

rate limiter was asked: add a rate limiter to alpha
beta retry bug was asked: fix the retry bug in beta

2 conflicted lines, all covered by the steps above: true
```

Naming the sessions is the part git cannot do. `-json` adds an `accounting`
block that proves nothing was lost: every conflicted line belongs to a step,
and `complete` is false if any does not.

## Resolving with a coding agent

```bash
hv resolve -ours sess-a -theirs sess-b \
  -ours-session a1b2c3d4 -theirs-session b3c4d5e6
```

Any harness will do. hv ships recipes for **pi, fx, claude, codex, and
opencode**, picks the first one on your PATH, and takes `-harness <binary>
-harness-args '<template with {prompt}>'` for anything else. The contract is
one question in, one answer out on stdout - the smallest thing every harness
already does, and the reason this stays open to whatever exists next year.

The harness is asked one region at a time, and is given the region, both
versions in full, and what each session was asked to do - **never the
repository**, and it runs in a scratch directory so its edit tools have
nothing to reach. It
answers with the resolved lines plus an account of what it kept from each side.
hv checks that account before anything is written: a line that existed on
either side must survive, be claimed as kept, or be listed as dropped with a
reason. A resolution that quietly loses a line is refused and no branch is
created.

Every line of both sides must be accounted for exactly once - kept verbatim,
rewritten into something else, or dropped with a reason - and that accounting
is checked before anything is written. A resolution that leaves a line out of
it is refused and no branch is created.

The result is a real merge commit - two parents, `ours` and `theirs` - on its
own branch, with each region's reasoning in the message and `hv-session:`
trailers for both sides. `hv undo` takes it back like any other merge.

`-dry-run` reports the conflicts without asking a harness or writing anything.

The accounting proves nothing was **lost**. It cannot prove the resolution is
**right**: an agent asked to reconcile `n * 2` and `n / 2` may well answer
`(n * 2) / 2`, which keeps both edits and means neither. Read the diff.

## Undo

```bash
hv undo -C <repo>          # take back the last applied plan
hv undo -C <repo> -list    # what has been applied here, and what is undone
hv undo -C <repo> -id ...  # an earlier one
```

Branches hv created are deleted; branches it extended are reset to the commit
they were at. The commits stay in the object database, unreferenced, where the
reflog keeps them recoverable. **If a branch has moved since it was applied,
undo refuses** rather than throwing away whatever moved it.

Every merge is journalled in `hv-journal.jsonl` inside the *common* git
directory, so it never appears as an untracked file in the tree hv is planning.

### Worktrees

hv works from a worktree, and this is the case it is built for: agents run in
worktrees, and worktrees of one repository share a single object database and a
single set of refs. Only `HEAD`, the index, and the working tree are per
worktree.

So a merge run in a worktree creates branches every checkout of that repository
can see, and the journal lives in the common git directory to match - undo
works from any of them, not only the one that ran the merge.

What a worktree does *not* share is uncommitted work: each has its own working
tree, and a plan is a diff of one of them. Sessions in different worktrees are
therefore planned separately, one plan per worktree, and their commits land on
branches in the same shared repository. To stack them, route them at the same
branch: hv extends an existing branch from its tip, so the second merge's
commits sit on top of the first's.

The browser owns no session logic - it reads through the same packages the
flags do. **Everything it does has a flag**, because a feature reachable only
by hand cannot be scripted or run over ssh in a hurry.

## Commands

```bash
go build -o hv ./cmd/hv

hv                           # the browser: every session, every repo
hv -repo                     # …narrowed to this repository
hv sessions                  # title, id, active/stale - newest first, 7 days
hv sessions -tool codex      # one tool; -state active; -since 6h; -json
hv watch                     # tail edits as they land
hv merge                     # pick sessions in this repo, commit their changes
hv merge -session a1b2,c3d4  # named sessions, each committed in its own repo
hv merge -dry-run            # …show the plan without committing
hv merge -all -repair -json  # every candidate, repaired, as a plan packet
hv undo -C <repo>            # take the last merge back
hv open <session>            # reopen a session where it lives
```

## How sessions register

There is no daemon and no registry. Every sweep walks the log roots, tails
what each file appended since last time, and folds the events into an index.
A session exists because its log does.

| Tool | Store | Edits visible | Repo resolved from |
|---|---|---|---|
| Claude Code | `~/.claude/projects/<slug>/<uuid>.jsonl` | Edit, Write, MultiEdit, NotebookEdit, and Bash writes | `cwd` on each record |
| Codex | `~/.codex/sessions/<date>/rollout-*.jsonl` | shell commands only | `cwd` on `session_meta` |
| Cursor | `~/.cursor/projects/<slug>/agent-transcripts/**.jsonl` | none | project slug, verified against the filesystem |

Each session is listed by **the title its own tool gave it** - Claude Code
writes an `ai-title` record once it knows what a session is about - falling
back to the first prompt, then to `(untitled)`. Claude Code is currently the
only tool that names sessions.

Activity is inferred from silence, because a log is written on every turn:
**active** if it wrote within ten minutes, **stale** otherwise. That signal
cannot tell a session someone still has open from one they walked away from -
nothing in the logs distinguishes them - so hv does not pretend to.

Only a format that declares itself untimed may borrow its log file's
modification time for a stamp. That is Cursor alone; for a timed format, a
touched file is not a working agent.

Two findings from the real stores worth knowing:

- **Codex has no edit tool.** Every file it writes goes through
  `exec_command`. Claude Code does the same whenever it prefers Bash. So
  `parse/shell.go` reads the commands themselves, recognizing only
  unambiguous write forms and stripping heredoc bodies first. Those writes
  carry no text, so only the temporal match tier can claim them - a shell
  write can never produce a false exact match.
- **Cursor transcripts are conversation only.** No timestamps, no tool calls,
  no file paths. A Cursor session registers and resolves its repo, but
  contributes no edit evidence; selecting one scopes nothing. Real Cursor
  attribution needs its SQLite store, and therefore a SQLite dependency.

## Merging n sessions

```
$ hv merge -C ~/git/hv
   1  active claude  0be860ce-6892   37 edits  How demux works
   2  stale  claude  07696390-a38b  425 edits  Setup gx review script for Code Review Bench

select sessions to merge [1-2, comma separated, or all]: 1,2

plan demux-b301279c  state=ready_to_apply  hunks=34  revisions=8  sessions=2  attributed=100%

u1   update parse/claude.go                 -> feature/parse
       parse/claude.go
       session 0be860ce-689 (matched)
```

The selected sessions' files scope the diff; their events attribute each hunk;
demux groups, orders, and routes. Selecting nothing plans the tree
unattributed, which is what demux alone would do.

## The two-pass handoff

Demux assigns hunk IDs (`h1`, `h2`, ...) positionally while parsing the diff,
and those IDs are what a `matcher.HunkLink` points at. hv therefore plans
twice:

1. `ProposeFromDiff` with no capture - to learn the hunk catalog;
2. match the catalog's hunks against the selected sessions' events;
3. `ProposeFromDiff` again, same diff, now carrying the links as evidence.

The second pass must receive a byte-identical diff and otherwise identical
options, or the links land on the wrong hunks. `plan.Run` enforces this by
building the diff once.

## A demo you can run

`~/git/fake` holds three throwaway repositories and a synthetic session store
with five sessions across them. See its README; `./hv` there is a wrapper that
points the binary at the fake store, and `./reset.sh` puts it back.

## Testing against a synthetic store

`HV_HOME` moves every log root and the desktop registry under another
directory, so hv can be exercised without writing fake sessions into the real
store:

```bash
HV_HOME=~/git/fake/.hv-home hv sessions
HV_HOME=~/git/fake/.hv-home hv merge -C ~/git/fake/alpha
```

The layout beneath it is exactly what the tools produce
(`.claude/projects/<slug>/<uuid>.jsonl`, and
`Library/Application Support/Claude/claude-code-sessions/*/*/local_*.json`).

## What is still stubbed

| Piece | State |
|---|---|
| `match` exact tier | works - added lines matched verbatim, whitespace aside |
| `match` fuzzy tier | works - overlap against the smaller side, 0.8 |
| temporal tier | works; the fallback when text says nothing |
| Cursor edit evidence | needs the SQLite store |
| watcher mid-write records | a sweep landing mid-append can cut the last record |

All three tiers run. A hunk is attributed to the session whose written text is
actually in it, falling back to recency only when the text says nothing - which
is what separates two sessions editing the same file in the same window.

The comparison is a set of trimmed, meaningful lines: whitespace changes do not
cost a link, blank lines and lone braces cannot earn one, and ties break on
session id so the same logs always produce the same links.
