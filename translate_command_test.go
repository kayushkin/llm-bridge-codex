package main

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestGenericCommandLifecycleEmitsToolEvents(t *testing.T) {
	var events []msg.Event
	translator := NewTranslator("br_test", "req_test", func(event msg.Event) {
		events = append(events, event)
	})
	server := NewAppServer("", 0, "")
	translator.RegisterHandlers(server)

	started := json.RawMessage(`{
		"threadId":"thread-1",
		"turnId":"turn-1",
		"item":{"type":"commandExecution","id":"exec-1","command":"pwd","status":"inProgress"}
	}`)
	completed := json.RawMessage(`{
		"threadId":"thread-1",
		"turnId":"turn-1",
		"item":{"type":"commandExecution","id":"exec-1","command":"pwd","aggregatedOutput":"/tmp/project\n","exitCode":0,"status":"completed"}
	}`)

	server.notifHandlers["item/started"]("", started)
	server.notifHandlers["item/completed"]("", completed)

	var call *msg.ToolCallEvent
	var result *msg.ToolResultEvent
	for i := range events {
		if events[i].ToolCall != nil {
			call = events[i].ToolCall
		}
		if events[i].ToolResult != nil {
			result = events[i].ToolResult
		}
	}
	if call == nil || call.ToolID != "exec-1" || call.Name != "command_execution" {
		t.Fatalf("tool call = %#v", call)
	}
	if string(call.Input) != `{"command":"pwd"}` {
		t.Fatalf("tool call input = %s", call.Input)
	}
	if result == nil || result.ToolID != "exec-1" || result.Output != "/tmp/project\n" || result.IsError {
		t.Fatalf("tool result = %#v", result)
	}
	if got := translator.toolCallCount("thread-1"); got != 1 {
		t.Fatalf("tool call count = %d, want 1", got)
	}
}

func TestCommandLifecycleDeduplicatesGenericAndLegacyNotifications(t *testing.T) {
	var calls, results int
	translator := NewTranslator("br_test", "req_test", func(event msg.Event) {
		if event.ToolCall != nil {
			calls++
		}
		if event.ToolResult != nil {
			results++
		}
	})
	server := NewAppServer("", 0, "")
	translator.RegisterHandlers(server)

	server.notifHandlers["item/started"]("", json.RawMessage(`{
		"threadId":"thread-1","item":{"type":"commandExecution","id":"exec-1","command":"pwd"}
	}`))
	server.notifHandlers["item/commandExecution/started"]("", json.RawMessage(`{
		"threadId":"thread-1","itemId":"exec-1","command":"pwd"
	}`))
	server.notifHandlers["item/completed"]("", json.RawMessage(`{
		"threadId":"thread-1","item":{"type":"commandExecution","id":"exec-1","aggregatedOutput":"ok","exitCode":0}
	}`))
	server.notifHandlers["item/commandExecution/completed"]("", json.RawMessage(`{
		"threadId":"thread-1","itemId":"exec-1","output":"ok","exitCode":0
	}`))

	if calls != 1 || results != 1 {
		t.Fatalf("tool events: calls=%d results=%d, want 1 each", calls, results)
	}
	if got := translator.toolCallCount("thread-1"); got != 1 {
		t.Fatalf("tool call count = %d, want 1", got)
	}
}

func TestGenericToolItemsAreClassified(t *testing.T) {
	tests := []struct {
		name       string
		started    string
		completed  string
		toolName   string
		wantOutput string
		wantError  bool
	}{
		{
			name:       "file change",
			started:    `{"type":"fileChange","id":"file-1","changes":[{"path":"a.txt","kind":"add","diff":"+hello"}],"status":"inProgress"}`,
			completed:  `{"type":"fileChange","id":"file-1","changes":[{"path":"a.txt","kind":"add","diff":"+hello"}],"status":"completed"}`,
			toolName:   "file_change",
			wantOutput: `[{"path":"a.txt","kind":"add","diff":"+hello"}]`,
		},
		{
			name:       "mcp call",
			started:    `{"type":"mcpToolCall","id":"mcp-1","server":"github","tool":"get_issue","arguments":{"number":42},"status":"inProgress"}`,
			completed:  `{"type":"mcpToolCall","id":"mcp-1","server":"github","tool":"get_issue","arguments":{"number":42},"result":{"content":[{"type":"text","text":"found"}],"structuredContent":null,"_meta":null},"status":"completed"}`,
			toolName:   "get_issue",
			wantOutput: `{"content":[{"type":"text","text":"found"}],"structuredContent":null,"_meta":null}`,
		},
		{
			name:       "dynamic call",
			started:    `{"type":"dynamicToolCall","id":"dynamic-1","namespace":"image_gen","tool":"imagegen","arguments":{"prompt":"owl"},"status":"inProgress"}`,
			completed:  `{"type":"dynamicToolCall","id":"dynamic-1","namespace":"image_gen","tool":"imagegen","arguments":{"prompt":"owl"},"contentItems":[{"type":"inputText","text":"done"}],"success":true,"status":"completed"}`,
			toolName:   "image_gen.imagegen",
			wantOutput: `[{"type":"inputText","text":"done"}]`,
		},
		{
			name:       "collaboration call",
			started:    `{"type":"collabAgentToolCall","id":"collab-1","tool":"spawnAgent","senderThreadId":"thread-1","receiverThreadIds":["thread-2"],"prompt":"inspect","status":"inProgress"}`,
			completed:  `{"type":"collabAgentToolCall","id":"collab-1","tool":"spawnAgent","senderThreadId":"thread-1","receiverThreadIds":["thread-2"],"agentsStates":{"thread-2":{"status":"completed","message":"done"}},"status":"completed"}`,
			toolName:   "spawnAgent",
			wantOutput: `{"agentsStates":{"thread-2":{"status":"completed","message":"done"}},"receiverThreadIds":["thread-2"]}`,
		},
		{
			name:       "web search",
			started:    `{"type":"webSearch","id":"search-1","query":"codex protocol","action":{"type":"search","query":"codex protocol"},"results":null}`,
			completed:  `{"type":"webSearch","id":"search-1","query":"codex protocol","action":{"type":"search","query":"codex protocol"},"results":[{"url":"https://example.com"}]}`,
			toolName:   "web_search",
			wantOutput: `[{"url":"https://example.com"}]`,
		},
		{
			name:       "failed mcp call",
			started:    `{"type":"mcpToolCall","id":"mcp-2","tool":"broken","arguments":{},"status":"inProgress"}`,
			completed:  `{"type":"mcpToolCall","id":"mcp-2","tool":"broken","arguments":{},"error":{"message":"boom"},"status":"failed"}`,
			toolName:   "broken",
			wantOutput: `{"message":"boom"}`,
			wantError:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var events []msg.Event
			translator := NewTranslator("br_test", "req_test", func(event msg.Event) {
				events = append(events, event)
			})
			server := NewAppServer("", 0, "")
			translator.RegisterHandlers(server)

			server.notifHandlers["item/started"]("", json.RawMessage(`{"threadId":"thread-1","item":`+test.started+`}`))
			server.notifHandlers["item/completed"]("", json.RawMessage(`{"threadId":"thread-1","item":`+test.completed+`}`))

			var call *msg.ToolCallEvent
			var result *msg.ToolResultEvent
			for i := range events {
				if events[i].ToolCall != nil {
					call = events[i].ToolCall
				}
				if events[i].ToolResult != nil {
					result = events[i].ToolResult
				}
			}
			if call == nil || call.Name != test.toolName || call.MessageID != call.ToolID {
				t.Fatalf("tool call = %#v", call)
			}
			if !json.Valid(call.Input) {
				t.Fatalf("tool call input is invalid JSON: %s", call.Input)
			}
			if result == nil || result.Name != test.toolName || result.MessageID != result.ToolID {
				t.Fatalf("tool result = %#v", result)
			}
			if result.Output != test.wantOutput || result.IsError != test.wantError {
				t.Fatalf("tool result output/error = %q/%v, want %q/%v", result.Output, result.IsError, test.wantOutput, test.wantError)
			}
		})
	}
}

func TestCommandOutputDeltaIsToolDataNotAssistantText(t *testing.T) {
	var events []msg.Event
	translator := NewTranslator("br_test", "req_test", func(event msg.Event) {
		events = append(events, event)
	})
	server := NewAppServer("", 0, "")
	translator.RegisterHandlers(server)

	server.notifHandlers["item/started"]("", json.RawMessage(`{
		"threadId":"thread-1","item":{"type":"commandExecution","id":"exec-1","command":"printenv"}
	}`))
	server.notifHandlers["item/commandExecution/outputDelta"]("", json.RawMessage(`{
		"threadId":"thread-1","itemId":"exec-1","delta":"large command output"
	}`))
	server.notifHandlers["item/completed"]("", json.RawMessage(`{
		"threadId":"thread-1","item":{"type":"commandExecution","id":"exec-1","command":"printenv","exitCode":0,"status":"completed"}
	}`))

	var result *msg.ToolResultEvent
	for i := range events {
		if events[i].Type == msg.EventStream {
			t.Fatalf("command output was emitted as assistant stream: %#v", events[i].Stream)
		}
		if events[i].ToolResult != nil {
			result = events[i].ToolResult
		}
	}
	if result == nil || result.Output != "large command output" {
		t.Fatalf("tool result = %#v", result)
	}
}

func TestAgentMessageDeltaCarriesCodexItemID(t *testing.T) {
	var stream *msg.HarnessStream
	translator := NewTranslator("br_test", "req_test", func(event msg.Event) {
		if event.Stream != nil {
			stream = event.Stream
		}
	})
	server := NewAppServer("", 0, "")
	translator.RegisterHandlers(server)

	server.notifHandlers["item/agentMessage/delta"]("", json.RawMessage(`{
		"threadId":"thread-1","turnId":"turn-1","itemId":"msg-1","delta":"hello"
	}`))

	if stream == nil || stream.MessageID != "msg-1" {
		t.Fatalf("stream = %#v", stream)
	}
}
