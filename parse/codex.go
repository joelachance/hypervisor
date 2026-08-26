package parse

import (
	"encoding/json"
	"strings"

	"github.com/satoricorp/demux/capture"
)

// Codex parses Codex CLI rollout logs.
//
// Codex writes JSONL under ~/.codex/sessions bucketed by date. Every record is
// a {type, timestamp, payload} envelope; session_meta carries the session id
// and cwd, and response_item carries the model's calls.
//
// Codex has no structured edit tool. Every file it writes goes through
// exec_command - a shell - so file attribution depends entirely on reading
// those commands, and is best-effort by nature. A Codex session always
// registers and is always selectable; what varies is how much of its work hv
// can pin to specific hunks.
type Codex struct{ roots []string }

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
	CWD       string `json:"cwd"`
	Model     string `json:"model"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
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
	sessionID := firstNonEmpty(payload.SessionID, codexSessionIDFromPath(path))
	if sessionID == "" {
		return nil, nil
	}
	base := capture.SessionEvent{
		SessionID: sessionID,
		Tool:      capture.ToolCodex,
		TS:        millis(record.Timestamp),
		Model:     strings.TrimSpace(payload.Model),
		Raw:       rawContext(payload.CWD),
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
	case "session_meta":
		// Registers the session and pins its worktree, even before any call.
		base.Kind = capture.KindMessage
		return []capture.SessionEvent{base}, nil
	case "response_item":
		if payload.Type != "function_call" || payload.Name != "exec_command" {
			return nil, nil
		}
		var arguments codexExecArguments
		if err := json.Unmarshal([]byte(payload.Arguments), &arguments); err != nil {
			return nil, nil
		}
		if base.Raw == nil {
			base.Raw = rawContext(arguments.Workdir)
		}
		events := shellWriteEvents(base, arguments.Command, base.Raw)
		if len(events) == 0 {
			// A command that wrote nothing still proves the session is alive.
			base.Kind = capture.KindToolCall
			return []capture.SessionEvent{base}, nil
		}
		return events, nil
	default:
		return nil, nil
	}
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
