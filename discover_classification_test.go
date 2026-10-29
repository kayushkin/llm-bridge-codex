package main

import (
	"github.com/kayushkin/llm-bridge/msg"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiscoverExplicitProvenance(t *testing.T) {
	cases := []struct {
		name, payload, initial, purpose, parent string
		fails                                   bool
	}{
		{"interactive", `{"source":"cli","thread_source":"user"}`, "", msg.PurposeChat, "", false},
		{"subagent", `{"source":{"subagent":{"other":"guardian"}},"thread_source":"guardian_review","parent_thread_id":"native-parent"}`, "", msg.PurposeSubagent, "native-parent", false},
		{"subagent takes precedence", `{"source":{"subagent":"review"},"thread_source":"user"}`, "", msg.PurposeSubagent, "", false},
		{"fork is not subagent", `{"source":"cli","parent_thread_id":"native-parent"}`, "", "", "", false},
		{"unknown", `{"source":"exec"}`, "", "", "", false},
		{"missing", `{}`, "", "", "", false},
		{"conformance retained", `{"thread_source":"user"}`, "conformance", "conformance", "", false},
		{"invalid metadata", `{"thread_source":42}`, "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollout.jsonl")
			if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":`+tc.payload+`}`+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			s := msg.StoredSession{Path: path, Source: tc.initial}
			err := classifyStoredSession(&s)
			if (err != nil) != tc.fails {
				t.Fatalf("error = %v", err)
			}
			if s.Source != tc.purpose || s.ParentHarnessSessionID != tc.parent {
				t.Fatalf("classification = %q, parent = %q", s.Source, s.ParentHarnessSessionID)
			}
		})
	}
}

func TestChangedSessionsRetainExplicitHumanProvenance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".codex", "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	id := "01a156b2-6866-7ed2-bc4e-e1441e9e72f3"
	path := filepath.Join(dir, "rollout-2026-10-21T00-00-00-"+id+".jsonl")
	row := `{"timestamp":"2026-10-21T00:00:00Z","type":"session_meta","payload":{"id":"` + id + `","timestamp":"2026-10-21T00:00:00Z","thread_source":"user","source":"cli"}}` + "\n"
	if err := os.WriteFile(path, []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	sessions, err := discoverSessionsChangedSince(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("changed sessions = %d, want 1", len(sessions))
	}
	if sessions[0].Source != msg.PurposeChat || sessions[0].HarnessSessionID != id {
		t.Fatalf("changed session lost provenance: %+v", sessions[0])
	}
}
