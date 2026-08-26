package apply

import (
	"fmt"
	"strings"

	"github.com/satoricorp/demux"
)

// messageFor writes the commit message.
//
// A subject like "update internal/limiter.go" says nothing a diff does not.
// The body carries what a reader six months later actually needs: why the
// change exists - which hv knows, because it is what the person asked the
// session for - what the session concluded, and what moved, file by file with
// the symbols involved.
//
// Nothing is padded. A session with no recorded prompt gets no Why paragraph
// rather than a sentence restating the subject.
func messageFor(revision demux.RevisionProposal, patch string, options Options) string {
	var builder strings.Builder
	builder.WriteString(subjectFor(revision))
	builder.WriteString("\n")

	if why := whyParagraph(revision, options); why != "" {
		builder.WriteString("\n" + why + "\n")
	}
	if outcome := outcomeParagraph(revision, options); outcome != "" {
		builder.WriteString("\n" + outcome + "\n")
	}
	if changes := changeSummary(revision, patch); len(changes) > 0 {
		builder.WriteString("\nChanged:\n")
		for _, change := range changes {
			builder.WriteString(change + "\n")
		}
	}

	// The sessions that produced the change are recorded as trailers, so the
	// provenance survives in the commit rather than only in hv's journal.
	if sessions := sessionsOf(revision, options); len(sessions) > 0 {
		builder.WriteString("\n")
		for _, id := range sessions {
			builder.WriteString("hv-session: " + id)
			if info, ok := options.infoFor(id); ok && info.Title != "" {
				builder.WriteString(" (" + info.Title + ")")
			}
			builder.WriteString("\n")
		}
	}
	return builder.String()
}

// sessionsOf is who wrote this revision: hv's own attribution when it has any,
// the proposal's otherwise.
func sessionsOf(revision demux.RevisionProposal, options Options) []string {
	if ids, ok := options.RevisionSessions[revision.ID]; ok && len(ids) > 0 {
		return ids
	}
	return revision.SessionIDs
}

// changeSummary describes the patch per file: how many lines moved, whether
// the file is new, and which symbols the hunks sit in.
func changeSummary(revision demux.RevisionProposal, patch string) []string {
	type stat struct {
		added, removed int
		created        bool
	}
	stats := map[string]*stat{}
	order := []string{}
	current := ""
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			current = strings.TrimPrefix(line, "+++ b/")
			if _, ok := stats[current]; !ok {
				stats[current] = &stat{}
				order = append(order, current)
			}
		case strings.HasPrefix(line, "new file mode") && current != "":
			stats[current].created = true
		case current == "":
			continue
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "+"):
			stats[current].added++
		case strings.HasPrefix(line, "-"):
			stats[current].removed++
		}
	}
	// A new file's "---" line is /dev/null, which the loop above cannot see
	// because it skips header lines; the mode line covers it.
	symbols := map[string][]string{}
	for _, hunk := range revision.Hunks {
		symbol := strings.TrimSpace(hunk.Symbol)
		if symbol == "" {
			continue
		}
		if !containsString(symbols[hunk.File], symbol) {
			symbols[hunk.File] = append(symbols[hunk.File], symbol)
		}
	}

	out := make([]string, 0, len(order))
	for _, file := range order {
		entry := stats[file]
		line := fmt.Sprintf("  %s: +%d/-%d", file, entry.added, entry.removed)
		if entry.created {
			line += " (new file)"
		}
		if names := symbols[file]; len(names) > 0 {
			line += " in " + strings.Join(names, ", ")
		}
		out = append(out, line)
	}
	return out
}

// whyParagraph is what the sessions behind this revision were asked to do -
// the only part of a commit message a diff can never supply.
func whyParagraph(revision demux.RevisionProposal, options Options) string {
	reasons := []string{}
	for _, id := range sessionsOf(revision, options) {
		info, ok := options.infoFor(id)
		if !ok || strings.TrimSpace(info.Intent) == "" {
			continue
		}
		if title := strings.TrimSpace(info.Title); title != "" {
			reasons = append(reasons, fmt.Sprintf("%s (%s)", info.Intent, title))
			continue
		}
		reasons = append(reasons, info.Intent)
	}
	if len(reasons) == 0 {
		return ""
	}
	if len(reasons) == 1 {
		return wrap("Asked for: "+reasons[0], 72)
	}
	// Several sessions contributed - demux pairs a test with its source, and
	// those are routinely different sessions - so each gets its own line.
	var builder strings.Builder
	builder.WriteString("Asked for, across " + fmt.Sprint(len(reasons)) + " sessions:\n")
	for _, reason := range reasons {
		builder.WriteString(wrap("  - "+reason, 72) + "\n")
	}
	return strings.TrimRight(builder.String(), "\n")
}

// outcomeParagraph is what the session said it did, when that adds something
// the subject and the file list do not already say.
func outcomeParagraph(revision demux.RevisionProposal, options Options) string {
	for _, id := range sessionsOf(revision, options) {
		info, ok := options.infoFor(id)
		if !ok || strings.TrimSpace(info.Outcome) == "" {
			continue
		}
		return wrap("The session reported: "+info.Outcome, 72)
	}
	return ""
}

// wrap breaks a paragraph at a width commit messages are read at, without
// splitting a word.
func wrap(text string, width int) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	var builder strings.Builder
	line := words[0]
	for _, word := range words[1:] {
		if len(line)+1+len(word) > width {
			builder.WriteString(line + "\n")
			line = word
			continue
		}
		line += " " + word
	}
	builder.WriteString(line)
	return builder.String()
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func subjectFor(revision demux.RevisionProposal) string {
	subject := strings.TrimSpace(revision.Intent)
	if subject == "" {
		subject = "update " + strings.Join(revision.Files, ", ")
	}
	if index := strings.IndexByte(subject, '\n'); index >= 0 {
		subject = subject[:index]
	}
	return subject
}

func labelFor(revision demux.RevisionProposal, options Options) string {
	if label := strings.TrimSpace(options.Label); label != "" {
		return label
	}
	if intent := strings.TrimSpace(revision.Intent); intent != "" {
		return intent
	}
	return revision.ID
}
