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

// Origin is where a session runs. Claude Code records it as "entrypoint".
const (
	OriginCLI = "cli"
	OriginApp = "claude-desktop"
)

// PullRequest is a pull request the desktop app associated with a session.
type PullRequest struct {
	Number  int    `json:"prNumber"`
	URL     string `json:"url"`
	Repo    string `json:"repo"`
	Host    string `json:"host"`
	Branch  string `json:"branch"`
	BaseRef string `json:"baseRef"`
	State   string `json:"state"`
}

// State is whether a session is being worked in right now.
type State string

const (
	// StateActive means the session wrote to its log within ActiveWindow.
	StateActive State = "active"
	// StateStale means it has been quiet longer than that.
	StateStale State = "stale"
)

// ActiveWindow is how recently a session must have written to count as active.
//
// A log is written on every turn, so silence is the only signal available
// without asking the tool - and it cannot separate a session someone still has
// open from one they finished with. Ten minutes is long enough to survive a
// person reading output or thinking, and short enough that yesterday's work
// does not claim to be live.
const ActiveWindow = 10 * 60 * 1000 // milliseconds

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
	// Title is the name the session goes by. The desktop app's registry wins
	// when it has one, because that is the name the person has been looking
	// at - and may have written themselves; otherwise it is the tool's own
	// generated title. TitleSource is "user" or "auto" when known.
	Title       string `json:"title,omitempty"`
	TitleSource string `json:"title_source,omitempty"`
	// Origin is where the session runs: OriginCLI or OriginApp.
	Origin string `json:"origin,omitempty"`
	// AppSessionID is the desktop app's own id for this session, when the app
	// knows about it. It is what a deep link addresses.
	AppSessionID string `json:"app_session_id,omitempty"`
	// Archived sessions are hidden in the app's list; hv hides them too
	// unless asked, since archiving is the person saying they are done.
	Archived bool `json:"archived,omitempty"`
	// Branch is the branch the session wrote to, when the app recorded one.
	Branch string `json:"branch,omitempty"`
	// PRs are the pull requests the app associated with the session - the
	// cheapest available answer to "did this work land?".
	PRs []PullRequest `json:"prs,omitempty"`
	// Summary is a short human label for the session, taken from its first
	// prompt. It is the fallback when the tool supplied no Title, and it is
	// secret-redacted before it is ever carried onto a plan.
	Summary string `json:"summary,omitempty"`
}

// State reports whether the session is active at the given time, in unix
// millis, using ActiveWindow.
func (s Session) State(now int64) State {
	return s.StateWithin(now, ActiveWindow)
}

// StateWithin reports activity against a caller-chosen window.
func (s Session) StateWithin(now, window int64) State {
	if now-s.LastActiveAt <= window {
		return StateActive
	}
	return StateStale
}

// Label is what to call this session: the tool's own title when it has one,
// the first prompt when it does not.
func (s Session) Label() string {
	if title := strings.TrimSpace(s.Title); title != "" {
		return title
	}
	if summary := strings.TrimSpace(s.Summary); summary != "" {
		return summary
	}
	return "(untitled)"
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
	if event.TS > 0 && (event.TS < current.StartedAt || current.StartedAt == 0) {
		current.StartedAt = event.TS
	}
	if model := strings.TrimSpace(event.Model); model != "" {
		current.Model = model
	}
	if root := strings.TrimSpace(parse.CWD(event)); root != "" && current.RepoRoot == "" {
		current.RepoRoot = root
	}
	// A session can be renamed, and the last name is the one its UI shows.
	if title := strings.TrimSpace(parse.Title(event)); title != "" {
		current.Title = title
		current.TitleSource = "auto"
	}
	if origin := strings.TrimSpace(parse.Origin(event)); origin != "" {
		current.Origin = origin
	}
	if log := strings.TrimSpace(parse.LogPath(event)); log != "" {
		current.LogPath = log
	}
	// The first thing the person asked for, not the first thing said: an
	// assistant's opening line describes nothing.
	if current.Summary == "" && event.Kind == capture.KindMessage && parse.Role(event) != parse.RoleAssistant {
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

// AbbreviateMax caps how long a plain prefix may grow before Abbreviate
// switches to eliding the middle. Session ids are random enough that eight
// characters usually separate them; ids sharing a long head are near-identical
// by construction, and spelling out all 36 characters to separate them costs
// every other row its readability.
const AbbreviateMax = 12

// Abbreviate returns a short, unique label for each session id, the way git
// abbreviates commits - except that when ids share a long head it keeps both
// ends and elides the middle ("aaaaaaaa…a001") rather than growing without
// bound or giving up on uniqueness.
//
// A fixed truncation is a trap: sessions started by the same tool in the same
// place can share a long prefix, and a list where three rows read "aaaaaaaa"
// tells the reader nothing and gives them no id they can pass back.
func Abbreviate(sessions []Session, minimum int) map[string]string {
	if minimum < 4 {
		minimum = 4
	}
	ids := make([]string, 0, len(sessions))
	for _, current := range sessions {
		ids = append(ids, current.ID)
	}
	for length := minimum; length <= AbbreviateMax; length++ {
		if labels, ok := labelled(ids, func(id string) string { return prefixOf(id, length) }); ok {
			return labels
		}
	}
	for tail := 4; tail <= 12; tail++ {
		if labels, ok := labelled(ids, func(id string) string { return elide(id, 8, tail) }); ok {
			return labels
		}
	}
	labels, _ := labelled(ids, func(id string) string { return id })
	return labels
}

// labelled applies a labelling function and reports whether it separated every
// distinct id. Two entries for the same id are not a collision.
func labelled(ids []string, label func(string) string) (map[string]string, bool) {
	labels := make(map[string]string, len(ids))
	owner := map[string]string{}
	unique := true
	for _, id := range ids {
		text := label(id)
		if previous, taken := owner[text]; taken && previous != id {
			unique = false
		}
		owner[text] = id
		labels[id] = text
	}
	return labels, unique
}

// elide keeps both ends of an id, which is where near-identical ids differ.
func elide(id string, head, tail int) string {
	if len(id) <= head+tail+1 {
		return id
	}
	return id[:head] + "\u2026" + id[len(id)-tail:]
}

func prefixOf(id string, length int) string {
	if len(id) <= length {
		return id
	}
	return id[:length]
}
