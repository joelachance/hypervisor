// Package resolve fixes merge conflicts with fx and commits the result as a
// real merge.
//
// fx is given one conflict region at a time, with both versions in full and
// what each session was asked to do - never the repository. It answers with
// the resolved lines and an account of what it kept from each side; hv applies
// that answer and does the git work itself. Two reasons for that shape:
//
//   - a model that can edit the tree can also quietly change things nobody
//     reviewed, and hv's whole safety story is that it only adds objects and
//     moves refs;
//   - a structured answer can be checked. Every line either survives into the
//     resolution or is listed as deliberately dropped, and a resolution that
//     silently loses a line is rejected before it reaches a commit.
package resolve

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

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

// Resolution is the agent's answer, in the shape hv can verify.
type Resolution struct {
	// Resolved is the replacement for the region, line by line.
	Resolved []string `json:"resolved"`
	// KeptOurs and KeptTheirs are the lines from each side the resolution
	// preserves, verbatim. Dropped is anything deliberately left out, with a
	// reason - the only way a line may disappear.
	KeptOurs   []string `json:"kept_ours"`
	KeptTheirs []string `json:"kept_theirs"`
	Dropped    []string `json:"dropped"`
	Reasoning  string   `json:"reasoning"`
}

// Fx runs the fx coding agent noninteractively.
type Fx struct {
	// Binary defaults to "fx" on PATH.
	Binary string
	// Dir is the workspace fx runs in.
	Dir string
	// Model overrides fx's configured model.
	Model string
}

// Name implements Runner.
func (f Fx) Name() string { return "fx" }

// Resolve implements Runner by asking fx one question and parsing its JSON.
//
// --quiet keeps fx's transcript off stdout, --json gives a machine-readable
// envelope, and --no-save keeps a conflict question out of the workspace's
// session history, where it would look like work someone did.
func (f Fx) Resolve(ctx context.Context, request Request) (Resolution, error) {
	binary := f.Binary
	if strings.TrimSpace(binary) == "" {
		binary = "fx"
	}
	args := []string{"ask", "--json", "--quiet", "--no-save"}
	if strings.TrimSpace(f.Model) != "" {
		args = append(args, "--model", f.Model)
	}
	args = append(args, "--", Prompt(request))

	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = f.Dir
	out, err := command.Output()
	if err != nil {
		detail := ""
		var exitErr *exec.ExitError
		if ok := asExit(err, &exitErr); ok {
			detail = strings.TrimSpace(string(exitErr.Stderr))
		}
		return Resolution{}, fmt.Errorf("fx ask: %v: %s", err, detail)
	}
	var envelope struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		return Resolution{}, fmt.Errorf("fx returned unreadable JSON: %w", err)
	}
	return ParseResolution(envelope.Output)
}

// Prompt is the question fx is asked. It is a plain function so the exact text
// is reviewable and testable rather than buried in a call.
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
Keep both intents working if that is possible. Drop a line only when keeping it
would be wrong, and say why.

Reply with only a JSON object, no prose around it:

{
  "resolved": ["the replacement lines, in order"],
  "kept_ours": ["lines from the first version that survive, verbatim"],
  "kept_theirs": ["lines from the second version that survive, verbatim"],
  "dropped": ["any line you left out, each with a reason after ' -- '"],
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
// This is the guard that makes an agent's answer safe to commit: a line that
// existed on either side must appear in the resolution, be claimed as kept, or
// be listed as dropped with a reason. Anything else has gone missing quietly,
// which is the one failure mode nobody would notice.
func Verify(request Request, resolution Resolution) []string {
	problems := []string{}
	resolved := strings.Join(resolution.Resolved, "\n")
	accounted := func(line string) bool {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			return true
		}
		if strings.Contains(resolved, trimmed) {
			return true
		}
		for _, kept := range append(append([]string{}, resolution.KeptOurs...), resolution.KeptTheirs...) {
			if strings.TrimSpace(kept) == trimmed {
				return true
			}
		}
		for _, dropped := range resolution.Dropped {
			if strings.Contains(dropped, trimmed) {
				return true
			}
		}
		return false
	}
	for _, line := range request.Ours {
		if !accounted(line) {
			problems = append(problems, fmt.Sprintf("%s: line not kept, dropped, or explained: %s", request.OursFrom, strings.TrimSpace(line)))
		}
	}
	for _, line := range request.Theirs {
		if !accounted(line) {
			problems = append(problems, fmt.Sprintf("%s: line not kept, dropped, or explained: %s", request.TheirsFrom, strings.TrimSpace(line)))
		}
	}
	return problems
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

func asExit(err error, target **exec.ExitError) bool {
	exitErr, ok := err.(*exec.ExitError)
	if ok {
		*target = exitErr
	}
	return ok
}
