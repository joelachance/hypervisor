package parse

import (
	"path/filepath"
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
