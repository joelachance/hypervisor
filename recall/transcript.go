// Package recall reads a session's transcript on demand and reduces it to
// something small enough to carry around: what the session set out to do, what
// it concluded, and which files it touched.
//
// The architecture is lifted from hypervisor (src-tauri/src/transcript.rs and
// history.rs), which solves the same problem for the same logs:
//
//   - transcript parsing is deliberately *not* part of the watch loop. The
//     sweep stays cheap because it only ever reads timestamps and file paths;
//     the conversation is read for one session at a time, when something
//     actually needs it.
//   - summaries are extractive, never generated. The last substantive
//     assistant message is the outcome and the edited paths are the files.
//     No model call, so it always works, costs nothing, and the content never
//     leaves the machine.
//
// hypervisor keeps its summaries in SQLite; hv keeps them in a JSONL file, to
// stay cgo-free and for the same reason its apply journal is a file - one
// append per session, read whole, no schema to migrate.
package recall

import (
	"regexp"
	"strings"

	"github.com/satoricorp/demux/capture"
	"github.com/satoricorp/demux/redact"
	"github.com/satoricorp/hv/parse"
	"github.com/satoricorp/hv/session"
)

// Kind is what a transcript item is.
type Kind string

const (
	KindUser      Kind = "user"
	KindAssistant Kind = "assistant"
	KindTool      Kind = "tool"
)

// Item is one entry in a session's conversation.
type Item struct {
	Kind Kind   `json:"kind"`
	Text string `json:"text,omitempty"`
	// Tool items carry what was run and on what.
	Tool string `json:"tool,omitempty"`
	File string `json:"file,omitempty"`
	TS   int64  `json:"ts,omitempty"`
}

// clipItem caps one item. A single file read can be a megabyte, and none of
// this is worth carrying at that size.
const clipItem = 4000

// Read parses one session's transcript from its log.
//
// limit caps the returned tail; zero means the last 400 items, which is what
// hypervisor settled on and covers any conversation someone would review.
func Read(current session.Session, limit int) ([]Item, error) {
	if limit <= 0 {
		limit = 400
	}
	path := strings.TrimSpace(current.LogPath)
	if path == "" {
		return nil, nil
	}
	events, _, err := parse.Default().File(path)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(events))
	for _, event := range events {
		if event.SessionID != "" && event.SessionID != current.ID {
			continue
		}
		if item, ok := itemFor(event); ok {
			items = append(items, item)
		}
	}
	if len(items) > limit {
		items = items[len(items)-limit:]
	}
	return items, nil
}

// noisePrefixes are the wrappers a harness puts in the transcript that are not
// part of the conversation: slash-command plumbing, hook output, injected
// reminders. They are recorded as user messages, so without this the first
// thing a session appears to have been asked is "Caveat: the messages below
// were generated while running local commands".
var noisePrefixes = []string{
	"<command-name>", "<command-message>", "<command-args>",
	"<local-command-stdout>", "<local-command-stderr>", "<local-command-caveat>",
	"<user-prompt-submit-hook>", "<system-reminder>", "<bash-input>", "<bash-stdout>",
	"caveat: the messages below",
}

// reminderBlock is an injected note appended to a real message; the message is
// worth keeping, the note is not.
var reminderBlock = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

// isNoise reports whether a message is transcript plumbing rather than
// something a person or an agent said.
func isNoise(text string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(text))
	if trimmed == "" {
		return true
	}
	for _, prefix := range noisePrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

// clean strips injected blocks from a message that is otherwise real.
func clean(text string) string {
	return strings.TrimSpace(reminderBlock.ReplaceAllString(text, ""))
}

func itemFor(event capture.SessionEvent) (Item, bool) {
	switch event.Kind {
	case capture.KindMessage:
		body := clean(event.PromptContext)
		if isNoise(body) {
			return Item{}, false
		}
		// Kept with its line breaks: a message is paragraphs and lists, and
		// flattening it to one line is what made transcripts unreadable.
		text := clipLines(redact.Redact(body), clipItem)
		if text == "" {
			return Item{}, false
		}
		kind := KindUser
		if parse.Role(event) == parse.RoleAssistant {
			kind = KindAssistant
		}
		return Item{Kind: kind, Text: text, TS: event.TS}, true
	case capture.KindEdit:
		return Item{Kind: KindTool, Tool: "edit", File: event.FilePath, TS: event.TS}, true
	case capture.KindRead:
		return Item{Kind: KindTool, Tool: "read", File: event.FilePath, TS: event.TS}, true
	default:
		return Item{}, false
	}
}

// Summary is one session reduced to a line, plus the files behind it.
//
// It is the unit of cross-repo memory: small enough to hold hundreds of them,
// specific enough to tell whether a past session is relevant to this one.
type Summary struct {
	SessionID string   `json:"session_id"`
	Tool      string   `json:"tool"`
	Repo      string   `json:"repo"`
	Title     string   `json:"title"`
	Intent    string   `json:"intent,omitempty"`
	Outcome   string   `json:"outcome,omitempty"`
	Files     []string `json:"files,omitempty"`
	Line      string   `json:"line"`
	At        int64    `json:"at"`
}

// Summarize reduces a session and its transcript to a Summary, extractively.
//
// Intent is the first thing the person asked for and Outcome the last thing
// the agent said - between them they describe a session better than either
// alone, and both are already redacted.
func Summarize(current session.Session, items []Item) Summary {
	summary := Summary{
		SessionID: current.ID,
		Tool:      current.Tool,
		Repo:      current.RepoRoot,
		Title:     clip(current.Label(), 80),
		At:        current.LastActiveAt,
	}
	files := []string{}
	seen := map[string]struct{}{}
	for _, item := range items {
		switch item.Kind {
		case KindUser:
			if summary.Intent == "" {
				summary.Intent = clip(item.Text, 160)
			}
		case KindAssistant:
			if strings.TrimSpace(item.Text) != "" {
				summary.Outcome = clip(item.Text, 160)
			}
		case KindTool:
			if item.Tool != "edit" || item.File == "" {
				continue
			}
			if _, ok := seen[item.File]; ok || len(files) >= 8 {
				continue
			}
			seen[item.File] = struct{}{}
			files = append(files, item.File)
		}
	}
	if len(files) == 0 {
		files = current.Files
		if len(files) > 8 {
			files = files[:8]
		}
	}
	summary.Files = files
	summary.Line = line(summary)
	return summary
}

func line(summary Summary) string {
	parts := []string{summary.Title}
	if summary.Intent != "" {
		parts = append(parts, "— "+summary.Intent)
	}
	if summary.Outcome != "" {
		parts = append(parts, "→ "+summary.Outcome)
	}
	if len(summary.Files) > 0 {
		parts = append(parts, "· files: "+clip(strings.Join(summary.Files, ", "), 120))
	}
	return strings.Join(parts, " ")
}

// clipLines caps length while keeping newlines, for text that will be
// displayed as written rather than summarized into a line.
func clipLines(text string, max int) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max-1]) + "…"
}

// clip collapses whitespace and caps length, counting runes so a multi-byte
// character is never cut in half.
func clip(text string, max int) string {
	flat := strings.Join(strings.Fields(text), " ")
	runes := []rune(flat)
	if len(runes) <= max {
		return flat
	}
	return string(runes[:max-1]) + "…"
}
