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
