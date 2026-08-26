package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/satoricorp/hv/internal/text"
	"github.com/satoricorp/hv/recall"
	"github.com/satoricorp/hv/session"
)

// peekState is one session's transcript, read on demand.
//
// A row in the list says a session touched three files in a repo, which is
// enough to tell two sessions apart and not enough to decide whether to merge
// one. Peeking answers the actual question - what was this asked to do, and
// what did it do - without leaving the browser.
type peekState struct {
	session session.Session
	summary recall.Summary
	items   []recall.Item
	offset  int
	loading bool
	err     error
}

type peekedMsg struct {
	sessionID string
	summary   recall.Summary
	items     []recall.Item
	err       error
}

// peekCmd reads the transcript off the render loop: a long session is a large
// file, and the list should not freeze while it is parsed.
func peekCmd(current session.Session) tea.Cmd {
	return func() tea.Msg {
		items, err := recall.Read(current, 0)
		if err != nil {
			return peekedMsg{sessionID: current.ID, err: err}
		}
		return peekedMsg{
			sessionID: current.ID,
			summary:   recall.Summarize(current, items),
			items:     items,
		}
	}
}

func (m model) updatePeek(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "esc", "p", "v", "left", "h":
		m.screen = screenSessions
		return m, nil
	case "up", "k":
		m.peek.offset = maxInt(m.peek.offset-1, 0)
	case "down", "j":
		m.peek.offset = minInt(m.peek.offset+1, maxInt(len(m.peek.items)-m.peekHeight(), 0))
	case "pgup":
		m.peek.offset = maxInt(m.peek.offset-m.peekHeight(), 0)
	case "pgdown", " ":
		m.peek.offset = minInt(m.peek.offset+m.peekHeight(), maxInt(len(m.peek.items)-m.peekHeight(), 0))
	case "enter":
		// Straight from looking at it to merging it.
		m.screen = screenPlanning
		m.dryRun = false
		m.outcome = Outcome{Merge: []session.Session{m.peek.session}}
		return m, planCmd(m.outcome.Merge, m.events)
	case "o":
		m.screen = screenSessions
		return m.openCurrent()
	}
	return m, nil
}

// peekFacts is the summary block above the transcript. It is a method so the
// body can be sized against its real height rather than a guess, which is the
// difference between a frame that fits the terminal and one that scrolls it.
// exchange is one round of the conversation: what was asked and what came
// back. Tool calls are left out - a list of file reads is what the session
// row already shows, and it buries the two things worth reading.
type exchange struct {
	asked    string
	answered string
}

// exchanges pairs the transcript into rounds, most recent last.
func exchanges(items []recall.Item) []exchange {
	out := []exchange{}
	for _, item := range items {
		switch item.Kind {
		case recall.KindUser:
			out = append(out, exchange{asked: item.Text})
		case recall.KindAssistant:
			// An answer with nothing before it still happened; a second answer
			// in a row replaces the first, which is the one that summarized.
			if len(out) == 0 {
				out = append(out, exchange{})
			}
			out[len(out)-1].answered = item.Text
		}
	}
	return out
}

// peekSummary is at most three sentences: what the session was asked, what it
// reported, and what it touched. Three is the limit because a summary someone
// has to read carefully is not a summary.
func (m model) peekSummary() []string {
	lines := []string{}
	for _, sentence := range m.peekSentences() {
		lines = append(lines, wrapLines(sentence, m.width-4, "  ", "  ")...)
	}
	return lines
}

// peekSentences is the summary itself, one string per sentence.
func (m model) peekSentences() []string {
	current := m.peek.session
	sentences := []string{}
	if intent := strings.TrimSpace(m.peek.summary.Intent); intent != "" {
		// Quoted rather than folded into a sentence: prompts are not always
		// imperatives, and "Asked to testing this out" reads as a typo.
		sentences = append(sentences, "Asked: “"+trimPeriod(intent)+"”.")
	}
	if outcome := strings.TrimSpace(m.peek.summary.Outcome); outcome != "" {
		sentences = append(sentences, "Reported: "+trimPeriod(outcome)+".")
	}
	if len(current.Files) > 0 {
		names := shortPaths(current.Files)
		listed := names
		suffix := ""
		if len(names) > 3 {
			listed = names[:3]
			suffix = fmt.Sprintf(" and %d more", len(names)-3)
		}
		sentences = append(sentences, fmt.Sprintf("Edited %s%s.", strings.Join(listed, ", "), suffix))
	}
	if len(sentences) == 0 {
		sentences = append(sentences, "No transcript and no edits were recorded for this session.")
	}
	return sentences
}

// peekHeight is what the conversation gets: the terminal, less the header and
// its border, the summary block, the pane's own border, and the footer.
func (m model) peekHeight() int {
	return maxInt(m.height-2-(len(m.peekSummary())+2)-1-1, 1)
}

func (m model) peekView() string {
	current := m.peek.session
	// The header stays one line: a wrapped title costs a line of conversation
	// and reads as a rendering fault.
	title := lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Render(text.Truncate(current.Label(), 40))
	facts := fmt.Sprintf("  ·  %s  ·  %s  ·  %d edits in %d files",
		current.Tool, repoName(current), current.EditCount, len(current.Files))
	header := styleTitleBar.Width(m.width).Render(
		title + styleCount.Render(text.Truncate(facts, maxInt(m.width-lipgloss.Width(title)-4, 10))))

	summary := lipgloss.NewStyle().Padding(1, 1).Render(strings.Join(m.peekSummary(), "\n"))
	body := m.peekBody()

	key := func(k, label string) string { return styleKey.Render(k) + styleDim.Render(" "+label) }
	footer := styleFooter.Width(m.width).Render(strings.Join([]string{
		key("↑↓", "scroll"),
		key("enter", "merge this"),
		key("o", "open"),
		key("esc", "back"),
		key("q", "quit"),
	}, styleDim.Render("  ·  ")))

	return lipgloss.JoinVertical(lipgloss.Left, header, summary, body, footer)
}

// lastExchanges is how many rounds the conversation pane shows. Four is enough
// to see where a session ended up and short enough to read at a glance.
const lastExchanges = 4

// peekBody is the tail of the conversation, wrapped and spaced so the two
// voices are told apart without reading closely.
func (m model) peekBody() string {
	rounds := exchanges(m.peek.items)
	if len(rounds) > lastExchanges {
		rounds = rounds[len(rounds)-lastExchanges:]
	}
	rendered := []string{}
	for index, round := range rounds {
		if index > 0 {
			rendered = append(rendered, "")
		}
		if round.asked != "" {
			rendered = append(rendered, m.speech("▌ you", styleKey, round.asked)...)
		}
		if round.answered != "" {
			rendered = append(rendered, m.speech("▌ "+m.peek.session.Tool, styleBadgeCLI, round.answered)...)
		}
	}
	if len(rendered) == 0 {
		rendered = m.peekWithoutTranscript()
	}

	height := m.peekHeight()
	if m.peek.offset > maxInt(len(rendered)-height, 0) {
		m.peek.offset = maxInt(len(rendered)-height, 0)
	}
	visible := []string{}
	for index := m.peek.offset; index < len(rendered) && len(visible) < height; index++ {
		visible = append(visible, rendered[index])
	}
	for len(visible) < height {
		visible = append(visible, "")
	}
	return styleDetail.Width(m.width).Height(height).Render(strings.Join(visible, "\n"))
}

// maxSpeechLines caps one side of an exchange. A single answer can be a page
// of markdown, and a pane filled by one message shows fewer exchanges than a
// pane showing none.
const maxSpeechLines = 6

// speech renders one side of an exchange: a labelled first line, then the rest
// indented under it so the label reads as a speaker rather than a bullet.
func (m model) speech(label string, style lipgloss.Style, text string) []string {
	width := maxInt(m.width-6, 24)
	paragraphs := readable(text)
	body := []string{}
	for _, paragraph := range paragraphs {
		if strings.HasPrefix(paragraph, "\x00code:") {
			body = append(body, styleDim.Render(strings.TrimPrefix(paragraph, "\x00code:")))
			continue
		}
		body = append(body, wrapLines(paragraph, width, "", "")...)
	}
	if len(body) > maxSpeechLines {
		hidden := len(body) - maxSpeechLines
		body = body[:maxSpeechLines]
		suffix := "lines"
		if hidden == 1 {
			suffix = "line"
		}
		body = append(body, styleDim.Render(fmt.Sprintf("… %d more %s", hidden, suffix)))
	}

	out := make([]string, 0, len(body)+1)
	out = append(out, style.Render(" "+label))
	for _, line := range body {
		out = append(out, "   "+line)
	}
	return out
}

// readable turns a message into lines worth showing in a narrow pane: fenced
// code becomes a one-line note, headings and emphasis lose their markup, and
// runs of blank lines collapse. The point is to read a conversation, not to
// render markdown.
func readable(text string) []string {
	out := []string{}
	inFence := false
	fenceLines := 0
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inFence {
				out = append(out, fmt.Sprintf("\x00code:[code, %d lines]", fenceLines))
				inFence, fenceLines = false, 0
				continue
			}
			inFence = true
			continue
		}
		if inFence {
			fenceLines++
			continue
		}
		if trimmed == "" {
			// One blank line between paragraphs, never a run of them.
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
			continue
		}
		out = append(out, plain(trimmed))
	}
	if inFence {
		out = append(out, fmt.Sprintf("\x00code:[code, %d lines]", fenceLines))
	}
	// A trailing blank line wastes a row the next speaker could use.
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// plain strips the markup that only makes sense rendered.
func plain(line string) string {
	line = strings.TrimLeft(line, "#")
	line = strings.ReplaceAll(line, "**", "")
	line = strings.ReplaceAll(line, "`", "")
	return strings.TrimSpace(line)
}

// wrapLines breaks text to a width without splitting words, prefixing the
// first line and the rest independently.
func wrapLines(text string, width int, first, rest string) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	if width < 20 {
		width = 20
	}
	out := []string{}
	line := first + words[0]
	for _, word := range words[1:] {
		if len([]rune(line))+1+len([]rune(word)) > width {
			out = append(out, line)
			line = rest + word
			continue
		}
		line += " " + word
	}
	return append(out, line)
}

func trimPeriod(text string) string {
	return strings.TrimRight(strings.TrimSpace(text), ".")
}

// peekWithoutTranscript summarizes a session from what hv has when the
// conversation is missing.
func (m model) peekWithoutTranscript() []string {
	current := m.peek.session
	if len(current.Files) == 0 {
		return []string{styleDim.Render("  no conversation and no edits recorded for this session")}
	}
	out := []string{styleDim.Render(fmt.Sprintf(
		"  no conversation recorded; %d edits across %d files:", current.EditCount, len(current.Files)))}
	for index, file := range current.Files {
		if index >= m.peekHeight()-1 {
			out = append(out, styleDim.Render(fmt.Sprintf("  … and %d more", len(current.Files)-index)))
			break
		}
		out = append(out, styleActiveDot.Render("  edit   ")+styleDim.Render(shortPath(file)))
	}
	return out
}

func shortPaths(files []string) []string {
	out := make([]string, 0, len(files))
	for _, file := range files {
		out = append(out, shortPath(file))
	}
	return out
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

// PeekFrameForTest renders one session's peek screen without a terminal.
func PeekFrameForTest(current session.Session, width, height int) string {
	built := model{chosen: map[string]bool{}, width: width, height: height, screen: screenPeek}
	built.peek = peekState{session: current}
	if message, ok := peekCmd(current)().(peekedMsg); ok {
		built.peek.items, built.peek.summary, built.peek.err = message.items, message.summary, message.err
	}
	return built.peekView()
}
