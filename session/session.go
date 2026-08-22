// Package session defines the selectable unit hv works in: one agent coding
// session, summarized from the events its tool logged.
//
// A Session is derived state. The authority is the event stream (see package
// parse); this package only indexes it into something a person can look at and
// choose from.
package session

import (
	"sort"
	"strings"

	"github.com/satoricorp/demux/capture"
	"github.com/satoricorp/demux/redact"
	"github.com/satoricorp/hv/parse"
)

// State is how alive a session is, judged by how recently it wrote to its log.
type State string

const (
	// StateLive means the session wrote within LiveWindow - an agent is
	// probably working in it right now.
	StateLive State = "live"
	// StateIdle means it wrote within IdleWindow: still open, between turns.
	StateIdle State = "idle"
	// StateEnded means it has been quiet longer than that.
	StateEnded State = "ended"
)

// The windows separating those states. A session log is written on every turn,
// so silence is the only liveness signal available without asking the tool.
const (
	LiveWindow = 3 * 60 * 1000      // 3 minutes in milliseconds
	IdleWindow = 2 * 60 * 60 * 1000 // 2 hours in milliseconds
)

// Session is one agent coding session, summarized for selection.
type Session struct {
	// ID is the tool's own session identifier, unique per tool.
	ID string `json:"id"`
	// Tool is a capture.Tool* constant: claude, codex, or cursor.
	Tool string `json:"tool"`
	// Model is the last model observed on an event in this session.
	Model string `json:"model,omitempty"`
	// RepoRoot is the worktree the session edited, when it can be resolved.
	RepoRoot string `json:"repo_root,omitempty"`
	// LogPath is the JSONL file the session was read from.
	LogPath string `json:"log_path,omitempty"`
	// StartedAt and LastActiveAt are unix milliseconds.
	StartedAt    int64 `json:"started_at"`
	LastActiveAt int64 `json:"last_active_at"`
	// EventCount is every event; EditCount only the file edits.
	EventCount int `json:"event_count"`
	EditCount  int `json:"edit_count"`
	// Files are the paths the session edited, repo-relative when RepoRoot is
	// known, sorted and deduplicated.
	Files []string `json:"files,omitempty"`
	// Summary is a short human label for the session, taken from its first
	// prompt. It is secret-redacted before it is ever carried onto a plan.
	Summary string `json:"summary,omitempty"`
}

// State reports how alive the session is at the given time, in unix millis.
func (s Session) State(now int64) State {
	switch age := now - s.LastActiveAt; {
	case age <= LiveWindow:
		return StateLive
	case age <= IdleWindow:
		return StateIdle
	default:
		return StateEnded
	}
}

// Active reports whether the session was touched within the last window
// milliseconds relative to now.
func (s Session) Active(now, window int64) bool {
	return now-s.LastActiveAt <= window
}

// Touched reports whether the session edited the given path.
func (s Session) Touched(file string) bool {
	file = strings.TrimSpace(file)
	for _, candidate := range s.Files {
		if candidate == file {
			return true
		}
	}
	return false
}

// Index accumulates session events into Session records.
//
// It is not safe for concurrent use; the watcher owns one and publishes
// snapshots.
type Index struct {
	sessions map[string]*Session
	files    map[string]map[string]struct{}
}

// NewIndex returns an empty index.
func NewIndex() *Index {
	return &Index{
		sessions: map[string]*Session{},
		files:    map[string]map[string]struct{}{},
	}
}

// Observe folds one event into the index, creating the session if it is new.
func (i *Index) Observe(event capture.SessionEvent) {
	id := strings.TrimSpace(event.SessionID)
	if id == "" {
		return
	}
	current, ok := i.sessions[id]
	if !ok {
		current = &Session{ID: id, Tool: event.Tool, StartedAt: event.TS}
		i.sessions[id] = current
		i.files[id] = map[string]struct{}{}
	}
	if event.TS > current.LastActiveAt {
		current.LastActiveAt = event.TS
	}
	if event.TS < current.StartedAt || current.StartedAt == 0 {
		current.StartedAt = event.TS
	}
	if model := strings.TrimSpace(event.Model); model != "" {
		current.Model = model
	}
	if root := strings.TrimSpace(parse.CWD(event)); root != "" && current.RepoRoot == "" {
		current.RepoRoot = root
	}
	if current.Summary == "" && event.Kind == capture.KindMessage {
		current.Summary = summarize(event.PromptContext)
	}
	current.EventCount++
	if event.IsEditEvent() {
		current.EditCount++
		if file := strings.TrimSpace(event.FilePath); file != "" {
			i.files[id][file] = struct{}{}
		}
	}
}

// summarize turns a prompt into a one-line label. It redacts first: session
// text rides onto plans, and a leaked key there outlives the session.
func summarize(prompt string) string {
	prompt = strings.TrimSpace(redact.Redact(prompt))
	if prompt == "" {
		return ""
	}
	if index := strings.IndexByte(prompt, '\n'); index >= 0 {
		prompt = prompt[:index]
	}
	prompt = strings.Join(strings.Fields(prompt), " ")
	const limit = 72
	if len(prompt) > limit {
		return prompt[:limit-1] + "\u2026"
	}
	return prompt
}

// Get returns one session by ID.
func (i *Index) Get(id string) (Session, bool) {
	current, ok := i.sessions[strings.TrimSpace(id)]
	if !ok {
		return Session{}, false
	}
	return i.snapshot(current), true
}

// Sessions returns every known session, most recently active first.
func (i *Index) Sessions() []Session {
	out := make([]Session, 0, len(i.sessions))
	for _, current := range i.sessions {
		out = append(out, i.snapshot(current))
	}
	sort.Slice(out, func(left, right int) bool {
		if out[left].LastActiveAt != out[right].LastActiveAt {
			return out[left].LastActiveAt > out[right].LastActiveAt
		}
		return out[left].ID < out[right].ID
	})
	return out
}

func (i *Index) snapshot(current *Session) Session {
	out := *current
	files := make([]string, 0, len(i.files[current.ID]))
	for file := range i.files[current.ID] {
		files = append(files, file)
	}
	sort.Strings(files)
	out.Files = files
	return out
}
