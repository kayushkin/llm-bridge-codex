package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// A turn of three model calls, as codex reported a real one (rollout
// 01a1844c, 2026-10-28): the wrapper used to send only the third call as the
// turn's usage, 27,438 input and 58 output, when the turn had used 80,991 and
// 252, and put the running total in ContextTokens.
var threeCalls = []codexCallUsage{
	{InputTokens: 26900, CachedInputTokens: 18688, OutputTokens: 120, ReasoningOutputTokens: 19},
	{InputTokens: 26653, CachedInputTokens: 18688, OutputTokens: 74},
	{InputTokens: 27438, CachedInputTokens: 26368, OutputTokens: 58},
}

func wantThreeCallTurn(t *testing.T, usage msg.TokenUsage) {
	t.Helper()
	want := msg.TokenUsage{
		InputTokens:     80991 - 63744,
		CacheReadTokens: 63744,
		OutputTokens:    252,
		ReasoningTokens: 19,
		TotalTokens:     80991 + 252,
		ContextTokens:   27438,
		ContextLimit:    258400,
	}
	if usage.InputTokens != want.InputTokens || usage.CacheReadTokens != want.CacheReadTokens ||
		usage.OutputTokens != want.OutputTokens || usage.ReasoningTokens != want.ReasoningTokens ||
		usage.TotalTokens != want.TotalTokens || usage.ContextTokens != want.ContextTokens || usage.ContextLimit != want.ContextLimit {
		t.Errorf("turn usage = %+v\nwant %+v", usage, want)
	}
}

func TestATurnsUsageFromTheAppServerIsEveryCallOfTheTurn(t *testing.T) {
	var results []*msg.ResultEvent
	translator := NewTranslator("br_test", "req_test", func(event msg.Event) {
		if event.Type == msg.EventResult {
			results = append(results, event.Result)
		}
	})
	server := NewAppServer("", 0, "")
	translator.RegisterHandlers(server)

	send := func(call codexCallUsage) {
		params, _ := json.Marshal(map[string]any{
			"threadId": "thread-1", "turnId": "turn-1",
			"tokenUsage": map[string]any{
				"last": map[string]any{"inputTokens": call.InputTokens, "cachedInputTokens": call.CachedInputTokens,
					"outputTokens": call.OutputTokens, "reasoningOutputTokens": call.ReasoningOutputTokens},
				"modelContextWindow": 258400,
			},
		})
		server.notifHandlers["thread/tokenUsage/updated"]("", params)
	}
	for _, call := range threeCalls {
		send(call)
	}
	server.notifHandlers["turn/completed"]("", json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`))
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	wantThreeCallTurn(t, results[0].Usage)

	// The next turn starts from nothing, not from this turn's sum.
	send(threeCalls[2])
	server.notifHandlers["turn/completed"]("", json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-2","status":"completed"}}`))
	if len(results) != 2 || results[1].Usage.OutputTokens != 58 {
		t.Fatalf("second turn usage = %+v, want only its own call", results[len(results)-1].Usage)
	}
}

func TestATurnsUsageFromARolloutIsEveryCallOfTheTurn(t *testing.T) {
	var rollout strings.Builder
	rollout.WriteString(`{"timestamp":"2026-10-28T21:15:53.000Z","type":"session_meta","payload":{"id":"01a1844c-0000-7000-8000-000000000000","cwd":"/tmp","timestamp":"2026-10-28T21:15:53.000Z"}}` + "\n")
	rollout.WriteString(`{"timestamp":"2026-10-28T21:15:53.100Z","type":"event_msg","payload":{"type":"user_message","message":"read the task","images":[]}}` + "\n")
	for _, call := range threeCalls {
		line, _ := json.Marshal(map[string]any{
			"timestamp": "2026-10-28T21:15:54.000Z", "type": "event_msg",
			"payload": map[string]any{"type": "token_count", "info": map[string]any{
				"last_token_usage": map[string]any{"input_tokens": call.InputTokens, "cached_input_tokens": call.CachedInputTokens,
					"output_tokens": call.OutputTokens, "reasoning_output_tokens": call.ReasoningOutputTokens},
				"total_token_usage":    map[string]any{"total_tokens": 81243},
				"model_context_window": 258400,
			}},
		})
		rollout.Write(line)
		rollout.WriteString("\n")
	}
	rollout.WriteString(`{"timestamp":"2026-10-28T21:15:57.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1","last_agent_message":"done","duration_ms":4000}}` + "\n")

	var result *msg.ResultEvent
	for _, event := range readAll(t, rollout.String()) {
		if event.Type == msg.EventResult {
			result = event.Result
		}
	}
	if result == nil {
		t.Fatal("no result event")
	}
	wantThreeCallTurn(t, result.Usage)
}
