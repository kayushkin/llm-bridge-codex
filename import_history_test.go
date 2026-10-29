package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestImportCodexHistoryPreservesRecordsAndAvoidsMirroredMessages(t *testing.T) {
	payloads := []string{
		`"session_meta", "payload":{"id":"session"}`,
		`"event_msg", "payload":{"type":"task_started","turn_id":"turn"}`,
		`"turn_context", "payload":{"turn_id":"turn","summary":"auto"}`,
		`"response_item", "payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"actual prompt"}]}`,
		`"event_msg", "payload":{"type":"user_message","message":"actual prompt"}`,
		`"response_item", "payload":{"type":"message","id":"native","role":"assistant","content":[{"type":"output_text","text":"actual answer"}]}`,
		`"event_msg", "payload":{"type":"agent_message","message":"actual answer"}`,
		`"response_item", "payload":{"type":"function_call","call_id":"call","name":"shell","arguments":"{\"cmd\":\"echo hi\"}"}`,
		`"response_item", "payload":{"type":"function_call_output","call_id":"call","output":"hi"}`,
		`"event_msg", "payload":{"type":"task_complete","turn_id":"turn","last_agent_message":"actual answer"}`,
		`"future_record", "payload":{"field":"must survive"}`,
	}
	var input strings.Builder
	for _, p := range payloads {
		input.WriteString(`{"timestamp":"2026-09-29T00:00:00Z","type":` + p + "}\n")
	}
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	os.WriteFile(path, []byte(input.String()), 0600)
	var out bytes.Buffer
	if err := importCodexHistory("session", path, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	originals := strings.Split(strings.TrimSpace(input.String()), "\n")
	if len(lines) != len(originals) {
		t.Fatalf("lost records: %d vs %d", len(lines), len(originals))
	}
	counts := map[msg.EventType]int{}
	for i, line := range lines {
		var ev msg.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		var original, raw any
		json.Unmarshal([]byte(originals[i]), &original)
		json.Unmarshal(ev.Raw, &raw)
		a, _ := json.Marshal(original)
		b, _ := json.Marshal(raw)
		if !bytes.Equal(a, b) {
			t.Fatalf("raw record %d changed", i)
		}
		counts[ev.Type]++
		if ev.Type == msg.EventUserMessage && ev.Result.Text != "actual prompt" {
			t.Fatal("user response item lost")
		}
		if i >= 1 && ev.TurnID != "turn" {
			t.Fatalf("lost turn ID on %d", i)
		}
		if ev.MessageID != "" {
			t.Fatal("adapter invented bridge message ID")
		}
		if ev.ToolResult != nil && (ev.ToolResult.ToolID != "call" || ev.ToolResult.Name != "shell" || ev.ToolResult.Output != "hi") {
			t.Fatal("tool pairing lost")
		}
	}
	if counts[msg.EventUserMessage] != 1 || counts[msg.EventBlock] != 1 || counts[msg.EventResult] != 1 {
		t.Fatalf("duplicated or lost messages: %v", counts)
	}
}

func TestImportCodexHistoryRejectsCorruptionAndWrongSession(t *testing.T) {
	for _, input := range []string{
		`{"timestamp":"2026-09-29T00:00:00Z","type":"session_meta","payload":{"id":"other"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"missing timestamp"}}`,
		`{broken`,
	} {
		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		os.WriteFile(path, []byte(input), 0600)
		if err := importCodexHistory("session", path, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted invalid input: %s", input)
		}
	}
}

func TestImportCodexHistoryDoesNotTruncateLongToolOutputs(t *testing.T) {
	output := strings.Repeat("世界", 200000)
	row := map[string]any{"timestamp": "2026-09-29T00:00:00Z", "type": "response_item", "payload": map[string]any{"type": "custom_tool_call_output", "call_id": "c", "output": output}}
	data, _ := json.Marshal(row)
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	os.WriteFile(path, data, 0600)
	var out bytes.Buffer
	if err := importCodexHistory("session", path, &out); err != nil {
		t.Fatal(err)
	}
	var ev msg.Event
	if err := json.Unmarshal(out.Bytes(), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.ToolResult.Output != output {
		t.Fatal("tool output was truncated")
	}
}

func TestImportCodexHistoryDefersUnfinishedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	first := `{"timestamp":"2026-09-29T00:00:00Z","type":"session_meta","payload":{"id":"session"}}` + "\n"
	partial := `{"timestamp":"2026-09-29T00:00:01Z","type":"response_item","payload":`
	if err := os.WriteFile(path, []byte(first+partial), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := importCodexHistory("session", path, &out); err != nil {
		t.Fatal(err)
	}
	if len(bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))) != 1 {
		t.Fatal("unfinished record emitted")
	}
	if err := os.WriteFile(path, []byte(first+partial+`{"type":"message","role":"user","content":[{"type":"input_text","text":"finished"}]}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := importCodexHistory("session", path, &out); err != nil {
		t.Fatal(err)
	}
	if len(bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))) != 2 {
		t.Fatal("finished record missing")
	}
}
