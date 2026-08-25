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

2 commits on 1 branches (1 new, 0 extended); working tree untouched
undo with: hv undo -C ~/git/fake/alpha
```

Nothing is ever checked out. Each revision's patch goes into a temporary index
(`read-tree`, `apply --cached`, `write-tree`, `commit-tree`) and the branch ref
moves last, which has three consequences worth relying on:

- **the working tree keeps every change**, exactly as it was, so nothing is
  lost if the commits turn out wrong;
- a dirty tree is not an obstacle, and it always is one here - committing part
  of a mixed working tree is the whole point;
- **undo is only ref surgery**, because refs are the only thing that changed.

Revisions routed to the same branch become successive commits on it, in plan
order. An existing branch is extended from its own tip rather than rewritten:
hv adds commits, it never discards anyone else's. Session provenance is written
into the commit as `hv-session:` trailers, so it survives outside hv.

### Undo

```bash
hv undo -C <repo>          # take back the last applied plan
hv undo -C <repo> -list    # what has been applied here, and what is undone
hv undo -C <repo> -id ...  # an earlier one
```

Branches hv created are deleted; branches it extended are reset to the commit
they were at. The commits stay in the object database, unreferenced, where the
reflog keeps them recoverable. **If a branch has moved since it was applied,
undo refuses** rather than throwing away whatever moved it.

Every apply is journalled in `.git/hv-journal.jsonl`, inside the git directory
so it never appears as an untracked file in the tree hv is planning.

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
| `match` exact tier | not implemented - returns no match |
| `match` fuzzy tier | not implemented - returns no match |
| temporal tier | works; attribution is by file and recency |
| Cursor edit evidence | needs the SQLite store |
| watcher mid-write records | a sweep landing mid-append can cut the last record |

Only the temporal tier runs today, so a hunk is attributed to whichever
selected session last edited that file. That is right far more often than not
and wrong in exactly one situation worth caring about: two sessions editing
the same file in the same window. The exact tier is what fixes it - it must
stay deterministic, because everything downstream in the planner is.
