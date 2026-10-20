package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Codex writes every conversation to a rollout file,
// ~/.codex/sessions/YYYY/MM/DD/rollout-<time>-<thread id>.jsonl, one JSON entry
// per line. This file turns those entries into msg.Events. Three callers read
// rollouts this way: -import-history (a whole file, for a session the bridge
// found on disk), -pty-sidecar (a file as codex writes it, for a session the
// person is driving in codex's own screen), and through import-history the
// server's pickup of turns run in a terminal outside the bridge.
//
// Codex has written two dialects:
//   - through 2026-08, event_msg entries of type user_message, agent_message;
//   - since 2026-09, event_msg item_completed entries carrying a UserMessage or
//     AgentMessage item.
//
// Both are read. Messages come only from event_msg, never from response_item
// messages: those also carry the developer prompt and the context codex injects
// as user-role text, which the person never typed. Tool calls come from
// response_item in both dialects (function_call, custom_tool_call,
// web_search_call, and the outputs). A turn ends at task_complete or
// turn_aborted, which become the EventResult the server's state derivation
// closes a turn on.
//
// Every event is stamped with the entry's own time, the thread id and
// Extensions["source"]="rollout".

// rolloutEntry is one line of a rollout file.
type rolloutEntry struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type rolloutPayload struct {
	Type string `json:"type"`

	// session_meta
	ID string `json:"id"`

	// turn_context
	Model string `json:"model"`

	// event_msg user_message / agent_message
	Message string `json:"message"`

	// event_msg item_completed
	Item *struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"item"`

	// event_msg task_complete / turn_aborted
	LastAgentMessage string `json:"last_agent_message"`
	Reason           string `json:"reason"`
	DurationMS       int64  `json:"duration_ms"`

	// event_msg token_count
	Info *struct {
		LastTokenUsage     rolloutTokenUsage `json:"last_token_usage"`
		TotalTokenUsage    rolloutTokenUsage `json:"total_token_usage"`
		ModelContextWindow int               `json:"model_context_window"`
	} `json:"info"`

	// response_item tool calls and outputs
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Action    json.RawMessage `json:"action"`
	Status    string          `json:"status"`
}

type rolloutTokenUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	TotalTokens           int `json:"total_tokens"`
}

// rolloutReader carries what a rollout says once and later entries need: the
// thread id from session_meta, the model from turn_context, and the latest
// token usage, which codex reports before the turn's end.
type rolloutReader struct {
	threadID      string
	model         string
	usage         msg.TokenUsage
	toolCallNames map[string]string // call_id → tool name, for its output
}

func newRolloutReader(threadID string) *rolloutReader {
	return &rolloutReader{threadID: threadID, toolCallNames: map[string]string{}}
}

// translateLine turns one rollout line into zero or more events. A line that
// is not JSON returns an error; an entry of a kind this does not read returns
// no events and no error.
func (r *rolloutReader) translateLine(line []byte) ([]msg.Event, error) {
	line = []byte(strings.TrimRight(string(line), "\r\n"))
	if len(line) == 0 {
		return nil, nil
	}
	var entry rolloutEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return nil, fmt.Errorf("rollout line is not JSON: %w", err)
	}
	var p rolloutPayload
	if len(entry.Payload) > 0 && entry.Payload[0] == '{' {
		if err := json.Unmarshal(entry.Payload, &p); err != nil {
			return nil, fmt.Errorf("rollout %s payload: %w", entry.Type, err)
		}
	}
	ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("rollout %s timestamp %q: %w", entry.Type, entry.Timestamp, err)
	}

	var out []msg.Event
	switch entry.Type {
	case "session_meta":
		if r.threadID == "" {
			r.threadID = p.ID
		}
	case "turn_context":
		if p.Model != "" {
			r.model = p.Model
		}
	case "event_msg":
		out = r.translateEventMsg(p, ts)
	case "response_item":
		out = r.translateResponseItem(p, ts)
	}
	for i := range out {
		out[i].Harness = harness
		out[i].HarnessSessionID = r.threadID
		out[i].Timestamp = ts
		if out[i].Extensions == nil {
			out[i].Extensions = map[string]json.RawMessage{}
		}
		out[i].Extensions["source"] = json.RawMessage(`"rollout"`)
	}
	return out, nil
}

func (r *rolloutReader) translateEventMsg(p rolloutPayload, ts time.Time) []msg.Event {
	switch p.Type {
	case "user_message":
		return userMessageEvent(p.Message)
	case "agent_message":
		return agentTextEvent(p.Message)
	case "item_completed":
		if p.Item == nil {
			return nil
		}
		var text strings.Builder
		for _, c := range p.Item.Content {
			text.WriteString(c.Text)
		}
		switch p.Item.Type {
		case "UserMessage":
			return userMessageEvent(text.String())
		case "AgentMessage":
			return agentTextEvent(text.String())
		}
	case "token_count":
		if p.Info != nil {
			last := p.Info.LastTokenUsage
			r.usage = msg.TokenUsage{
				InputTokens:     last.InputTokens,
				OutputTokens:    last.OutputTokens,
				TotalTokens:     last.TotalTokens,
				CacheReadTokens: last.CachedInputTokens,
				ReasoningTokens: last.ReasoningOutputTokens,
				ContextTokens:   p.Info.TotalTokenUsage.TotalTokens,
				ContextLimit:    p.Info.ModelContextWindow,
			}
		}
	case "task_complete":
		e := msg.Event{Type: msg.EventResult, Result: &msg.ResultEvent{
			Text:       p.LastAgentMessage,
			NumTurns:   1,
			Model:      r.model,
			DurationMS: p.DurationMS,
			Usage:      r.usage,
		}}
		r.usage = msg.TokenUsage{}
		return []msg.Event{e}
	case "turn_aborted":
		// The person stopped the turn (esc). It still ends the turn; the
		// error says why it has no answer.
		e := msg.Event{Type: msg.EventResult, Result: &msg.ResultEvent{
			Text:       "",
			IsError:    true,
			NumTurns:   1,
			Model:      r.model,
			DurationMS: p.DurationMS,
			Usage:      r.usage,
		}}
		e.Error = &msg.ErrorEvent{Code: "TURN_ABORTED", Message: "turn aborted: " + p.Reason}
		r.usage = msg.TokenUsage{}
		return []msg.Event{e}
	}
	return nil
}

func (r *rolloutReader) translateResponseItem(p rolloutPayload, ts time.Time) []msg.Event {
	switch p.Type {
	case "function_call":
		r.toolCallNames[p.CallID] = p.Name
		input := json.RawMessage(p.Arguments)
		if !json.Valid(input) {
			input = marshalRaw(map[string]string{"arguments": p.Arguments})
		}
		return []msg.Event{toolCallEvent(p.CallID, p.Name, input)}
	case "custom_tool_call":
		r.toolCallNames[p.CallID] = p.Name
		return []msg.Event{toolCallEvent(p.CallID, p.Name, marshalRaw(map[string]string{"input": p.Input}))}
	case "web_search_call":
		input := p.Action
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		// A web search has no call id and no separate output entry.
		id := fmt.Sprintf("web_search_%d", ts.UnixNano())
		return []msg.Event{toolCallEvent(id, "web_search", input)}
	case "function_call_output", "custom_tool_call_output":
		name := r.toolCallNames[p.CallID]
		delete(r.toolCallNames, p.CallID)
		return []msg.Event{{Type: msg.EventToolResult, ToolResult: &msg.ToolResultEvent{
			ToolID: p.CallID,
			Name:   name,
			Output: rolloutOutputText(p.Output),
		}}}
	}
	return nil
}

func userMessageEvent(text string) []msg.Event {
	if text == "" {
		return nil
	}
	return []msg.Event{{Type: msg.EventUserMessage, Result: &msg.ResultEvent{Text: text}}}
}

func agentTextEvent(text string) []msg.Event {
	if text == "" {
		return nil
	}
	return []msg.Event{{Type: msg.EventBlock, Block: &msg.BlockEvent{
		Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: text}},
	}}}
}

func toolCallEvent(id, name string, input json.RawMessage) msg.Event {
	return msg.Event{Type: msg.EventToolCall, ToolCall: &msg.ToolCallEvent{ToolID: id, Name: name, Input: input}}
}

// rolloutOutputText reads a tool output, which codex writes either as a string
// or as an array of {type, text} parts.
func rolloutOutputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, part := range parts {
			b.WriteString(part.Text)
		}
		return b.String()
	}
	return string(raw)
}

// translateRolloutStream reads a whole rollout and hands each event to emit.
// A line that cannot be read stops the import with its line number: a
// transcript silently missing entries is the wrong answer.
func translateRolloutStream(threadID string, in io.Reader, emit func(msg.Event) error) error {
	reader := newRolloutReader(threadID)
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		events, err := reader.translateLine(scanner.Bytes())
		if err != nil {
			return fmt.Errorf("line %d: %w", lineNumber, err)
		}
		for _, ev := range events {
			if err := emit(ev); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

// runImportHistory is -import-history <thread id>: the thread's whole rollout
// as NDJSON events on stdout, the contract llm-bridge-server's ImportHistory
// reads.
func runImportHistory(threadID string) int {
	if threadID == "" {
		fmt.Fprintln(os.Stderr, "llm-bridge-codex -import-history: a thread id is required")
		return 2
	}
	path := findRolloutForThread(threadID)
	if path == "" {
		// The conformance contract: a session this harness does not have is
		// a no-op, as -discover finding nothing is (llm-bridge-server
		// conformance/runner.go testImport). Said on stderr, as Claude
		// Code's wrapper says it.
		fmt.Fprintf(os.Stderr, "import-history: no rollout for thread %s under ~/.codex/sessions (no-op)\n", threadID)
		return 0
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "llm-bridge-codex -import-history: %v\n", err)
		return 1
	}
	defer f.Close()
	enc := json.NewEncoder(os.Stdout)
	if err := translateRolloutStream(threadID, f, func(ev msg.Event) error { return enc.Encode(ev) }); err != nil {
		fmt.Fprintf(os.Stderr, "llm-bridge-codex -import-history %s: %v\n", path, err)
		return 1
	}
	return 0
}
