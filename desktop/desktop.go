// Package desktop reads the Claude desktop app's own session registry.
//
// The app keeps one JSON file per session under
// ~/Library/Application Support/Claude/claude-code-sessions/<account>/<org>/,
// and that file - not the transcript - is the source of truth for what the app
// shows: the session's name, whether it has been archived, the branch it wrote,
// and the pull requests it opened. Titles there can be user-authored, which no
// amount of reading transcripts will ever recover.
//
// Every field is optional and the whole registry is optional: a machine with
// no desktop app simply has none, which is not an error.
package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/satoricorp/hv/session"
)

// Record is one session as the desktop app knows it.
type Record struct {
	// AppSessionID is the app's own id ("local_<uuid>"); CLISessionID is the
	// transcript's, which is what joins this to a parsed session.
	AppSessionID string `json:"sessionId"`
	CLISessionID string `json:"cliSessionId"`
	// Title is what the app displays. TitleSource is "user" when a person
	// named it and "auto" when the app did.
	Title       string `json:"title"`
	TitleSource string `json:"titleSource"`
	// Archived sessions are hidden in the app's list.
	Archived bool `json:"isArchived"`
	// Branch is the branch the session worked on, when it made one.
	Branch string `json:"branch"`
	CWD    string `json:"cwd"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
	// LastActivityAt and LastFocusedAt are unix milliseconds.
	LastActivityAt int64 `json:"lastActivityAt"`
	LastFocusedAt  int64 `json:"lastFocusedAt"`
	// PRs are the pull requests the app associated with this session.
	PRs []session.PullRequest `json:"prs"`
}

// UserNamed reports whether a person chose this session's title.
func (r Record) UserNamed() bool { return strings.EqualFold(r.TitleSource, "user") }

// Root is the directory the app keeps its session registry in.
func Root() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "Claude", "claude-code-sessions")
}

// Load reads the registry, keyed by CLI session id.
//
// A missing registry yields an empty map and no error - not every machine runs
// the desktop app - and one unreadable file never fails the rest.
func Load() (map[string]Record, error) { return LoadFrom(Root()) }

// LoadFrom reads a registry rooted at an explicit directory.
func LoadFrom(root string) (map[string]Record, error) {
	records := map[string]Record{}
	if root == "" {
		return records, nil
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", "*", "local_*.json"))
	if err != nil {
		return records, err
	}
	for _, path := range matches {
		record, ok := loadRecord(path)
		if !ok {
			continue
		}
		key := strings.TrimSpace(record.CLISessionID)
		if key == "" {
			continue
		}
		// A session resumed more than once has several registry entries; the
		// most recently active one is what the app shows.
		if existing, ok := records[key]; ok && existing.LastActivityAt > record.LastActivityAt {
			continue
		}
		records[key] = record
	}
	return records, nil
}

func loadRecord(path string) (Record, bool) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Record{}, false
	}
	var record Record
	if err := json.Unmarshal(body, &record); err != nil {
		return Record{}, false
	}
	return record, true
}

// Apply overlays registry facts onto parsed sessions.
//
// The app's title wins over the transcript's. A person's own name for a
// session is the only name they will recognize, and even the app's automatic
// title is the one they have been looking at.
func Apply(sessions []session.Session, records map[string]Record) []session.Session {
	out := make([]session.Session, 0, len(sessions))
	for _, current := range sessions {
		record, ok := records[current.ID]
		if !ok {
			out = append(out, current)
			continue
		}
		if title := strings.TrimSpace(record.Title); title != "" {
			current.Title = title
			current.TitleSource = record.TitleSource
		}
		current.Archived = record.Archived
		current.PRs = record.PRs
		if branch := strings.TrimSpace(record.Branch); branch != "" {
			current.Branch = branch
		}
		if current.RepoRoot == "" {
			current.RepoRoot = record.CWD
		}
		current.AppSessionID = record.AppSessionID
		out = append(out, current)
	}
	return out
}
