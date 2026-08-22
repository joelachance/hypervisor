package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/satoricorp/demux/capture"
)

func TestIndexObserveSummarizesEdits(t *testing.T) {
	index := NewIndex()
	index.Observe(capture.SessionEvent{SessionID: "s1", Tool: capture.ToolClaude, TS: 100, Kind: capture.KindMessage})
	index.Observe(capture.SessionEvent{SessionID: "s1", Tool: capture.ToolClaude, TS: 200, Kind: capture.KindEdit, FilePath: "b.go", Model: "opus"})
	index.Observe(capture.SessionEvent{SessionID: "s1", Tool: capture.ToolClaude, TS: 300, Kind: capture.KindEdit, FilePath: "a.go"})
	index.Observe(capture.SessionEvent{SessionID: "s1", Tool: capture.ToolClaude, TS: 400, Kind: capture.KindEdit, FilePath: "a.go"})

	got, ok := index.Get("s1")
	if !ok {
		t.Fatal("session s1 not indexed")
	}
	if got.EventCount != 4 || got.EditCount != 3 {
		t.Fatalf("counts = %d events / %d edits, want 4/3", got.EventCount, got.EditCount)
	}
	if got.StartedAt != 100 || got.LastActiveAt != 400 {
		t.Fatalf("window = %d..%d, want 100..400", got.StartedAt, got.LastActiveAt)
	}
	if got.Model != "opus" {
		t.Fatalf("model = %q, want opus", got.Model)
	}
	if len(got.Files) != 2 || got.Files[0] != "a.go" || got.Files[1] != "b.go" {
		t.Fatalf("files = %v, want [a.go b.go] deduplicated and sorted", got.Files)
	}
}

func TestSessionsMostRecentFirst(t *testing.T) {
	index := NewIndex()
	index.Observe(capture.SessionEvent{SessionID: "old", TS: 10, Kind: capture.KindEdit, FilePath: "x.go"})
	index.Observe(capture.SessionEvent{SessionID: "new", TS: 20, Kind: capture.KindEdit, FilePath: "y.go"})

	sessions := index.Sessions()
	if len(sessions) != 2 || sessions[0].ID != "new" {
		t.Fatalf("sessions = %v, want most recently active first", sessions)
	}
}

func TestStateFromSilence(t *testing.T) {
	now := int64(10_000_000)
	cases := []struct {
		age  int64
		want State
	}{
		{0, StateLive},
		{LiveWindow - 1, StateLive},
		{LiveWindow + 1, StateIdle},
		{IdleWindow + 1, StateEnded},
	}
	for _, testCase := range cases {
		got := Session{LastActiveAt: now - testCase.age}.State(now)
		if got != testCase.want {
			t.Errorf("session silent for %dms = %q, want %q", testCase.age, got, testCase.want)
		}
	}
}

func TestObserveRecordsRepoRootAndSummary(t *testing.T) {
	index := NewIndex()
	index.Observe(capture.SessionEvent{
		SessionID:     "s1",
		Kind:          capture.KindMessage,
		TS:            1,
		PromptContext: "fix the billing rounding bug\nsecond line ignored",
		Raw:           rawCWD("/repo"),
	})
	got, _ := index.Get("s1")
	if got.RepoRoot != "/repo" {
		t.Errorf("repo root = %q, want /repo", got.RepoRoot)
	}
	if got.Summary != "fix the billing rounding bug" {
		t.Errorf("summary = %q, want the first prompt line only", got.Summary)
	}
}

func TestSummaryRedactsSecrets(t *testing.T) {
	index := NewIndex()
	index.Observe(capture.SessionEvent{
		SessionID:     "s1",
		Kind:          capture.KindMessage,
		TS:            1,
		PromptContext: "deploy with ghp_012345678901234567890123456789012345",
	})
	got, _ := index.Get("s1")
	if strings.Contains(got.Summary, "ghp_012345678901234567890123456789012345") {
		t.Fatalf("summary = %q; a session summary rides onto plans and must be redacted", got.Summary)
	}
}

func rawCWD(dir string) map[string]json.RawMessage {
	encoded, _ := json.Marshal(dir)
	return map[string]json.RawMessage{"cwd": encoded}
}
