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

Scaffold. Every package has its types and function signatures; the bodies are
stubs marked `TODO(hv)`. Nothing parses a real log yet.

## Commands

```bash
go build -o hv ./cmd/hv

hv sessions          # list known coding sessions
hv watch             # tail session logs and print events as they land
hv plan -session ID  # plan the working tree, attributing hunks to a session
```

## Packages

```
session/     Session + SessionIndex: the selectable unit
parse/       per-tool session log parsers -> capture.SessionEvent
watch/       filesystem watcher over session log roots
selection/   filters that turn "which sessions?" into a set
match/       working-tree hunks -> matcher.HunkLink (exact/fuzzy/temporal)
plan/        demux handoff: ComposeCaptureResult + Planner invocation
cmd/hv/      CLI
```

## The two-pass handoff

Demux assigns hunk IDs (`h1`, `h2`, …) positionally while parsing the diff, and
those IDs are what a `matcher.HunkLink` points at. hv therefore plans twice:

1. `ProposeFromDiff` with no capture — to learn the hunk catalog;
2. match the catalog's hunks against the selected sessions' events;
3. `ProposeFromDiff` again, same diff, now carrying the links as evidence.

The second pass must receive a byte-identical diff and otherwise identical
options, or the links land on the wrong hunks. `plan.Run` enforces this by
building the diff once.

## What is stubbed

| Package | State |
|---|---|
| `session` | working |
| `selection` | working; `Interactive` is the identity function |
| `watch` | working; can cut a record in half if a sweep lands mid-write |
| `parse` | dispatch works; **no tool's records are decoded yet** |
| `match` | temporal tier works; exact and fuzzy return no match |
| `plan` | working end to end, including the git `ContentProbe` |
| `cmd/hv` | working |

Until `parse` decodes real logs, `hv sessions` lists nothing and `hv plan`
falls back to planning the whole working tree unattributed — which is exactly
what `demux` alone does.

## Next

1. Decode Claude Code records (`parse/claude.go`) — the mapping is written down
   in the TODO there; sidechain records carry their own session ID and must not
   fold into the parent.
2. Recover the repo root from Claude's project slug. The encoding is lossy, so
   candidates need verifying against the filesystem.
3. Implement the exact and fuzzy match tiers. They must stay deterministic —
   everything downstream in the planner is.
4. Build the interactive picker.
