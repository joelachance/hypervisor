package parse

import (
	"path/filepath"
	"strings"

	"github.com/satoricorp/demux/capture"
)

// Claude parses Claude Code session logs.
//
// Claude Code writes one JSONL file per session under
// ~/.claude/projects/<path-slug>/<session-uuid>.jsonl, where the slug is the
// worktree path with separators replaced by dashes. That slug is the only
// place the repo root appears, so it is worth recovering.
type Claude struct{ roots []string }

// NewClaude returns the Claude Code parser.
func NewClaude() *Claude {
	return &Claude{roots: []string{home(".claude", "projects")}}
}

// Tool implements Parser.
func (c *Claude) Tool() string { return capture.ToolClaude }

// Roots implements Parser.
func (c *Claude) Roots() []string { return append([]string(nil), c.roots...) }

// Owns implements Parser.
func (c *Claude) Owns(path string) bool {
	if filepath.Ext(path) != ".jsonl" {
		return false
	}
	for _, root := range c.roots {
		if root != "" && strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// ParseLine implements Parser.
func (c *Claude) ParseLine(path string, line []byte) ([]capture.SessionEvent, error) {
	raw, err := rawLine(line)
	if err != nil {
		return nil, err
	}
	_ = raw
	// TODO(hv): map Claude Code records to capture.SessionEvent.
	//   - session id: the "sessionId" field, falling back to the file's base
	//     name, which is the session uuid
	//   - kind: assistant tool_use blocks named Edit/Write/NotebookEdit become
	//     KindEdit with FilePath/OldText/NewText from the tool input; Read
	//     becomes KindRead; user/assistant text becomes KindMessage
	//   - ts: "timestamp", RFC3339, converted to unix milliseconds
	//   - model: the "message.model" field on assistant records
	// Sidechain records (subagent transcripts) carry their own session id and
	// must not be folded into the parent session.
	return nil, nil
}

// RepoRootFromLogPath recovers the worktree a Claude session ran in from its
// project slug directory.
func RepoRootFromLogPath(path string) string {
	// TODO(hv): invert the slug encoding. It is lossy - a dash in a real
	// directory name is indistinguishable from a separator - so this must
	// verify candidates against the filesystem rather than trusting the
	// first reconstruction.
	return ""
}
