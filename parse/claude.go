package parse

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/satoricorp/demux/capture"
)

// Claude parses Claude Code session logs.
//
// Claude Code writes one JSONL file per session under
// ~/.claude/projects/<path-slug>/<session-uuid>.jsonl. Every substantive
// record carries "cwd" outright, so the worktree is read directly and the
// lossy slug never has to be inverted.
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
	return ownedByRoot(path, c.roots, ".jsonl")
}

// claudeRecord is the subset of a Claude Code record hv reads.
type claudeRecord struct {
	Type       string          `json:"type"`
	SessionID  string          `json:"sessionId"`
	Timestamp  string          `json:"timestamp"`
	CWD        string          `json:"cwd"`
	Message    json.RawMessage `json:"message"`
	AITitle    string          `json:"aiTitle"`
	Entrypoint string          `json:"entrypoint"`
}

type claudeMessage struct {
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

type claudeBlock struct {
	Type  string          `json:"type"`
	Name  string          `json:"name"`
	Text  string          `json:"text"`
	Input json.RawMessage `json:"input"`
}

type claudeToolInput struct {
	FilePath   string `json:"file_path"`
	NotebookID string `json:"notebook_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	Content    string `json:"content"`
	Command    string `json:"command"`
	Edits      []struct {
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	} `json:"edits"`
}

// ParseLine implements Parser.
//
// Sidechain records - subagent transcripts - carry the parent's session id and
// are folded in deliberately: a subagent's edit is still that session's work,
// and splitting it out would attribute hunks to a session no one can select.
func (c *Claude) ParseLine(path string, line []byte) ([]capture.SessionEvent, error) {
	var record claudeRecord
	if err := json.Unmarshal(line, &record); err != nil {
		return nil, malformed(path, err)
	}
	sessionID := strings.TrimSpace(record.SessionID)
	if sessionID == "" {
		// Every session-scoped record names its session; the file's own name
		// is the session uuid, so fall back to it rather than dropping.
		sessionID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	base := capture.SessionEvent{
		SessionID: sessionID,
		Tool:      capture.ToolClaude,
		TS:        millis(record.Timestamp),
		Raw:       rawContext(record.CWD, [2]string{RawOrigin, record.Entrypoint}),
	}
	switch record.Type {
	case "assistant":
		return c.assistantEvents(base, record)
	case "user":
		return c.userEvents(base, record)
	case "ai-title":
		// Claude Code names a session once it knows what it is about, and that
		// name is what the app shows. It is worth far more as a label than the
		// first prompt, which is often a paste, a caveat block, or a slash
		// command. The record carries no timestamp: a rename is not activity.
		base.Kind = capture.KindMessage
		base.TS = 0
		base.Raw = rawTitle(record.AITitle)
		return []capture.SessionEvent{base}, nil
	default:
		return nil, nil
	}
}

func (c *Claude) assistantEvents(base capture.SessionEvent, record claudeRecord) ([]capture.SessionEvent, error) {
	var message claudeMessage
	if err := json.Unmarshal(record.Message, &message); err != nil {
		return nil, nil
	}
	base.Model = strings.TrimSpace(message.Model)
	blocks, err := decodeBlocks(message.Content)
	if err != nil {
		return nil, nil
	}
	events := []capture.SessionEvent{}
	for _, block := range blocks {
		if block.Type != "tool_use" {
			continue
		}
		var input claudeToolInput
		if len(block.Input) > 0 {
			_ = json.Unmarshal(block.Input, &input)
		}
		events = append(events, claudeToolEvents(base, block.Name, input)...)
	}
	return events, nil
}

func (c *Claude) userEvents(base capture.SessionEvent, record claudeRecord) ([]capture.SessionEvent, error) {
	var message claudeMessage
	if err := json.Unmarshal(record.Message, &message); err != nil {
		return nil, nil
	}
	text := firstText(message.Content)
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	base.Kind = capture.KindMessage
	base.PromptContext = text
	return []capture.SessionEvent{base}, nil
}

// claudeToolEvents maps one tool call to normalized events.
func claudeToolEvents(base capture.SessionEvent, tool string, input claudeToolInput) []capture.SessionEvent {
	switch tool {
	case "Edit":
		return []capture.SessionEvent{editEvent(base, input.FilePath, input.OldString, input.NewString)}
	case "Write":
		return []capture.SessionEvent{editEvent(base, input.FilePath, "", input.Content)}
	case "NotebookEdit":
		return []capture.SessionEvent{editEvent(base, firstNonEmpty(input.NotebookID, input.FilePath), input.OldString, input.NewString)}
	case "MultiEdit":
		events := make([]capture.SessionEvent, 0, len(input.Edits))
		for _, edit := range input.Edits {
			events = append(events, editEvent(base, input.FilePath, edit.OldString, edit.NewString))
		}
		return events
	case "Read":
		if strings.TrimSpace(input.FilePath) == "" {
			return nil
		}
		event := base
		event.Kind = capture.KindRead
		event.FilePath = input.FilePath
		return []capture.SessionEvent{event}
	case "Bash":
		// Claude Code writes files through Bash constantly - heredocs, sed,
		// redirection - and in some configurations does so in preference to
		// the Edit tool. Those writes are real edits and have to be seen.
		return shellWriteEvents(base, input.Command, base.Raw)
	default:
		return nil
	}
}

func editEvent(base capture.SessionEvent, file, oldText, newText string) capture.SessionEvent {
	event := base
	event.Kind = capture.KindEdit
	event.FilePath = strings.TrimSpace(file)
	event.OldText = oldText
	event.NewText = newText
	return event
}

// shellWriteEvents turns a shell command's plausible writes into edit events.
// They carry no text, so only the temporal tier can act on them - the exact and
// fuzzy tiers have nothing to compare and will not claim a false match.
func shellWriteEvents(base capture.SessionEvent, command string, raw map[string]json.RawMessage) []capture.SessionEvent {
	writes := ShellWrites(StripHeredocs(command))
	events := make([]capture.SessionEvent, 0, len(writes))
	for _, file := range writes {
		event := base
		event.Kind = capture.KindEdit
		event.FilePath = resolvePath(file, cwdOf(raw))
		events = append(events, event)
	}
	return events
}

func decodeBlocks(content json.RawMessage) ([]claudeBlock, error) {
	if len(content) == 0 {
		return nil, nil
	}
	var blocks []claudeBlock
	if err := json.Unmarshal(content, &blocks); err == nil {
		return blocks, nil
	}
	// A plain-string content body carries no tool calls.
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return []claudeBlock{{Type: "text", Text: text}}, nil
	}
	return nil, nil
}

func firstText(content json.RawMessage) string {
	blocks, _ := decodeBlocks(content)
	for _, block := range blocks {
		if strings.TrimSpace(block.Text) != "" {
			return block.Text
		}
	}
	return ""
}

func millis(timestamp string) int64 {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(timestamp))
	if err != nil {
		return 0
	}
	return parsed.UnixMilli()
}
