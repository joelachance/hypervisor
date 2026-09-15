package parse

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/satoricorp/demux/capture"
)

func claudeLine(body string) []byte { return []byte(body) }

func TestClaudeEditToolBecomesEditEvent(t *testing.T) {
	line := claudeLine(`{"type":"assistant","sessionId":"s1","timestamp":"2026-08-21T10:00:00.000Z","cwd":"/repo",
	 "message":{"model":"claude-opus-5","content":[{"type":"tool_use","name":"Edit","input":
	 {"file_path":"/repo/a.go","old_string":"old","new_string":"new"}}]}}`)

	events, err := (&Claude{}).ParseLine("/logs/s1.jsonl", line)
	if err != nil {
		t.Fatalf("ParseLine() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %v, want one", events)
	}
	got := events[0]
	if got.Kind != capture.KindEdit || got.FilePath != "/repo/a.go" {
		t.Errorf("event = %+v, want an edit of /repo/a.go", got)
	}
	if got.OldText != "old" || got.NewText != "new" {
		t.Errorf("texts = %q/%q, want old/new - the exact tier needs both", got.OldText, got.NewText)
	}
	if got.Model != "claude-opus-5" || got.SessionID != "s1" {
		t.Errorf("identity = %q/%q", got.SessionID, got.Model)
	}
	if CWD(got) != "/repo" {
		t.Errorf("cwd = %q, want /repo carried in Raw", CWD(got))
	}
	if got.TS != 1787306400000 {
		t.Errorf("ts = %d, want the RFC3339 stamp in millis", got.TS)
	}
}

func TestClaudeMultiEditYieldsOneEventPerEdit(t *testing.T) {
	line := claudeLine(`{"type":"assistant","sessionId":"s1","timestamp":"2026-08-21T10:00:00Z","cwd":"/repo",
	 "message":{"content":[{"type":"tool_use","name":"MultiEdit","input":{"file_path":"/repo/a.go","edits":[
	 {"old_string":"a","new_string":"b"},{"old_string":"c","new_string":"d"}]}}]}}`)

	events, err := (&Claude{}).ParseLine("/logs/s1.jsonl", line)
	if err != nil || len(events) != 2 {
		t.Fatalf("ParseLine() = %v, %v; want two edit events", events, err)
	}
}

// Claude Code writes files through Bash constantly, and in some configurations
// in preference to the Edit tool. Those writes must still register.
func TestClaudeBashWriteBecomesEditEvent(t *testing.T) {
	line := claudeLine(`{"type":"assistant","sessionId":"s1","timestamp":"2026-08-21T10:00:00Z","cwd":"/repo",
	 "message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"cat > internal/x.go <<'EOF'\nignored > decoy.go\nEOF\necho done"}}]}}`)

	events, err := (&Claude{}).ParseLine("/logs/s1.jsonl", line)
	if err != nil {
		t.Fatalf("ParseLine() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %v, want one - the heredoc body is data, not a command", events)
	}
	if events[0].FilePath != "/repo/internal/x.go" {
		t.Errorf("path = %q, want it resolved against cwd", events[0].FilePath)
	}
	if events[0].NewText != "" {
		t.Error("shell writes must carry no text, so only the temporal tier can claim them")
	}
}

func TestClaudeReadIsNotAnEdit(t *testing.T) {
	line := claudeLine(`{"type":"assistant","sessionId":"s1","timestamp":"2026-08-21T10:00:00Z",
	 "message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/repo/a.go"}}]}}`)
	events, _ := (&Claude{}).ParseLine("/logs/s1.jsonl", line)
	if len(events) != 1 || events[0].Kind != capture.KindRead || events[0].IsEditEvent() {
		t.Fatalf("events = %v, want one non-edit read", events)
	}
}

func TestClaudeFallsBackToFileNameForSessionID(t *testing.T) {
	line := claudeLine(`{"type":"assistant","timestamp":"2026-08-21T10:00:00Z","message":{"content":[
	 {"type":"tool_use","name":"Write","input":{"file_path":"/repo/a.go","content":"x"}}]}}`)
	events, _ := (&Claude{}).ParseLine(filepath.Join("/logs", "abc-123.jsonl"), line)
	if len(events) != 1 || events[0].SessionID != "abc-123" {
		t.Fatalf("events = %v, want the session id recovered from the file name", events)
	}
}

func TestCodexSessionMetaRegistersSession(t *testing.T) {
	line := []byte(`{"type":"session_meta","timestamp":"2026-08-21T10:00:00Z","payload":
	 {"session_id":"019ff782-de0d-7601-b731-ba4607072031","cwd":"/repo"}}`)
	events, err := (&Codex{}).ParseLine("/logs/rollout-2026-08-21T10-00-00-019ff782-de0d-7601-b731-ba4607072031.jsonl", line)
	if err != nil || len(events) != 1 {
		t.Fatalf("ParseLine() = %v, %v; want the session registered", events, err)
	}
	if events[0].SessionID != "019ff782-de0d-7601-b731-ba4607072031" || CWD(events[0]) != "/repo" {
		t.Errorf("event = %+v, want id and cwd", events[0])
	}
}

// Codex has no edit tool at all: every write goes through a shell.
func TestCodexExecCommandWriteBecomesEdit(t *testing.T) {
	line := []byte(`{"type":"response_item","timestamp":"2026-08-21T10:00:00Z","payload":
	 {"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"tee /repo/b.go\",\"workdir\":\"/repo\"}"}}`)
	events, err := (&Codex{}).ParseLine("/logs/rollout-x-019ff782-de0d-7601-b731-ba4607072031.jsonl", line)
	if err != nil || len(events) != 1 {
		t.Fatalf("ParseLine() = %v, %v", events, err)
	}
	if events[0].Kind != capture.KindEdit || events[0].FilePath != "/repo/b.go" {
		t.Errorf("event = %+v, want an edit of /repo/b.go", events[0])
	}
}

func TestCodexReadOnlyCommandStillProvesActivity(t *testing.T) {
	line := []byte(`{"type":"response_item","timestamp":"2026-08-21T10:00:00Z","payload":
	 {"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"ls -la\",\"workdir\":\"/repo\"}"}}`)
	events, _ := (&Codex{}).ParseLine("/logs/rollout-x-019ff782-de0d-7601-b731-ba4607072031.jsonl", line)
	if len(events) != 1 || events[0].IsEditEvent() {
		t.Fatalf("events = %v, want one non-edit activity event", events)
	}
}

func TestShellWritesRecognizesWriteFormsOnly(t *testing.T) {
	cases := map[string][]string{
		"echo hi > out.txt":              {"out.txt"},
		"echo hi >> out.txt":             {"out.txt"},
		"tee -a log.txt":                 {"log.txt"},
		"touch new.go":                   {"new.go"},
		"cp a.go b.go":                   {"b.go"},
		"grep -rn foo .":                 nil,
		"ls -la":                         nil,
		"echo hi >&2":                    nil,
		"cat x > /dev/null":              nil,
		"go test ./... > /dev/null 2>&1": nil,
	}
	for command, want := range cases {
		got := ShellWrites(command)
		if len(got) != len(want) {
			t.Errorf("ShellWrites(%q) = %v, want %v", command, got, want)
			continue
		}
		for index := range want {
			if got[index] != want[index] {
				t.Errorf("ShellWrites(%q) = %v, want %v", command, got, want)
				break
			}
		}
	}
}

func TestStripHeredocsRemovesBodies(t *testing.T) {
	got := StripHeredocs("cat > real.go <<'EOF'\necho fake > decoy.go\nEOF\ntouch after.go")
	if writes := ShellWrites(got); len(writes) != 2 || writes[0] != "real.go" || writes[1] != "after.go" {
		t.Fatalf("writes = %v, want [real.go after.go] - the body must not count", writes)
	}
}

// Claude Code names a session once it knows what it is about, and that name is
// what its UI shows. A rename carries no timestamp: it is not activity.
func TestClaudeAITitleCarriesTheSessionName(t *testing.T) {
	line := claudeLine(`{"type":"ai-title","sessionId":"s1","aiTitle":"How demux works"}`)
	events, err := (&Claude{}).ParseLine("/logs/s1.jsonl", line)
	if err != nil || len(events) != 1 {
		t.Fatalf("ParseLine() = %v, %v; want the title event", events, err)
	}
	if Title(events[0]) != "How demux works" {
		t.Errorf("title = %q, want the aiTitle", Title(events[0]))
	}
	if events[0].TS != 0 {
		t.Errorf("ts = %d, want zero - a rename is not activity", events[0].TS)
	}
}

// Only a format that declares itself untimed may borrow the log file's
// modification time; a touched file is not a working agent.
func TestOnlyCursorDeclaresItselfUntimed(t *testing.T) {
	if IsUntimed(NewClaude()) || IsUntimed(NewCodex()) {
		t.Error("a timestamped format must not be treated as untimed")
	}
	if !IsUntimed(NewCursor()) {
		t.Error("Cursor transcripts carry no timestamps and must declare it")
	}
}

// Quoted text is data. A `>` in a commit message, an awk program, or a
// JavaScript arrow used to be read as a redirect, and the name of a package
// manager's subcommand as the command itself.
func TestShellWritesIgnoresQuotedAndUnrelatedForms(t *testing.T) {
	cases := map[string][]string{
		`git commit -m "parse: a -> b"`:                nil,
		`git commit -m "mv foo to bar"`:                nil,
		`git commit -m "feat: install hooks on start"`: nil,
		`pip install requests numpy`:                   nil,
		`npm install --save-dev typescript ts-node`:    nil,
		`awk '$3 > 100' f`:                             nil,
		`grep -rn "a > b" .`:                           nil,
		`node -e "rows.map(r => r.fullName)"`:          nil,
		`test "$a" \> "$b"`:                            nil,
		`echo hi >out.txt`:                             {"out.txt"},
		`echo hi > "my file.txt"`:                      {"my file.txt"},
		`echo hi 2>err.log`:                            {"err.log"},
		`cmd &>all.log`:                                {"all.log"},
		`cmd >&2`:                                      nil,
		`cmd 2>&1 | tee -a run.log`:                    {"run.log"},
		`cmd > >(logger)`:                              nil,
		`FOO=1 sudo -n tee /etc/x`:                     {"/etc/x"},
		`sed -i.bak 's/a/b/' f.go`:                     {"f.go"},
		`sed --in-place -e 's/a/b/' f.go g.go`:         {"f.go", "g.go"},
		`sed -i 's/a/b/' f.go && go test ./...`:        {"f.go"},
		`sed 's/a/b/' f.go > g.go`:                     {"g.go"},
		`sed -n '1p' f.go`:                             nil,
		`cp -r src dest`:                               {"dest"},
		`cp a b -t dir`:                                nil,
		`install -d /tmp/x`:                            nil,
		`touch a.go b.go # and > nothing`:              {"a.go", "b.go"},
		`ls # echo > comment.txt`:                      nil,
		`cat > x.txt <<EOF`:                            {"x.txt"},
		`echo a > $OUT`:                                nil,
		`echo a > "$OUT"`:                              nil,
		`for f in a b; do echo $f >> out; done`:        {"out"},
		"echo one > a.txt \\\n  && echo two > b.txt":   {"a.txt", "b.txt"},
	}
	for command, want := range cases {
		got := ShellWrites(command)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("ShellWrites(%q) = %v, want %v", command, got, want)
		}
	}
}

// A << inside quotes is not a heredoc, and a here-string is not one either;
// both used to swallow every following line.
func TestStripHeredocsHonoursQuotesAndHereStrings(t *testing.T) {
	got := StripHeredocs("grep '<<' f\ntouch after.go\ncat <<< \"$x\" > y.txt\ntouch last.go\ncat <<-EOF > z.txt\n\tbody > decoy\n\tEOF\ntouch end.go")
	writes := ShellWrites(got)
	want := "after.go,y.txt,last.go,z.txt,end.go"
	if strings.Join(writes, ",") != want {
		t.Fatalf("writes = %v, want %s", writes, want)
	}
}

func TestApplyPatchEditsReadsEveryFileForm(t *testing.T) {
	input := "*** Begin Patch\n*** Update File: src/lib.rs\n*** Move to: src/lib2.rs\n@@ fn main\n pub mod a;\n-pub mod old;\n+pub mod new;\n+pub mod newer;\n*** Add File: src/fresh.rs\n+line one\n+line two\n*** Delete File: src/gone.rs\n*** End Patch\n"
	edits := ApplyPatchEdits(input)
	want := []PatchEdit{
		{Path: "src/lib.rs", Deleted: true},
		{Path: "src/lib2.rs", OldText: "pub mod old;", NewText: "pub mod new;\npub mod newer;"},
		{Path: "src/fresh.rs", NewText: "line one\nline two"},
		{Path: "src/gone.rs", Deleted: true},
	}
	if len(edits) != len(want) {
		t.Fatalf("edits = %+v, want %d", edits, len(want))
	}
	for index := range want {
		if edits[index] != want[index] {
			t.Errorf("edit %d = %+v, want %+v", index, edits[index], want[index])
		}
	}
	if got := ApplyPatchEdits("*** Begin Patch\n*** End Patch"); len(got) != 0 {
		t.Fatalf("empty patch produced %+v", got)
	}
}

// apply_patch is Codex's structured edit tool. Its edits carry text, so the
// exact and fuzzy tiers can claim them, and relative paths resolve against
// the directory the turn declared.
func TestCodexApplyPatchBecomesEditsWithText(t *testing.T) {
	parser := &Codex{}
	path := "/logs/rollout-x-019ff782-de0d-7601-b731-ba4607072031.jsonl"
	turn := []byte(`{"type":"turn_context","timestamp":"2026-08-21T10:00:00Z","payload":{"cwd":"/repo","model":"gpt-5.4"}}`)
	events, err := parser.ParseLine(path, turn)
	if err != nil || len(events) != 1 || CWD(events[0]) != "/repo" || events[0].Model != "gpt-5.4" {
		t.Fatalf("turn_context = %+v, %v; want cwd and model", events, err)
	}
	call := []byte(`{"type":"response_item","timestamp":"2026-08-21T10:00:01Z","payload":{"type":"custom_tool_call","name":"apply_patch","input":"*** Begin Patch\n*** Update File: src/a.go\n@@\n-old\n+new line\n*** Add File: /abs/b.go\n+package b\n*** End Patch"}}`)
	events, err = parser.ParseLine(path, call)
	if err != nil || len(events) != 2 {
		t.Fatalf("apply_patch = %+v, %v; want two edits", events, err)
	}
	if events[0].Kind != capture.KindEdit || events[0].FilePath != "/repo/src/a.go" || events[0].NewText != "new line" || events[0].OldText != "old" {
		t.Errorf("first edit = %+v", events[0])
	}
	if events[1].FilePath != "/abs/b.go" || events[1].NewText != "package b" {
		t.Errorf("second edit = %+v", events[1])
	}
	// A message record's id is not a session id.
	msg := []byte(`{"type":"response_item","timestamp":"2026-08-21T10:00:02Z","payload":{"type":"message","id":"msg_1"}}`)
	if events, _ := (&Codex{}).ParseLine("/logs/plain.jsonl", msg); len(events) != 0 {
		t.Fatalf("message id registered a session: %+v", events)
	}
}
