package main

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// The notifications below are the ones codex-cli 0.165.0 sent for one parent
// turn that spawned a subagent, waited for it and closed it, trimmed to the
// fields the translator reads. The child's own turn arrives on the same
// connection, naming the child's thread.
const (
	parentThread = "01a18959-3e41-72e1-89b1-7a8c7178f6b5"
	childThread  = "01a18959-5183-7253-9293-7dffbc98dfea"
)

func subagentActivity(id, kind string) json.RawMessage {
	return json.RawMessage(`{"threadId":"` + parentThread + `","turnId":"turn-parent","item":{"type":"subAgentActivity","id":"` + id +
		`","kind":"` + kind + `","agentThreadId":"` + childThread + `","agentPath":"/root/hello"}}`)
}

func TestSubagentActivityBecomesTaskFramesAndChildThreadStaysOut(t *testing.T) {
	var events []msg.Event
	translator := NewTranslator("br_test", "req_test", func(event msg.Event) {
		events = append(events, event)
	})
	translator.SetSessionID(parentThread)
	server := NewAppServer("", 0, "")
	translator.RegisterHandlers(server)
	send := func(method string, params json.RawMessage) { server.notifHandlers[method]("", params) }

	// Each subAgentActivity item comes twice, on item/started and on
	// item/completed.
	send("item/started", subagentActivity("call_spawn", "started"))
	send("item/completed", subagentActivity("call_spawn", "started"))
	send("turn/started", json.RawMessage(`{"threadId":"`+childThread+`","turn":{"id":"turn-child"}}`))
	send("item/completed", json.RawMessage(`{"threadId":"`+childThread+`","turnId":"turn-child","item":{"type":"agentMessage","id":"msg-child","text":"hello","phase":"final_answer"}}`))
	send("turn/completed", json.RawMessage(`{"threadId":"`+childThread+`","turn":{"id":"turn-child","status":"completed","durationMs":2281}}`))
	send("item/started", subagentActivity("subagent-completed-turn-child", "completed"))
	send("item/completed", subagentActivity("subagent-completed-turn-child", "completed"))
	send("item/started", subagentActivity("call_close", "interrupted"))
	send("item/completed", subagentActivity("call_close", "interrupted"))
	send("turn/completed", json.RawMessage(`{"threadId":"`+parentThread+`","turn":{"id":"turn-parent","status":"completed","durationMs":9000}}`))

	var tasks []*msg.SystemEvent
	results := 0
	for _, event := range events {
		if event.HarnessSessionID != parentThread {
			t.Errorf("event %s carries harness session %q, want the parent's %q", event.Type, event.HarnessSessionID, parentThread)
		}
		switch {
		case event.Type == msg.EventResult:
			results++
		case event.Type == msg.EventSystem && event.System.Subtype == "agent_message_complete":
			t.Errorf("the child's message %q reached the parent's stream", event.System.Message)
		case event.Type == msg.EventSystem && (event.System.Subtype == "task_started" || event.System.Subtype == "task_notification"):
			tasks = append(tasks, event.System)
		}
	}
	if results != 1 {
		t.Errorf("got %d result events, want 1: the child's turn/completed must not end the parent's turn", results)
	}
	if len(tasks) != 3 {
		t.Fatalf("got %d task frames, want 3 (started, completed, cancelled): %+v", len(tasks), tasks)
	}
	started := tasks[0]
	if started.Subtype != "task_started" || started.TaskID != childThread || started.ToolUseID != "call_spawn" ||
		started.TaskType != msg.TaskTypeLocalAgent || started.SubagentHarnessSessionID != childThread ||
		started.Description != "/root/hello" {
		t.Errorf("task_started = %+v", started)
	}
	if started.SubagentSessionID != "" {
		t.Errorf("subagent_session_id = %q; only the server may set it", started.SubagentSessionID)
	}
	if tasks[1].Subtype != "task_notification" || tasks[1].TaskID != childThread || tasks[1].TaskStatus != msg.TaskStatusCompleted {
		t.Errorf("second task frame = %+v, want task_notification completed", tasks[1])
	}
	if tasks[2].TaskStatus != msg.TaskStatusCancelled {
		t.Errorf("third task frame = %+v, want task_notification cancelled", tasks[2])
	}
}

// Before SetSessionID names the session's thread, nothing is known to be
// another thread's, so nothing is dropped.
func TestNotificationsPassBeforeTheThreadIsKnown(t *testing.T) {
	var events []msg.Event
	translator := NewTranslator("br_test", "req_test", func(event msg.Event) {
		events = append(events, event)
	})
	server := NewAppServer("", 0, "")
	translator.RegisterHandlers(server)
	server.notifHandlers["thread/status/changed"]("", json.RawMessage(`{"threadId":"`+childThread+`","status":{"type":"idle"}}`))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
}
