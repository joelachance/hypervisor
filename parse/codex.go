package parse

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/satoricorp/demux/capture"
)

// Codex parses Codex CLI rollout logs.
//
// Codex writes JSONL under ~/.codex/sessions bucketed by date. Every record is
// a {type, timestamp, payload} envelope; session_meta carries the session id
// and cwd, and response_item carries the model's calls.
//
// Codex edits files two ways. apply_patch is its structured edit tool, a
// custom_tool_call whose input is a patch in Codex's own dialect, and it
// carries the written text the way a Claude Edit does. exec_command is a
// shell, and a write made there is only as visible as the command is
// readable. A Codex session always registers and is always selectable; what
// varies is how much of its work hv can pin to specific hunks.
//
// The working directory lives on session_meta and turn_context, not on the
// calls, so the last one seen per log file is remembered for resolving the
// relative paths a call may use.
type Codex struct {
	roots []string
	mu    sync.Mutex
	cwds  map[string]string
}

// NewCodex returns the Codex parser.
func NewCodex() *Codex {
	return &Codex{roots: []string{home(".codex", "sessions")}}
}

// Tool implements Parser.
func (c *Codex) Tool() string { return capture.ToolCodex }

// Roots implements Parser.
func (c *Codex) Roots() []string { return append([]string(nil), c.roots...) }

// Owns implements Parser.
func (c *Codex) Owns(path string) bool {
	return ownedByRoot(path, c.roots, ".jsonl")
}

type codexRecord struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

type codexPayload struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	ID        string `json:"id"`
	CWD       string `json:"cwd"`
	Model     string `json:"model"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	// Input is a custom_tool_call's argument, which for apply_patch is the
	// patch text itself.
	Input string `json:"input"`
	// Codex records the conversation as event_msg records with a plain
	// message field, which is the whole transcript in two lines of parsing.
	Message string `json:"message"`
}

type codexExecArguments struct {
	Command string `json:"cmd"`
	Workdir string `json:"workdir"`
}

// ParseLine implements Parser.
//
// The session id and cwd appear only on the session_meta record at the head of
// the file, so they are recovered from the file name and each call's workdir
// respectively - a rollout file is one session, and its name embeds the uuid.
func (c *Codex) ParseLine(path string, line []byte) ([]capture.SessionEvent, error) {
	var record codexRecord
	if err := json.Unmarshal(line, &record); err != nil {
		return nil, malformed(path, err)
	}
	var payload codexPayload
	if len(record.Payload) > 0 {
		_ = json.Unmarshal(record.Payload, &payload)
	}
	sessionID := firstNonEmpty(codexSessionIDFromPath(path), payload.SessionID)
	if sessionID == "" && record.Type == "session_meta" {
		// Only session_meta's id is the session's; a response_item's id
		// names the message.
		sessionID = payload.ID
	}
	if sessionID == "" {
		return nil, nil
	}
	cwd := c.remember(path, payload.CWD)
	base := capture.SessionEvent{
		SessionID: sessionID,
		Tool:      capture.ToolCodex,
		TS:        millis(record.Timestamp),
		Model:     strings.TrimSpace(payload.Model),
		Raw:       rawContext(cwd),
	}

	switch record.Type {
	case "event_msg":
		// user_message and agent_message are the conversation itself; the
		// other event kinds are progress and token counts.
		role := ""
		switch payload.Type {
		case "user_message":
			role = RoleUser
		case "agent_message":
			role = RoleAssistant
		default:
			return nil, nil
		}
		if strings.TrimSpace(payload.Message) == "" {
			return nil, nil
		}
		base.Kind = capture.KindMessage
		base.PromptContext = payload.Message
		base.Raw = withRole(base.Raw, role)
		return []capture.SessionEvent{base}, nil
	case "session_meta", "turn_context":
		// Registers the session and pins its worktree and model, even before
		// any call. turn_context repeats both every turn, which is also
		// where a directory change shows up.
		base.Kind = capture.KindMessage
		return []capture.SessionEvent{base}, nil
	case "response_item":
		switch {
		case payload.Type == "function_call" && payload.Name == "exec_command":
			var arguments codexExecArguments
			if err := json.Unmarshal([]byte(payload.Arguments), &arguments); err != nil {
				return nil, nil
			}
			if strings.TrimSpace(arguments.Workdir) != "" {
				base.Raw = rawContext(arguments.Workdir)
			}
			events := shellWriteEvents(base, arguments.Command, base.Raw)
			if len(events) == 0 {
				// A command that wrote nothing still proves the session is
				// alive.
				base.Kind = capture.KindToolCall
				return []capture.SessionEvent{base}, nil
			}
			return events, nil
		case payload.Type == "custom_tool_call" && payload.Name == "apply_patch":
			edits := ApplyPatchEdits(payload.Input)
			if len(edits) == 0 {
				base.Kind = capture.KindToolCall
				return []capture.SessionEvent{base}, nil
			}
			events := make([]capture.SessionEvent, 0, len(edits))
			for _, edit := range edits {
				event := base
				event.Kind = capture.KindEdit
				event.FilePath = resolvePath(edit.Path, cwd)
				event.OldText = edit.OldText
				event.NewText = edit.NewText
				events = append(events, event)
			}
			return events, nil
		}
		return nil, nil
	default:
		return nil, nil
	}
}

// remember keeps the last working directory a log declared, and returns the
// one in force: the record's own when it has one, the remembered one
// otherwise.
func (c *Codex) remember(path, cwd string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cwds == nil {
		c.cwds = map[string]string{}
	}
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		c.cwds[path] = cwd
		return cwd
	}
	return c.cwds[path]
}

// codexSessionIDFromPath pulls the uuid out of a rollout file name, which is
// shaped rollout-<timestamp>-<uuid>.jsonl.
func codexSessionIDFromPath(path string) string {
	base := strings.TrimSuffix(pathBase(path), ".jsonl")
	parts := strings.Split(base, "-")
	if len(parts) < 5 {
		return ""
	}
	return strings.Join(parts[len(parts)-5:], "-")
}
