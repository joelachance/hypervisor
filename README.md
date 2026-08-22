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

## Commands

```bash
go build -o hv ./cmd/hv

hv sessions                  # live / idle / ended sessions, newest first
hv sessions -tool codex      # one tool; -state live; -since 6h; -json
hv watch                     # tail edits as they land
hv merge                     # pick sessions in this repo, plan their changes
hv merge -session a1b2,c3d4  # merge named sessions without the picker
hv merge -all -repair -json  # every candidate, repaired, as a plan packet
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

Liveness is inferred from silence, because a log is written on every turn:
**live** under 3 minutes, **idle** under 2 hours, **ended** after that.

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
   1  live  claude    34 edits  0be860ce-689 pull demux logic into ~/git/hv
   2  idle  codex      6 edits  019ff782-de0 fix the rounding bug

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
