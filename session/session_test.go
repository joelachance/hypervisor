package session

import "testing"

import "github.com/satoricorp/demux/capture"

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
