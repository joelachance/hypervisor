package recall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/satoricorp/hv/parse"
)

// Store is the cross-repo memory: every session hv has summarized, in one
// place, regardless of which repository it worked in.
//
// hypervisor puts this in SQLite (app_data_dir/history.db) and retrieves
// same-repo-first plus keyword; the retrieval shape here is the same and the
// storage is a JSONL file, appended once per session and read whole. Hundreds
// of one-line summaries cost nothing to scan, and it keeps hv cgo-free.
type Store struct{ path string }

// DefaultPath is where the memory lives. It follows parse.Home, so pointing hv
// at a synthetic store moves its memory too rather than mixing real sessions
// with test ones.
func DefaultPath() string {
	home := parse.Home()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".hv", "recall.jsonl")
}

// Open returns the store at path, creating its directory. An empty path uses
// DefaultPath.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultPath()
	}
	if path == "" {
		return nil, os.ErrInvalid
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return &Store{path: path}, nil
}

// Put records a summary, superseding any earlier one for the same session.
func (s *Store) Put(summary Summary) error {
	body, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(body, '\n'))
	return err
}

// All returns every remembered session, most recent first, with only the
// newest record for each session id.
func (s *Store) All() ([]Summary, error) {
	body, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	latest := map[string]Summary{}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var summary Summary
		if err := json.Unmarshal([]byte(line), &summary); err != nil {
			continue
		}
		latest[summary.SessionID] = summary
	}
	out := make([]Summary, 0, len(latest))
	for _, summary := range latest {
		out = append(out, summary)
	}
	sort.Slice(out, func(left, right int) bool { return out[left].At > out[right].At })
	return out, nil
}

// SameRepo returns what hv remembers about a repository, newest first.
//
// Same-repo is the first cut for a reason: a past session in this repository
// is far more likely to matter than a better-worded one somewhere else.
func (s *Store) SameRepo(repo, excludeSessionID string, limit int) ([]Summary, error) {
	all, err := s.All()
	if err != nil {
		return nil, err
	}
	repo = strings.TrimSuffix(strings.TrimSpace(repo), "/")
	out := []Summary{}
	for _, summary := range all {
		if repo != "" && strings.TrimSuffix(summary.Repo, "/") != repo {
			continue
		}
		if summary.SessionID == excludeSessionID {
			continue
		}
		out = append(out, summary)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Search matches summaries by keyword across every repository, newest first.
func (s *Store) Search(query string, limit int) ([]Summary, error) {
	all, err := s.All()
	if err != nil {
		return nil, err
	}
	terms := strings.Fields(strings.ToLower(query))
	out := []Summary{}
	for _, summary := range all {
		haystack := strings.ToLower(summary.Line + " " + summary.Repo + " " + strings.Join(summary.Files, " "))
		matched := true
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		out = append(out, summary)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Touching returns remembered sessions that edited any of the given files,
// newest first. This is the retrieval a conflict needs: not "what happened in
// this repo" but "who else has been in these exact files".
func (s *Store) Touching(files []string, excludeSessionID string, limit int) ([]Summary, error) {
	all, err := s.All()
	if err != nil {
		return nil, err
	}
	wanted := map[string]struct{}{}
	for _, file := range files {
		wanted[strings.TrimSpace(file)] = struct{}{}
		wanted[filepath.Base(strings.TrimSpace(file))] = struct{}{}
	}
	out := []Summary{}
	for _, summary := range all {
		if summary.SessionID == excludeSessionID {
			continue
		}
		hit := false
		for _, file := range summary.Files {
			if _, ok := wanted[file]; ok {
				hit = true
				break
			}
			if _, ok := wanted[filepath.Base(file)]; ok {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		out = append(out, summary)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}
