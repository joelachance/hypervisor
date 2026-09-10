package parse

import (
	"strings"
)

// Both Claude Code and Codex do a share of their file writing through a shell
// tool rather than a structured edit tool. A session whose edits are invisible
// is a session hv cannot attribute, so the shell command itself has to be read.
//
// This reads conservatively. It recognizes only unambiguous write forms and
// returns the paths they target; anything clever (a script that writes paths it
// computes, a command substitution) is missed on purpose. A missed write costs
// attribution for that hunk. A wrong one attributes a person's work to an
// agent, which is worse - and the wrong ones used to be common: a `>` inside a
// commit message, an awk program, or a JavaScript arrow read as a redirect,
// and `pip install requests numpy` read as a write to `numpy`. So the line is
// split into shell words first, honouring quotes, and only then are the
// operators and command names read.

// word is one shell word after quote removal, or an unquoted operator.
type word struct {
	text string
	// op marks an operator: | ; & && || > >> < << ( ) and the fd-prefixed
	// forms like 2> and &>.
	op bool
}

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
	add := func(path string) {
		if !plausibleWritePath(path) {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	for _, line := range joinContinuations(strings.Split(command, "\n")) {
		for _, segment := range segments(splitWords(line)) {
			for _, path := range segmentWrites(segment) {
				add(path)
			}
		}
	}
	return out
}

// segmentWrites reads one simple command: its redirections, then the command
// itself when it is one of the few that write by name.
func segmentWrites(segment []word) []string {
	paths := []string{}
	argv := []string{}
	for index := 0; index < len(segment); index++ {
		current := segment[index]
		if !current.op {
			argv = append(argv, current.text)
			continue
		}
		if index+1 >= len(segment) || segment[index+1].op {
			continue
		}
		switch {
		case isFileRedirect(current.text):
			paths = append(paths, segment[index+1].text)
			index++
		case isHeredoc(current.text):
			// The word after << is the delimiter, not a file.
			index++
		}
	}
	paths = append(paths, commandWrites(argv)...)
	return paths
}

// isFileRedirect reports whether an operator sends output to a file: >, >>,
// 2>, &>, but not >&2 (an fd), >( (process substitution), or any input form.
func isFileRedirect(op string) bool {
	if !strings.Contains(op, ">") || strings.Contains(op, "<") {
		return false
	}
	return strings.HasSuffix(op, ">") || strings.HasSuffix(op, ">|")
}

// isHeredoc reports whether an operator opens a heredoc (<< or <<-), as
// opposed to a here-string (<<<) or plain input.
func isHeredoc(op string) bool {
	return strings.Contains(op, "<<") && !strings.Contains(op, "<<<")
}

// commandWrites reads the commands that write to a file named as an argument.
func commandWrites(argv []string) []string {
	argv = stripPrefixWords(argv)
	if len(argv) == 0 {
		return nil
	}
	name := argv[0]
	if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
		name = name[slash+1:]
	}
	args := argv[1:]
	switch name {
	case "tee", "touch":
		return operands(args, nil)
	case "sed":
		return sedWrites(args)
	case "cp", "mv", "install":
		return copyDestination(args)
	}
	return nil
}

// stripPrefixWords drops what precedes the command proper: environment
// assignments and the wrappers that take a command as their argument.
func stripPrefixWords(argv []string) []string {
	for len(argv) > 0 {
		head := argv[0]
		switch {
		case isAssignment(head):
			argv = argv[1:]
		case head == "sudo" || head == "env" || head == "command" || head == "exec" || head == "nohup" || head == "time":
			argv = argv[1:]
			// Their own options are dropped too.
			for len(argv) > 0 && strings.HasPrefix(argv[0], "-") {
				argv = argv[1:]
			}
		default:
			return argv
		}
	}
	return argv
}

func isAssignment(text string) bool {
	index := strings.IndexByte(text, '=')
	if index <= 0 {
		return false
	}
	for _, r := range text[:index] {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// operands is the non-option arguments, skipping the value of any option
// listed in takesValue.
func operands(args []string, takesValue map[string]bool) []string {
	out := []string{}
	for index := 0; index < len(args); index++ {
		current := args[index]
		if current == "--" {
			out = append(out, args[index+1:]...)
			break
		}
		if strings.HasPrefix(current, "-") && current != "-" {
			if takesValue[current] {
				index++
			}
			continue
		}
		out = append(out, current)
	}
	return out
}

// sedWrites is the files an in-place sed edits. Without -i sed writes to
// stdout, which the redirect rule covers.
func sedWrites(args []string) []string {
	inPlace := false
	scriptGiven := false
	for index := 0; index < len(args); index++ {
		current := args[index]
		switch {
		case current == "--in-place" || strings.HasPrefix(current, "--in-place="):
			inPlace = true
		case current == "-e" || current == "-f" || current == "--expression" || current == "--file":
			scriptGiven = true
			index++
		case strings.HasPrefix(current, "--expression=") || strings.HasPrefix(current, "--file="):
			scriptGiven = true
		case strings.HasPrefix(current, "-") && !strings.HasPrefix(current, "--") && len(current) > 1:
			// A short-option cluster; -i may carry a suffix (-i.bak) and sit
			// with others (-ni). -e/-f glued to a value are rare enough to
			// leave alone.
			if strings.Contains(current[1:], "i") {
				inPlace = true
			}
		}
	}
	if !inPlace {
		return nil
	}
	files := operands(args, map[string]bool{"-e": true, "-f": true, "--expression": true, "--file": true})
	if !scriptGiven {
		// The first operand is the script.
		if len(files) == 0 {
			return nil
		}
		files = files[1:]
	}
	return files
}

// copyDestination is the last operand of cp, mv, or install, which is where
// the bytes land - unless a target directory option names it instead, in
// which case the destination is a directory and no single file is written.
func copyDestination(args []string) []string {
	for _, current := range args {
		if current == "-t" || strings.HasPrefix(current, "--target-directory") || current == "-d" || current == "--directory" {
			return nil
		}
	}
	files := operands(args, map[string]bool{"-m": true, "-o": true, "-g": true, "--mode": true, "--owner": true, "--group": true})
	if len(files) < 2 {
		return nil
	}
	return files[len(files)-1:]
}

func plausibleWritePath(path string) bool {
	if path == "" || strings.HasPrefix(path, "&") || strings.HasPrefix(path, "(") {
		return false
	}
	if strings.ContainsAny(path, "*?$`") {
		// Globs and expansions name no single file.
		return false
	}
	if strings.HasPrefix(path, "/dev/") || strings.HasPrefix(path, "/proc/") {
		return false
	}
	return true
}

// StripHeredocs removes heredoc bodies from a command so the write scanner
// reads only the commands themselves. A body is data: scanning it invents
// writes out of whatever the document happens to contain.
func StripHeredocs(command string) string {
	lines := strings.Split(command, "\n")
	out := make([]string, 0, len(lines))
	delimiter := ""
	stripTabs := false
	for _, line := range lines {
		if delimiter != "" {
			candidate := line
			if stripTabs {
				candidate = strings.TrimLeft(candidate, "\t")
			}
			if candidate == delimiter {
				delimiter = ""
			}
			continue
		}
		out = append(out, line)
		words := splitWords(line)
		for index, current := range words {
			if current.op && isHeredoc(current.text) && index+1 < len(words) && !words[index+1].op {
				delimiter = words[index+1].text
				stripTabs = strings.HasSuffix(current.text, "-")
				break
			}
		}
	}
	return strings.Join(out, "\n")
}

// joinContinuations folds a line ending in a backslash into the next one.
func joinContinuations(lines []string) []string {
	out := make([]string, 0, len(lines))
	pending := ""
	for _, line := range lines {
		if strings.HasSuffix(line, "\\") && !strings.HasSuffix(line, "\\\\") {
			pending += strings.TrimSuffix(line, "\\") + " "
			continue
		}
		out = append(out, pending+line)
		pending = ""
	}
	if pending != "" {
		out = append(out, pending)
	}
	return out
}

// segments splits a word list at the control operators, leaving redirections
// inside the simple command they belong to.
func segments(words []word) [][]word {
	out := [][]word{}
	current := []word{}
	for _, w := range words {
		if w.op && !strings.ContainsAny(w.text, "<>") {
			if len(current) > 0 {
				out = append(out, current)
			}
			current = []word{}
			continue
		}
		current = append(current, w)
	}
	if len(current) > 0 {
		out = append(out, current)
	}
	return out
}

// splitWords cuts one line into shell words, honouring single quotes, double
// quotes, and backslashes, and pulling the unquoted operator characters out
// as operators of their own. An unquoted # at the start of a word begins a
// comment.
func splitWords(line string) []word {
	words := []word{}
	var current strings.Builder
	started := false
	flush := func() {
		if started {
			words = append(words, word{text: current.String()})
		}
		current.Reset()
		started = false
	}
	runes := []rune(line)
	for index := 0; index < len(runes); index++ {
		r := runes[index]
		switch {
		case r == '\'':
			started = true
			for index++; index < len(runes) && runes[index] != '\''; index++ {
				current.WriteRune(runes[index])
			}
		case r == '"':
			started = true
			for index++; index < len(runes) && runes[index] != '"'; index++ {
				if runes[index] == '\\' && index+1 < len(runes) && strings.ContainsRune("$\"\\`\n", runes[index+1]) {
					index++
				}
				current.WriteRune(runes[index])
			}
		case r == '\\':
			started = true
			if index+1 < len(runes) {
				index++
				current.WriteRune(runes[index])
			}
		case r == ' ' || r == '\t':
			flush()
		case r == '#' && !started:
			flush()
			return words
		case strings.ContainsRune("|;&<>()", r):
			// A word of digits before a redirect is its fd: 2>file.
			if started && strings.ContainsRune("<>", r) && isDigits(current.String()) {
				current.Reset()
				started = false
				current.WriteString(strings.TrimSpace(string(runes[operatorStart(runes, index):index])))
			}
			flush()
			start := index
			for index+1 < len(runes) && strings.ContainsRune("|;&<>()-", runes[index+1]) {
				// A hyphen belongs only to <<-.
				if runes[index+1] == '-' && string(runes[start:index+1]) != "<<" {
					break
				}
				index++
			}
			text := string(runes[start : index+1])
			if fd := fdBefore(words); fd != "" {
				words = words[:len(words)-1]
				text = fd + text
			}
			words = append(words, word{text: text, op: true})
		default:
			started = true
			current.WriteRune(r)
		}
	}
	flush()
	return words
}

// fdBefore returns the fd digits an operator is glued to, when the word just
// before it is digits with no space between - which splitWords records by
// flushing them as a word right before the operator.
func fdBefore(words []word) string {
	if len(words) == 0 || words[len(words)-1].op {
		return ""
	}
	last := words[len(words)-1].text
	if isDigits(last) && last != "" {
		return last
	}
	return ""
}

// operatorStart is where the digits glued to the operator at index begin.
func operatorStart(runes []rune, index int) int {
	start := index
	for start > 0 && runes[start-1] >= '0' && runes[start-1] <= '9' {
		start--
	}
	return start
}

func isDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
