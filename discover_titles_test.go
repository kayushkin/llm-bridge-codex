package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// The fixtures below follow the shape codex writes: a session_meta line, then
// role "user" response_items that codex writes itself (the environment and the
// AGENTS.md instructions it loaded) ahead of what the person typed, which a
// codex that has it also records as an event_msg of type user_message.

const agentsInstructionsText = "# AGENTS.md instructions for /work/repo\n\n<INSTRUCTIONS>\n# Directives\n\n**Use the requested UI source.** When asked to …\n</INSTRUCTIONS>"

func rolloutLine(t *testing.T, entryType string, payload map[string]any) string {
	t.Helper()
	line, err := json.Marshal(map[string]any{"timestamp": "2026-10-27T17:00:00Z", "type": entryType, "payload": payload})
	if err != nil {
		t.Fatalf("marshal %s line: %v", entryType, err)
	}
	return string(line)
}

func userItem(t *testing.T, text string) string {
	return rolloutLine(t, "response_item", map[string]any{
		"type": "message", "role": "user",
		"content": []map[string]any{{"type": "input_text", "text": text}},
	})
}

func writeRollout(t *testing.T, dir, id string, lines ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "rollout-2026-10-27T17-00-00-"+id+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func sessionMeta(t *testing.T, id, cwd string, source any) string {
	return rolloutLine(t, "session_meta", map[string]any{
		"id": id, "timestamp": "2026-10-27T17:00:00Z", "cwd": cwd,
		"originator": "codex-tui", "source": source,
	})
}

func TestParseCodexSessionTitlesFromWhatThePersonTyped(t *testing.T) {
	dir := t.TempDir()
	id := "01a17f13-5a13-7950-be14-de83e37aff1d"
	path := writeRollout(t, dir, id,
		sessionMeta(t, id, "/work/repo", "vscode"),
		userItem(t, "<environment_context>\n  <cwd>/work/repo</cwd>\n</environment_context>"),
		userItem(t, agentsInstructionsText),
		rolloutLine(t, "event_msg", map[string]any{"type": "user_message", "message": "Which are the recent orders?", "images": []any{}}),
		userItem(t, "Which are the recent orders?"),
		rolloutLine(t, "response_item", map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": "Here they are."}}}),
		userItem(t, "<turn_aborted>\nThe user interrupted the previous turn.\n</turn_aborted>"),
		rolloutLine(t, "event_msg", map[string]any{"type": "user_message", "message": "And yesterday's?"}),
		userItem(t, "And yesterday's?"),
	)

	gotID, prompt, _, cwd, turns := parseCodexSession(path)
	if gotID != id {
		t.Errorf("id = %q, want %q", gotID, id)
	}
	if prompt != "Which are the recent orders?" {
		t.Errorf("prompt = %q, want what the person typed", prompt)
	}
	if cwd != "/work/repo" {
		t.Errorf("cwd = %q, want /work/repo", cwd)
	}
	if turns != 2 {
		t.Errorf("turns = %d, want 2: the instructions, the environment and the aborted-turn marker are codex's own", turns)
	}
}

// A codex that writes no user_message events still gets a real title: the
// response_items are read, skipping every item codex writes itself.
func TestParseCodexSessionWithoutUserMessageEventsSkipsCodexText(t *testing.T) {
	dir := t.TempDir()
	id := "01a18402-7517-7a20-bc8d-5ddcfa3ad290"
	path := writeRollout(t, dir, id,
		sessionMeta(t, id, "/work/repo", "exec"),
		userItem(t, agentsInstructionsText),
		userItem(t, "<environment_context>\n  <cwd>/work/repo</cwd>\n</environment_context>"),
		userItem(t, "Add a function that parses dates"),
	)

	_, prompt, _, _, turns := parseCodexSession(path)
	if prompt != "Add a function that parses dates" {
		t.Errorf("prompt = %q, want the first message codex did not write itself", prompt)
	}
	if turns != 1 {
		t.Errorf("turns = %d, want 1", turns)
	}
}

// A guardian subagent's rollout is discovered like any other, with its own
// request as its title and its working folder reported as the project.
func TestDiscoverReportsAGuardianSubagentWithItsRequestAndFolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	id := "01a17f13-6000-7950-be14-de83e37aff1d"
	writeRollout(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "27"), id,
		sessionMeta(t, id, "/work/repo", map[string]any{"subagent": map[string]any{"other": "guardian"}}),
		userItem(t, agentsInstructionsText),
		userItem(t, "Review this command before it runs: rm -rf build/"),
	)

	sessions, err := discoverSessions()
	if err != nil {
		t.Fatalf("discoverSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("discovered %d sessions, want 1", len(sessions))
	}
	got := sessions[0]
	if got.Prompt != "Review this command before it runs: rm -rf build/" {
		t.Errorf("Prompt = %q", got.Prompt)
	}
	if got.Project != "/work/repo" {
		t.Errorf("Project = %q, want the rollout's cwd", got.Project)
	}
	if msg.IsCodexInjectedText(got.Prompt) {
		t.Errorf("Prompt %q is codex's own text", got.Prompt)
	}
}

// CODEX_HOME moves every rollout tree, and archived conversations are
// discovered too, by both -discover and -changed-since.
func TestDiscoverReadsCodexHomeAndArchivedSessions(t *testing.T) {
	home := t.TempDir()
	codexHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)

	liveID := "01a17f13-7000-7950-be14-de83e37aff1d"
	archivedID := "01a17f13-8000-7950-be14-de83e37aff1d"
	strayID := "01a17f13-9000-7950-be14-de83e37aff1d"
	writeRollout(t, filepath.Join(codexHome, "sessions", "2026", "10", "27"), liveID,
		sessionMeta(t, liveID, "/work/live", "cli"), userItem(t, "a live conversation"))
	writeRollout(t, filepath.Join(codexHome, "archived_sessions"), archivedID,
		sessionMeta(t, archivedID, "/work/archived", "cli"), userItem(t, "an archived conversation"))
	// Under ~/.codex, which CODEX_HOME replaces: codex would never read it.
	writeRollout(t, filepath.Join(home, ".codex", "sessions"), strayID,
		sessionMeta(t, strayID, "/work/stray", "cli"), userItem(t, "not codex's"))

	changed, err := discoverSessionsChangedSince(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("discoverSessionsChangedSince: %v", err)
	}
	if got := promptsByHarnessID(changed); len(got) != 2 || got[liveID] != "a live conversation" || got[archivedID] != "an archived conversation" {
		t.Errorf("-changed-since found %v, want the live and the archived conversation under CODEX_HOME only", got)
	}

	sessions, err := discoverSessions()
	if err != nil {
		t.Fatalf("discoverSessions: %v", err)
	}
	if got := promptsByHarnessID(sessions); len(got) != 2 || got[liveID] == "" || got[archivedID] == "" {
		t.Errorf("-discover found %v, want the live and the archived conversation under CODEX_HOME only", got)
	}
}

func promptsByHarnessID(sessions []msg.StoredSession) map[string]string {
	out := map[string]string{}
	for _, s := range sessions {
		out[s.HarnessSessionID] = s.Prompt
	}
	return out
}
