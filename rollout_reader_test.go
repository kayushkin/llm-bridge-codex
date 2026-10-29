package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// The fixtures are trimmed copies of entries codex wrote on this host: the
// 2026-04 dialect (event_msg user_message / agent_message) and the 2026-10 one
// (event_msg item_completed).

const rolloutBeforeSeptember = `{"timestamp":"2026-04-12T02:06:48.100Z","type":"session_meta","payload":{"id":"019d7f71-0e3e-7bb3-9cb3-b7e620e5810e","cwd":"/tmp","timestamp":"2026-04-12T02:06:48.000Z"}}
{"timestamp":"2026-04-12T02:06:48.200Z","type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/tmp"}}
{"timestamp":"2026-04-12T02:06:48.300Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"the host prompt, which nobody typed"}]}}
{"timestamp":"2026-04-12T02:06:48.400Z","type":"event_msg","payload":{"type":"user_message","message":"What is 2+2?","images":[]}}
{"timestamp":"2026-04-12T02:06:49.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"echo 4\"}","call_id":"call_1"}}
{"timestamp":"2026-04-12T02:06:49.100Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"4\n"}}
{"timestamp":"2026-04-12T02:06:49.200Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":10,"output_tokens":5,"reasoning_output_tokens":1,"total_tokens":105},"total_token_usage":{"total_tokens":105},"model_context_window":258400}}}
{"timestamp":"2026-04-12T02:06:49.300Z","type":"event_msg","payload":{"type":"agent_message","message":"4","phase":"final_answer"}}
{"timestamp":"2026-04-12T02:06:49.400Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1","last_agent_message":"4","duration_ms":1441}}
`

const rolloutSinceSeptember = `{"timestamp":"2026-10-20T00:24:55.000Z","type":"session_meta","payload":{"id":"01a156b2-6866-7ed2-bc4e-e1441e9e72f3","cwd":"/tmp","timestamp":"2026-10-20T00:24:55.000Z"}}
{"timestamp":"2026-10-20T00:24:55.100Z","type":"turn_context","payload":{"model":"gpt-5.6-luna","cwd":"/tmp"}}
{"timestamp":"2026-10-20T00:24:55.200Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}
{"timestamp":"2026-10-20T00:24:55.300Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<recommended_plugins> injected context </recommended_plugins>"}]}}
{"timestamp":"2026-10-20T00:24:55.400Z","type":"event_msg","payload":{"type":"item_completed","thread_id":"01a156b2-6866-7ed2-bc4e-e1441e9e72f3","item":{"type":"UserMessage","id":"u1","content":[{"type":"text","text":"Remember ZEBRA1."}]}}}
{"timestamp":"2026-10-20T00:24:56.000Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_2","name":"exec","input":"ls"}}
{"timestamp":"2026-10-20T00:24:56.100Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_2","output":[{"type":"input_text","text":"Script completed\n"},{"type":"input_text","text":"a b"}]}}
{"timestamp":"2026-10-20T00:24:56.500Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"AgentMessage","id":"m1","content":[{"type":"Text","text":"OK"}],"phase":"final_answer"}}}
{"timestamp":"2026-10-20T00:24:56.600Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1","last_agent_message":"OK","duration_ms":900}}
{"timestamp":"2026-10-20T00:25:10.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","id":"u2","content":[{"type":"text","text":"never mind"}]}}}
{"timestamp":"2026-10-20T00:25:11.000Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"t2","reason":"interrupted","duration_ms":1000}}
`

func readAll(t *testing.T, rollout string) []msg.Event {
	t.Helper()
	var events []msg.Event
	if err := translateRolloutStream("", strings.NewReader(rollout), func(ev msg.Event) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatalf("translateRolloutStream: %v", err)
	}
	return events
}

func eventTypes(events []msg.Event) []msg.EventType {
	types := make([]msg.EventType, len(events))
	for i, ev := range events {
		types[i] = ev.Type
	}
	return types
}

func TestRolloutBeforeSeptemberDialect(t *testing.T) {
	events := readAll(t, rolloutBeforeSeptember)
	want := []msg.EventType{msg.EventUserMessage, msg.EventToolCall, msg.EventToolResult, msg.EventBlock, msg.EventResult}
	if got := eventTypes(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("event types = %v, want %v (the developer prompt must not appear as a message)", got, want)
	}
	if events[0].Result.Text != "What is 2+2?" {
		t.Errorf("user message = %q", events[0].Result.Text)
	}
	if string(events[1].ToolCall.Input) != `{"cmd":"echo 4"}` || events[2].ToolResult.Name != "exec_command" || events[2].ToolResult.Output != "4\n" {
		t.Errorf("tool call/result = %+v / %+v", events[1].ToolCall, events[2].ToolResult)
	}
	result := events[4].Result
	if result.Text != "4" || result.Model != "gpt-5.4" || result.Usage.InputTokens != 90 || result.Usage.CacheReadTokens != 10 || result.Usage.ContextTokens != 100 || result.Usage.ContextLimit != 258400 {
		t.Errorf("result = %+v", result)
	}
	for _, ev := range events {
		if ev.HarnessSessionID != "019d7f71-0e3e-7bb3-9cb3-b7e620e5810e" || ev.Harness != msg.HarnessCodex || string(ev.Extensions["source"]) != `"rollout"` {
			t.Fatalf("event %s not stamped with thread, harness and source: %+v", ev.Type, ev)
		}
	}
	if !events[0].Timestamp.Equal(time.Date(2026, 4, 12, 2, 6, 48, 400_000_000, time.UTC)) {
		t.Errorf("timestamp = %s, want the entry's own", events[0].Timestamp)
	}
}

func TestRolloutSinceSeptemberDialect(t *testing.T) {
	events := readAll(t, rolloutSinceSeptember)
	want := []msg.EventType{msg.EventUserMessage, msg.EventToolCall, msg.EventToolResult, msg.EventBlock, msg.EventResult, msg.EventUserMessage, msg.EventResult}
	if got := eventTypes(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("event types = %v, want %v (injected user-role context must not appear as a message)", got, want)
	}
	if events[0].Result.Text != "Remember ZEBRA1." {
		t.Errorf("user message = %q", events[0].Result.Text)
	}
	if events[2].ToolResult.Output != "Script completed\na b" {
		t.Errorf("array tool output read as %q", events[2].ToolResult.Output)
	}
	if events[3].Block.Block.Text.Text != "OK" || events[4].Result.Text != "OK" || events[4].Result.Model != "gpt-5.6-luna" {
		t.Errorf("answer = %+v / %+v", events[3].Block, events[4].Result)
	}
	aborted := events[6]
	if !aborted.Result.IsError || aborted.Error == nil || aborted.Error.Code != "TURN_ABORTED" {
		t.Errorf("an aborted turn must still end the turn and say why: %+v", aborted)
	}
}

func TestRolloutLineThatIsNotJSONStopsTheImport(t *testing.T) {
	err := translateRolloutStream("", strings.NewReader(rolloutSinceSeptember+"{not json\n"), func(msg.Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "line 12") {
		t.Fatalf("err = %v, want one naming line 12", err)
	}
}

func TestPTYArguments(t *testing.T) {
	cases := []struct {
		model, resume string
		want          []string
	}{
		{"", "", []string{"--no-daemon"}},
		{"gpt-5.6-luna", "", []string{"--no-daemon", "--model", "gpt-5.6-luna"}},
		{"gpt-5.6-luna", "01a156b2-6866-7ed2-bc4e-e1441e9e72f3", []string{"--no-daemon", "--model", "gpt-5.6-luna", "resume", "01a156b2-6866-7ed2-bc4e-e1441e9e72f3"}},
		{"", "01a156b2-6866-7ed2-bc4e-e1441e9e72f3", []string{"--no-daemon", "resume", "01a156b2-6866-7ed2-bc4e-e1441e9e72f3"}},
	}
	for _, c := range cases {
		if got := ptyArguments(c.model, c.resume); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ptyArguments(%q, %q) = %q, want %q", c.model, c.resume, got, c.want)
		}
	}
}

// A resumed rollout already holds turns the dash has; following it must post
// only what codex appends.
func TestFollowRolloutPostsOnlyWhatIsAppendedOnResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(rolloutSinceSeptember), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var posted []msg.Event
	post := func(ev msg.Event) { mu.Lock(); posted = append(posted, ev); mu.Unlock() }
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- followRollout(path, "01a156b2-6866-7ed2-bc4e-e1441e9e72f3", true, post, stop) }()

	time.Sleep(300 * time.Millisecond)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Written in two pieces, as codex can be caught mid-line.
	f.WriteString(`{"timestamp":"2026-10-20T00:30:00.000Z","type":"event_msg","payload":{"type":"user_message",`)
	time.Sleep(300 * time.Millisecond)
	f.WriteString(`"message":"typed in the pty"}}` + "\n")
	f.Close()
	time.Sleep(500 * time.Millisecond)
	close(stop)
	if err := <-done; err != nil {
		t.Fatalf("followRollout: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 1 || posted[0].Type != msg.EventUserMessage || posted[0].Result.Text != "typed in the pty" {
		t.Fatalf("posted %d events %v, want only the appended user message", len(posted), eventTypes(posted))
	}
	if posted[0].HarnessSessionID != "01a156b2-6866-7ed2-bc4e-e1441e9e72f3" {
		t.Errorf("HarnessSessionID = %q", posted[0].HarnessSessionID)
	}
}

func TestNewRolloutForMatchesTheSessionsDirectoryAndStart(t *testing.T) {
	sessionsDir := t.TempDir()
	now := time.Now()
	dir := filepath.Join(sessionsDir, now.Format("2006"), now.Format("01"), now.Format("02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	since := now.Add(-time.Minute)
	write := func(name, cwd string, created time.Time) {
		line := `{"timestamp":"x","type":"session_meta","payload":{"id":"t","cwd":"` + cwd + `","timestamp":"` + created.UTC().Format(time.RFC3339Nano) + `"}}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("rollout-old.jsonl", "/tmp", since.Add(-time.Hour))
	write("rollout-elsewhere.jsonl", "/home", now)
	if got := newRolloutFor(sessionsDir, "/tmp", since); got != "" {
		t.Fatalf("found %s, want nothing: one is older than the session, the other in another directory", got)
	}
	write("rollout-ours.jsonl", "/tmp", now)
	if got := newRolloutFor(sessionsDir, "/tmp", since); filepath.Base(got) != "rollout-ours.jsonl" {
		t.Fatalf("found %q, want rollout-ours.jsonl", got)
	}
}
