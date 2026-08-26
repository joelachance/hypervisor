# Working on hv

hv watches agent coding sessions, groups their uncommitted work into
reviewable revisions, and commits them. It reads session logs; it never writes
to them.

## Commands

```bash
go build -o hv ./cmd/hv    # the binary
go test ./...              # everything; several packages shell out to git
go vet ./...
gofmt -w .
```

Tests that need `git` skip themselves when it is missing. Nothing needs
network access or an API key.

## Trying it

`~/git/fake` holds throwaway repositories and a synthetic session store.
`./hv` there points the binary at that store; `./reset.sh` puts it back.
`hv -store <dir>` does the same from anywhere, and hv says which store it is
reading whenever it is not your home directory.

## Constraints worth knowing

**hv only adds objects and moves refs.** It never checks anything out and
never edits a file in the working tree. Committing is `read-tree`,
`apply --cached`, `write-tree`, `commit-tree`, `update-ref` against a scratch
index. This is what makes undo exact, and what lets it commit part of a mixed
working tree. Do not reach for `git add`, `git stash`, or a checkout.

**Attribution is evidence, never authority.** Session links inform grouping and
fill in commit messages; they never choose a branch.

**Everything the browser does has a flag.** A feature reachable only through
the TUI cannot be scripted or run over ssh.

**Planning is deterministic.** demux's grouping, ordering, and routing must
stay reproducible for the same inputs. A coding harness is asked only to settle
merge conflicts, and its answer is checked - every line kept, rewritten, or
dropped with a reason - before it reaches a commit.

## Layout

```
internal/gitx/   every git invocation hv makes, and conflict-region handling
internal/text/   the small string helpers output is built from

parse/      per-tool session log decoding
watch/      polling sweep over the log roots
session/    the indexed Session and its state
desktop/    the Claude app's own session registry
selection/  filters
recall/     transcripts and cross-repo memory
match/      hunk to session attribution
plan/       the demux handoff
apply/      building commits and moving refs
journal/    what was committed, and undo
merge/      the one path from chosen sessions to commits
conflict/   merge-tree analysis
resolve/    harness-driven conflict resolution
ui/         the browser, one file per screen
cmd/hv/     the CLI
```

**Both frontends go through `merge`.** The browser and the command line each
used to orchestrate plan-and-commit themselves, and the browser silently lost
its commit messages' Why paragraph because a fix landed in one copy. Add
behaviour to `merge`; the frontends only decide what to say about the outcome.

**Shell out to git only through `internal/gitx`.** Four packages had grown
their own copy, which is four ideas about when a non-zero exit is a failure.
It usually is not: several git commands report a finding through the exit code,
and `gitx.Output` exists for those.

demux itself lives in a sibling checkout (`../demux`) and is wired in with a
`replace` directive; the two repositories have to stay siblings.
