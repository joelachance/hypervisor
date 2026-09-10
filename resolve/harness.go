// Package resolve fixes merge conflicts with a coding agent and commits the
// result as a real merge.
//
// Any harness will do. The contract is one question in, one answer out on
// stdout, which is the smallest thing fx, pi, claude, codex, and opencode all
// already do - so hv ships a recipe for each and takes a binary plus an
// argument template for anything else.
//
// The harness is given one conflict region at a time, with both versions in full and
// what each session was asked to do - never the repository. It answers with
// the resolved lines and an account of what it kept from each side; hv applies
// that answer and does the git work itself. Two reasons for that shape:
//
//   - a model that can edit the tree can also quietly change things nobody
//     reviewed, and hv's whole safety story is that it only adds objects and
//     moves refs. The harness runs in a scratch directory for the same reason;
//   - a structured answer can be checked. Every line either survives into the
//     resolution or is listed as deliberately dropped, and a resolution that
//     silently loses a line is rejected before it reaches a commit.
package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/satoricorp/hv/apply"
	"github.com/satoricorp/hv/conflict"
)

// Runner asks an agent to resolve one region. It is an interface so the
// pipeline can be tested without a model, and so another harness can be
// substituted for fx.
type Runner interface {
	Resolve(ctx context.Context, request Request) (Resolution, error)
	Name() string
}

// Request is everything the agent is given. Deliberately not the repository:
// the region, the two versions, and why each side is the way it is.
type Request struct {
	Path         string   `json:"path"`
	Region       int      `json:"region"`
	Base         []string `json:"base"`
	Ours         []string `json:"ours"`
	Theirs       []string `json:"theirs"`
	OursFrom     string   `json:"ours_from"`
	TheirsFrom   string   `json:"theirs_from"`
	OursIntent   string   `json:"ours_intent,omitempty"`
	TheirsIntent string   `json:"theirs_intent,omitempty"`
}

// Rewrite is an input line the resolution replaced with different text -
// combining two comments into one sentence, say. It is the third way a line
// can be accounted for, and without it an honest merge of two lines would look
// like two lines going missing.
type Rewrite struct {
	From string `json:"from"`
	To   string `json:"to"`
	Why  string `json:"why,omitempty"`
}

// Addition is a line the resolution introduces that came from neither side -
// an import both halves now need, say. It has to be declared with a reason,
// or a line from nowhere is indistinguishable from an agent slipping
// something in.
type Addition struct {
	Line string `json:"line"`
	Why  string `json:"why,omitempty"`
}

// Resolution is the agent's answer, in the shape hv can verify.
type Resolution struct {
	// Resolved is the replacement for the region, line by line.
	Resolved []string `json:"resolved"`
	// KeptOurs and KeptTheirs are the lines from each side the resolution
	// preserves verbatim; Rewritten are lines it replaced; Dropped are lines
	// it left out, each with a reason; Added are lines from neither side.
	// Between them they must account for every line of both sides and every
	// line of the resolution - that is the whole check.
	KeptOurs   []string   `json:"kept_ours"`
	KeptTheirs []string   `json:"kept_theirs"`
	Rewritten  []Rewrite  `json:"rewritten"`
	Dropped    []string   `json:"dropped"`
	Added      []Addition `json:"added,omitempty"`
	Reasoning  string     `json:"reasoning"`
}

// Harness runs any coding agent that can answer one question on stdout.
//
// That contract is the whole abstraction, and it is deliberately the smallest
// one every harness already satisfies: run a command, get text back. No
// plugin, no server, no per-harness client - hv can drive fx, pi, claude,
// codex, opencode, or whatever exists next year, and someone can point it at a
// harness hv has never heard of with two flags.
type Harness struct {
	// Label is what appears in output and commit messages.
	Label string
	// Binary is the executable; Args are its arguments. The literal
	// "{prompt}" in Args is replaced with the question; if it appears
	// nowhere, the question is written to the process's stdin instead.
	Binary string
	Args   []string
	// ModelFlag and Model are appended when Model is set.
	ModelFlag string
	Model     string
	// Dir is where the harness runs. It defaults to a temporary directory,
	// not the repository: several harnesses have edit and write tools on by
	// default, and hv wants an answer, not a harness loose in the tree.
	Dir string
}

// Builtins are the harnesses hv knows how to call, in the order Detect tries
// them. Each is invoked noninteractively, without saving a session - a
// conflict question is not work someone did, and it should not appear in their
// history as though it were.
func Builtins() []Harness {
	return []Harness{
		{Label: "pi", Binary: "pi", ModelFlag: "--model",
			Args: []string{"-p", "--no-tools", "--no-session", "{prompt}"}},
		{Label: "fx", Binary: "fx", ModelFlag: "--model",
			Args: []string{"ask", "--quiet", "--no-save", "--", "{prompt}"}},
		{Label: "claude", Binary: "claude", ModelFlag: "--model",
			Args: []string{"-p", "{prompt}"}},
		{Label: "codex", Binary: "codex", ModelFlag: "--model",
			Args: []string{"exec", "{prompt}"}},
		{Label: "opencode", Binary: "opencode", ModelFlag: "--model",
			Args: []string{"run", "{prompt}"}},
	}
}

// Builtin returns a known harness by name.
func Builtin(name string) (Harness, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, harness := range Builtins() {
		if harness.Label == name {
			return harness, true
		}
	}
	return Harness{}, false
}

// Detect returns the first known harness present on PATH.
func Detect() (Harness, bool) {
	for _, harness := range Builtins() {
		if _, err := exec.LookPath(harness.Binary); err == nil {
			return harness, true
		}
	}
	return Harness{}, false
}

// Available reports which known harnesses are installed.
func Available() []string {
	out := []string{}
	for _, harness := range Builtins() {
		if _, err := exec.LookPath(harness.Binary); err == nil {
			out = append(out, harness.Label)
		}
	}
	return out
}

// Custom builds a harness from a binary and an argument template, for a
// harness hv does not ship a recipe for.
func Custom(binary string, args []string) Harness {
	name := filepath.Base(binary)
	return Harness{Label: name, Binary: binary, Args: args}
}

// Name implements Runner.
func (h Harness) Name() string {
	if strings.TrimSpace(h.Label) != "" {
		return h.Label
	}
	return filepath.Base(h.Binary)
}

// Resolve implements Runner: ask the question, read the answer, parse it.
func (h Harness) Resolve(ctx context.Context, request Request) (Resolution, error) {
	output, err := h.Ask(ctx, Prompt(request))
	if err != nil {
		return Resolution{}, err
	}
	return ParseResolution(output)
}

// Ask runs the harness with one prompt and returns its stdout.
func (h Harness) Ask(ctx context.Context, prompt string) (string, error) {
	binary := strings.TrimSpace(h.Binary)
	if binary == "" {
		return "", fmt.Errorf("no harness configured")
	}
	args := make([]string, 0, len(h.Args)+2)
	onStdin := true
	for _, arg := range h.Args {
		if strings.Contains(arg, "{prompt}") {
			onStdin = false
			arg = strings.ReplaceAll(arg, "{prompt}", prompt)
		}
		args = append(args, arg)
	}
	if strings.TrimSpace(h.Model) != "" && strings.TrimSpace(h.ModelFlag) != "" {
		args = append([]string{h.ModelFlag, h.Model}, args...)
	}

	dir := strings.TrimSpace(h.Dir)
	if dir == "" {
		// A scratch directory by default: hv wants an answer, and a harness
		// with write tools pointed at the repository is a way to get changes
		// nobody reviewed.
		scratch, err := os.MkdirTemp("", "hv-harness-*")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(scratch)
		dir = scratch
	}

	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = dir
	if onStdin {
		command.Stdin = strings.NewReader(prompt)
	}
	out, err := command.Output()
	if err != nil {
		detail := ""
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			detail = strings.TrimSpace(string(exitErr.Stderr))
		}
		return "", fmt.Errorf("%s: %v: %s", h.Name(), err, clipError(detail))
	}
	return string(out), nil
}

// clipError keeps a harness's stderr readable when it decides to be verbose.
func clipError(detail string) string {
	const max = 400
	detail = strings.TrimSpace(detail)
	if len(detail) <= max {
		return detail
	}
	return detail[:max] + "…"
}

// Prompt is the question the harness is asked. It is a plain function so the
// exact text is reviewable and testable rather than buried in a call.
func Prompt(request Request) string {
	var builder strings.Builder
	builder.WriteString("Two coding agents edited the same lines of ")
	builder.WriteString(request.Path)
	builder.WriteString(" and their work conflicts. Resolve it.\n\n")

	if request.OursIntent != "" {
		fmt.Fprintf(&builder, "%s was asked to: %s\n", request.OursFrom, request.OursIntent)
	}
	if request.TheirsIntent != "" {
		fmt.Fprintf(&builder, "%s was asked to: %s\n", request.TheirsFrom, request.TheirsIntent)
	}
	builder.WriteString("\nThe original lines:\n")
	writeBlock(&builder, request.Base)
	fmt.Fprintf(&builder, "\n%s changed them to:\n", request.OursFrom)
	writeBlock(&builder, request.Ours)
	fmt.Fprintf(&builder, "\n%s changed them to:\n", request.TheirsFrom)
	writeBlock(&builder, request.Theirs)

	builder.WriteString(`
Keep both intents working if that is possible.

Every line of both versions above must be accounted for exactly once: kept
verbatim, rewritten into something else, or dropped with a reason. Every line
of your resolution must be one of the kept or rewritten lines, or declared as
added with a reason. That accounting is checked both ways, and a resolution
that leaves a line out of it, or contains a line it does not explain, is
rejected.

Reply with only a JSON object, no prose around it:

{
  "resolved": ["the replacement lines, in order"],
  "kept_ours": ["lines from the first version that survive verbatim"],
  "kept_theirs": ["lines from the second version that survive verbatim"],
  "rewritten": [{"from": "an input line", "to": "what it became", "why": "short reason"}],
  "dropped": ["any line you left out, each with a reason after ' -- '"],
  "added": [{"line": "a line from neither version", "why": "short reason"}],
  "reasoning": "one sentence"
}
`)
	return builder.String()
}

func writeBlock(builder *strings.Builder, lines []string) {
	builder.WriteString("```\n")
	for _, line := range lines {
		builder.WriteString(line)
		builder.WriteString("\n")
	}
	builder.WriteString("```\n")
}

// ParseResolution reads the agent's reply, tolerating the fenced code block
// models wrap JSON in.
func ParseResolution(output string) (Resolution, error) {
	body := strings.TrimSpace(output)
	if fenced := between(body, "```json", "```"); fenced != "" {
		body = fenced
	} else if fenced := between(body, "```", "```"); fenced != "" {
		body = fenced
	}
	if start := strings.IndexByte(body, '{'); start > 0 {
		body = body[start:]
	}
	if end := strings.LastIndexByte(body, '}'); end >= 0 && end < len(body)-1 {
		body = body[:end+1]
	}
	var resolution Resolution
	if err := json.Unmarshal([]byte(body), &resolution); err != nil {
		return Resolution{}, fmt.Errorf("could not read the resolution: %w", err)
	}
	if len(resolution.Resolved) == 0 {
		return resolution, fmt.Errorf("the resolution is empty")
	}
	return resolution, nil
}

func between(text, open, close string) string {
	start := strings.Index(text, open)
	if start < 0 {
		return ""
	}
	rest := text[start+len(open):]
	end := strings.Index(rest, close)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// Verify checks a resolution against the region it claims to resolve.
//
// This is the guard that makes an agent's answer safe to commit, and it is
// checked in both directions. Every line of both sides must be claimed
// exactly once - kept, rewritten, or dropped with a reason - and every claim
// must be true: a kept line has to be in the resolution, a rewrite's result
// has to be in the resolution, and a claimed line has to have existed. Then
// the resolution is read back: every line in it must be one of those kept or
// rewritten or added lines, no more often than it was claimed, so nothing
// invented and nothing duplicated gets through either. Blank lines and
// leading or trailing whitespace are not counted.
func Verify(request Request, resolution Resolution) []string {
	problems := []string{}
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	keptOurs, keptTheirs := multiset(resolution.KeptOurs), multiset(resolution.KeptTheirs)
	// A line both sides had may be listed under either; the copies read
	// against the other side without being consumed.
	sharedOurs, sharedTheirs := multiset(resolution.KeptOurs), multiset(resolution.KeptTheirs)
	rewrittenFrom := counts{}
	for _, rewrite := range resolution.Rewritten {
		if strings.TrimSpace(rewrite.From) == "" || strings.TrimSpace(rewrite.To) == "" {
			fail("a rewrite is missing its from or to line")
			continue
		}
		rewrittenFrom.add(rewrite.From)
	}
	dropped := counts{}
	for _, entry := range resolution.Dropped {
		line, reason, ok := strings.Cut(entry, " -- ")
		if !ok || strings.TrimSpace(reason) == "" {
			fail("dropped without a reason: %s", strings.TrimSpace(entry))
			continue
		}
		dropped.add(line)
	}

	// Every input line, claimed once - or, failing a claim, simply present
	// in the resolution, which is what kept means.
	resolved := multiset(resolution.Resolved)
	account := func(from string, lines []string, own, other counts) {
		for _, line := range lines {
			key := strings.TrimSpace(line)
			if key == "" {
				continue
			}
			switch {
			case own.take(key), rewrittenFrom.take(key), other.has(key):
			case dropped.take(key):
				if resolved.has(key) {
					fail("dropped but still in the resolution: %s", key)
				}
			case resolved.has(key):
			default:
				fail("%s: line not kept, rewritten, or dropped: %s", from, key)
			}
		}
	}
	account(request.OursFrom, request.Ours, keptOurs, sharedTheirs)
	account(request.TheirsFrom, request.Theirs, keptTheirs, sharedOurs)

	// Every claim, about a line that existed.
	for _, leftover := range []struct {
		name string
		rest counts
	}{{"kept_ours", keptOurs}, {"kept_theirs", keptTheirs}, {"rewritten", rewrittenFrom}, {"dropped", dropped}} {
		for _, line := range leftover.rest.remaining() {
			if leftover.name == "kept_ours" && sharedTheirs.has(line) || leftover.name == "kept_theirs" && sharedOurs.has(line) {
				continue
			}
			if hasLine(request.Ours, line) || hasLine(request.Theirs, line) {
				// Claimed more often than it appeared.
				fail("%s claims %q more times than the sides had it", leftover.name, line)
				continue
			}
			fail("%s claims a line neither side had: %s", leftover.name, line)
		}
	}

	// Every claim, true of the resolution; and the resolution, nothing but
	// input lines, rewrites, and declared additions - each no more often
	// than the sides had it or the claims allow.
	explained := counts{}
	for _, line := range append(append([]string{}, request.Ours...), request.Theirs...) {
		if strings.TrimSpace(line) != "" {
			explained.add(line)
		}
	}
	for _, kept := range append(append([]string{}, resolution.KeptOurs...), resolution.KeptTheirs...) {
		if strings.TrimSpace(kept) == "" {
			continue
		}
		if !resolved.has(kept) {
			fail("claimed kept but not in the resolution: %s", strings.TrimSpace(kept))
		}
	}
	for _, rewrite := range resolution.Rewritten {
		if strings.TrimSpace(rewrite.To) == "" {
			continue
		}
		if !resolved.has(rewrite.To) {
			fail("rewritten to a line that is not in the resolution: %s", strings.TrimSpace(rewrite.To))
		}
		explained.add(rewrite.To)
	}
	for _, added := range resolution.Added {
		if strings.TrimSpace(added.Line) == "" {
			continue
		}
		if strings.TrimSpace(added.Why) == "" {
			fail("added without a reason: %s", strings.TrimSpace(added.Line))
		}
		if !resolved.has(added.Line) {
			fail("added a line that is not in the resolution: %s", strings.TrimSpace(added.Line))
		}
		explained.add(added.Line)
	}
	for _, line := range resolution.Resolved {
		key := strings.TrimSpace(line)
		if key == "" {
			continue
		}
		if !explained.take(key) {
			fail("line in the resolution that came from neither side and was not declared added, or a duplicate: %s", key)
		}
	}
	return problems
}

// counts is a multiset of trimmed lines.
type counts map[string]int

func multiset(lines []string) counts {
	out := counts{}
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			out.add(line)
		}
	}
	return out
}

func (c counts) add(line string) { c[strings.TrimSpace(line)]++ }

func (c counts) has(line string) bool { return c[strings.TrimSpace(line)] > 0 }

// take consumes one occurrence, reporting whether there was one.
func (c counts) take(line string) bool {
	key := strings.TrimSpace(line)
	if c[key] == 0 {
		return false
	}
	c[key]--
	return true
}

func (c counts) remaining() []string {
	out := []string{}
	for line, count := range c {
		for ; count > 0; count-- {
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return out
}

func hasLine(lines []string, want string) bool {
	for _, line := range lines {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// RequestFor builds the question for one step of a conflict report.
func RequestFor(report conflict.Report, step conflict.Step) Request {
	request := Request{
		Path: step.Path, Region: step.Region,
		Base: step.Base, Ours: step.Ours, Theirs: step.Theirs,
		OursFrom: step.OursFrom, TheirsFrom: step.TheirsFrom,
	}
	if report.Ours.Context != nil {
		request.OursIntent = report.Ours.Context.Intent
	}
	if report.Theirs.Context != nil {
		request.TheirsIntent = report.Theirs.Context.Intent
	}
	return request
}

// Adapter lets a Harness settle conflicts that arise while committing, which
// is the same question in a different frame: two versions of a region, and
// which side each came from.
type Adapter struct{ Harness Harness }

// Name implements apply.Resolver.
func (a Adapter) Name() string { return a.Harness.Name() }

// ResolveConflict implements apply.Resolver.
func (a Adapter) ResolveConflict(ctx context.Context, request apply.ConflictRequest) ([]string, error) {
	ask := Request{
		Path: request.Path, Region: request.Region,
		Base: request.Base, Ours: request.Ours, Theirs: request.Theirs,
		OursFrom: request.OursFrom, TheirsFrom: request.TheirsFrom,
	}
	resolution, err := a.Harness.Resolve(ctx, ask)
	if err != nil {
		return nil, err
	}
	if problems := Verify(ask, resolution); len(problems) > 0 {
		return nil, fmt.Errorf("%s left %d line(s) unaccounted for: %s",
			a.Harness.Name(), len(problems), problems[0])
	}
	return resolution.Resolved, nil
}
