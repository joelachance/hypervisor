package parse

import (
	"regexp"
	"strings"
)

// Both Claude Code and Codex do a large share of their file writing through a
// shell tool rather than a structured edit tool - Codex does *all* of it that
// way. A session whose edits are invisible is a session hv cannot attribute, so
// the shell command itself has to be read.
//
// This reads conservatively. It recognizes only unambiguous write forms and
// returns the paths they target; anything clever (a script that writes paths it
// computes, a command substitution) is missed on purpose. A missed write costs
// attribution for that hunk. A wrong one attributes a person's work to an
// agent, which is worse.
var shellWritePatterns = []*regexp.Regexp{
	// redirection: > path, >> path (not >&2, not >(...))
	regexp.MustCompile(`>>?\s+([^\s|;&<>()]+)`),
	// tee path, tee -a path
	regexp.MustCompile(`\btee\s+(?:-a\s+)?([^\s|;&<>()-][^\s|;&<>()]*)`),
	// sed -i ... path
	regexp.MustCompile(`\bsed\s+(?:-[a-zA-Z]*i[a-zA-Z]*\s+(?:''\s+|"" \s+)?)(?:-e\s+\S+\s+)*(?:\S+\s+)?([^\s|;&<>()]+)$`),
	// cp/mv/install destination is the final argument
	regexp.MustCompile(`\b(?:cp|mv|install)\s+(?:-\S+\s+)*\S+\s+([^\s|;&<>()]+)`),
	// touch path
	regexp.MustCompile(`\btouch\s+([^\s|;&<>()]+)`),
}

// shellRedirectSkip matches redirections that do not write a file: fd
// duplication, process substitution, and the null device.
var shellRedirectSkip = regexp.MustCompile(`^(&\d+|\(|/dev/)`)

// ShellWrites returns the file paths a shell command plausibly writes.
//
// The result is best-effort by construction. Callers should emit the paths as
// edit evidence with no text attached, so only the weakest matching tier can
// act on them.
func ShellWrites(command string) []string {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}
	seen := map[string]struct{}{}
	out := []string{}
	for _, line := range strings.Split(command, "\n") {
		// A heredoc body is data, not commands: scanning it invents writes
		// out of whatever the document happens to contain.
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, pattern := range shellWritePatterns {
			for _, match := range pattern.FindAllStringSubmatch(line, -1) {
				path := strings.Trim(match[1], `"'`)
				if !plausibleWritePath(path) {
					continue
				}
				if _, ok := seen[path]; ok {
					continue
				}
				seen[path] = struct{}{}
				out = append(out, path)
			}
		}
	}
	return out
}

func plausibleWritePath(path string) bool {
	if path == "" || shellRedirectSkip.MatchString(path) {
		return false
	}
	if strings.ContainsAny(path, "*?$`") {
		// Globs and expansions name no single file.
		return false
	}
	if strings.HasPrefix(path, "/dev/") || path == "/dev/null" {
		return false
	}
	return true
}

// StripHeredocs removes heredoc bodies from a command so the write scanner
// reads only the commands themselves.
func StripHeredocs(command string) string {
	lines := strings.Split(command, "\n")
	out := make([]string, 0, len(lines))
	delimiter := ""
	for _, line := range lines {
		if delimiter != "" {
			if strings.TrimSpace(line) == delimiter {
				delimiter = ""
			}
			continue
		}
		out = append(out, line)
		if match := heredocStart.FindStringSubmatch(line); match != nil {
			delimiter = strings.Trim(match[1], `"'`)
		}
	}
	return strings.Join(out, "\n")
}

var heredocStart = regexp.MustCompile(`<<-?\s*'?"?([A-Za-z_][A-Za-z0-9_]*)'?"?`)
