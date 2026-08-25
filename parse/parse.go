// Package parse normalizes per-tool agent session logs into the
// capture.SessionEvent stream the rest of hv works in.
//
// Each supported tool writes JSONL with its own shape; a Parser is responsible
// for one tool and for exactly one thing: turning a line into zero or more
// normalized events. Nothing downstream should ever branch on Tool again.
package parse

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/satoricorp/demux/capture"
)

// Parser reads one tool's session log format.
type Parser interface {
	// Tool is the capture.Tool* constant this parser handles.
	Tool() string
	// Roots are the directories this tool writes session logs under, absolute
	// and with ~ already expanded. Missing directories are not an error.
	Roots() []string
	// Owns reports whether the parser recognizes a log file by path.
	Owns(path string) bool
	// ParseLine converts one JSONL line into normalized events. A line the
	// parser has no event for yields nil, nil - that is the common case, not
	// a failure.
	ParseLine(path string, line []byte) ([]capture.SessionEvent, error)
}

// Untimed is implemented by parsers whose format carries no per-record
// timestamps. Only for those may a caller substitute the log file's
// modification time; doing it for a timed format would turn "this file was
// touched" into "this session is active", which is a different claim.
type Untimed interface {
	Untimed() bool
}

// IsUntimed reports whether a parser's format lacks record timestamps.
func IsUntimed(parser Parser) bool {
	untimed, ok := parser.(Untimed)
	return ok && untimed.Untimed()
}

// Registry dispatches log files to the parser that owns them.
type Registry struct {
	parsers []Parser
}

// NewRegistry returns a registry over the given parsers.
func NewRegistry(parsers ...Parser) *Registry {
	return &Registry{parsers: parsers}
}

// Default returns a registry with every parser hv ships.
func Default() *Registry {
	return NewRegistry(NewClaude(), NewCodex(), NewCursor())
}

// Parsers returns the registered parsers.
func (r *Registry) Parsers() []Parser {
	return append([]Parser(nil), r.parsers...)
}

// Roots returns every directory any registered parser watches, deduplicated.
func (r *Registry) Roots() []string {
	seen := map[string]struct{}{}
	roots := []string{}
	for _, parser := range r.parsers {
		for _, root := range parser.Roots() {
			root = strings.TrimSpace(root)
			if root == "" {
				continue
			}
			if _, ok := seen[root]; ok {
				continue
			}
			seen[root] = struct{}{}
			roots = append(roots, root)
		}
	}
	sort.Strings(roots)
	return roots
}

// For returns the parser owning a log file path.
func (r *Registry) For(path string) (Parser, bool) {
	for _, parser := range r.parsers {
		if parser.Owns(path) {
			return parser, true
		}
	}
	return nil, false
}

// File parses a whole session log, in order. Lines the owning parser does not
// recognize are skipped; a malformed line is skipped with its error collected
// rather than abandoning the file, because a truncated tail is normal for a
// log that is still being written.
func (r *Registry) File(path string) ([]capture.SessionEvent, []error, error) {
	parser, ok := r.For(path)
	if !ok {
		return nil, nil, fmt.Errorf("no parser owns %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	return ParseReader(parser, path, file)
}

// ParseReader parses an open log stream with a specific parser.
func ParseReader(parser Parser, path string, reader io.Reader) ([]capture.SessionEvent, []error, error) {
	events := []capture.SessionEvent{}
	softErrors := []error{}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLogLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		parsed, err := parser.ParseLine(path, line)
		if err != nil {
			softErrors = append(softErrors, err)
			continue
		}
		events = append(events, parsed...)
	}
	if err := scanner.Err(); err != nil {
		return events, softErrors, err
	}
	return events, softErrors, nil
}

// maxLogLineBytes caps one JSONL record. Agent logs embed whole file contents
// in edit events, so the default bufio limit is far too small.
const maxLogLineBytes = 16 * 1024 * 1024

// HomeEnv overrides the directory session logs are looked for under.
//
// Every tool writes its logs beneath the user's home, which makes hv
// untestable against anything but the real machine and gives no way to try it
// on a copied or synthetic store. Pointing this at another directory moves
// every root at once, so the layout under it stays exactly what the tools
// produce.
const HomeEnv = "HV_HOME"

var homeOverride string

// SetHome points hv at another directory to read session logs under, taking
// precedence over HomeEnv. An empty string restores the default.
func SetHome(dir string) { homeOverride = strings.TrimSpace(dir) }

// Home is the directory hv reads session logs under.
func Home() string {
	if homeOverride != "" {
		return homeOverride
	}
	if override := strings.TrimSpace(os.Getenv(HomeEnv)); override != "" {
		return override
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return dir
}

// IsDefaultHome reports whether hv is reading the real session store. When it
// is not, every listing says so: two stores that look alike but hold different
// sessions is the kind of confusion that wastes an afternoon.
func IsDefaultHome() bool {
	dir, err := os.UserHomeDir()
	return err == nil && Home() == dir
}

// home expands a path relative to Home. An unresolvable home yields an empty
// root, which the walker treats as absent.
func home(parts ...string) string {
	dir := Home()
	if dir == "" {
		return ""
	}
	return filepath.Join(append([]string{dir}, parts...)...)
}

// RawLog is the key under which the watcher stamps the file an event was read
// from, so a session's transcript can be re-read on demand without searching
// every root for it again.
const RawLog = "log"

// LogPath returns the file an event was read from.
func LogPath(event capture.SessionEvent) string { return rawString(event, RawLog) }

func rawString(event capture.SessionEvent, key string) string {
	encoded, ok := event.Raw[key]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(encoded, &value); err != nil {
		return ""
	}
	return value
}

// StampLog records the source file on a batch of events.
func StampLog(events []capture.SessionEvent, path string) {
	encoded, err := json.Marshal(path)
	if err != nil {
		return
	}
	for index := range events {
		if events[index].Raw == nil {
			events[index].Raw = map[string]json.RawMessage{}
		}
		events[index].Raw[RawLog] = encoded
	}
}

// RawOrigin is the key under which parsers stash where a session is running -
// Claude Code records this as "entrypoint": cli for a terminal, claude-desktop
// for the app.
const RawOrigin = "origin"

// Origin returns where a parser recorded the session as running.
func Origin(event capture.SessionEvent) string { return rawString(event, RawOrigin) }

// RawTitle is the key under which parsers stash a session's own title - the
// name its tool shows for it. A title is a property of the session, not of the
// record that happens to announce it, so it travels the same way cwd does.
const RawTitle = "title"

// Title returns the session title a parser recorded on an event.
func Title(event capture.SessionEvent) string { return rawString(event, RawTitle) }

// rawTitle builds the Raw bag carrying a session title.
func rawTitle(title string) map[string]json.RawMessage {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil
	}
	encoded, err := json.Marshal(title)
	if err != nil {
		return nil
	}
	return map[string]json.RawMessage{RawTitle: encoded}
}

// RawCWD is the key under which parsers stash a session's working directory on
// capture.SessionEvent.Raw. The event type is demux's and has no field for it,
// but the worktree is exactly what decides which repo a session belongs to, so
// it travels in the raw bag rather than being dropped.
const RawCWD = "cwd"

// rawContext builds the Raw bag carrying a record's working directory.
func rawContext(cwd string, extras ...[2]string) map[string]json.RawMessage {
	raw := map[string]json.RawMessage{}
	for _, pair := range append([][2]string{{RawCWD, cwd}}, extras...) {
		value := strings.TrimSpace(pair[1])
		if value == "" {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			continue
		}
		raw[pair[0]] = encoded
	}
	if len(raw) == 0 {
		return nil
	}
	return raw
}

// CWD returns the working directory a parser recorded on an event.
func CWD(event capture.SessionEvent) string {
	return cwdOf(event.Raw)
}

func cwdOf(raw map[string]json.RawMessage) string {
	encoded, ok := raw[RawCWD]
	if !ok {
		return ""
	}
	var cwd string
	if err := json.Unmarshal(encoded, &cwd); err != nil {
		return ""
	}
	return cwd
}

// resolvePath makes a recorded path absolute against the session's working
// directory, so a relative shell write and an absolute tool edit compare equal.
func resolvePath(path, cwd string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	// Shell commands say ~/git/x constantly. Left unexpanded it matches no
	// repo prefix, and the session silently looks like it edited nothing.
	if path == "~" || strings.HasPrefix(path, "~/") {
		if dir, err := os.UserHomeDir(); err == nil {
			return filepath.Join(dir, strings.TrimPrefix(path, "~"))
		}
	}
	if filepath.IsAbs(path) || strings.TrimSpace(cwd) == "" {
		return path
	}
	return filepath.Join(cwd, path)
}

// ownedByRoot reports whether a path lies under any root with the given suffix.
func ownedByRoot(path string, roots []string, suffix string) bool {
	if suffix != "" && filepath.Ext(path) != suffix {
		return false
	}
	for _, root := range roots {
		if root != "" && strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func malformed(path string, err error) error {
	return fmt.Errorf("%s: %w", filepath.Base(path), err)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// verifiedPathFromSlug inverts a dash-encoded absolute path.
//
// The encoding replaces every separator with a dash, so a directory whose real
// name contains a dash is ambiguous: "Users-joe-git-yeet-term" is either
// /Users/joe/git/yeet/term or /Users/joe/git/yeet-term. The ambiguity is
// resolved against the filesystem, longest real directory first, and a slug
// that resolves to nothing returns empty rather than a guess.
func verifiedPathFromSlug(slug string) string {
	slug = strings.Trim(strings.TrimSpace(slug), "-")
	if slug == "" {
		return ""
	}
	return resolveSlugParts(string(filepath.Separator), strings.Split(slug, "-"))
}

func resolveSlugParts(base string, parts []string) string {
	if len(parts) == 0 {
		return base
	}
	candidate := ""
	for index, part := range parts {
		if candidate == "" {
			candidate = part
		} else {
			candidate += "-" + part
		}
		next := filepath.Join(base, candidate)
		info, err := os.Stat(next)
		if err != nil || !info.IsDir() {
			continue
		}
		if resolved := resolveSlugParts(next, parts[index+1:]); resolved != "" {
			return resolved
		}
	}
	return ""
}
