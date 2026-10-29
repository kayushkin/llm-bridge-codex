package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// importCodexHistory preserves every rollout record in Raw. The response items
// are the source for user, assistant, and tool content. This includes user-role
// context records rather than guessing which user content to discard. Mirrored agent and
// item-completed notifications stay in the raw timeline, not duplicate bubbles.
func importCodexHistory(sessionID, path string, output io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	enc := json.NewEncoder(output)
	turnID := ""
	toolNames := map[string]string{}
	for lineNumber := 1; ; lineNumber++ {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		if len(line) == 0 && readErr == io.EOF {
			return nil
		}
		var row struct {
			Timestamp time.Time       `json:"timestamp"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			// A live writer may not have finished its final JSON record yet.
			// It is retried from disk on the next revision, never checkpointed.
			if readErr == io.EOF && err.Error() == "unexpected end of JSON input" {
				return nil
			}
			return fmt.Errorf("%s:%d: %w", path, lineNumber, err)
		}
		if row.Timestamp.IsZero() {
			return fmt.Errorf("%s:%d: missing timestamp", path, lineNumber)
		}
		var p struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Role      string          `json:"role"`
			TurnID    string          `json:"turn_id"`
			Message   string          `json:"message"`
			Name      string          `json:"name"`
			CallID    string          `json:"call_id"`
			Input     string          `json:"input"`
			Arguments string          `json:"arguments"`
			Output    json.RawMessage `json:"output"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Summary json.RawMessage `json:"summary"`
		}
		if err := json.Unmarshal(row.Payload, &p); err != nil {
			return fmt.Errorf("%s:%d payload: %w", path, lineNumber, err)
		}
		if row.Type == "session_meta" && p.ID != sessionID {
			return fmt.Errorf("%s: session_meta id %q differs from requested %q", path, p.ID, sessionID)
		}
		if (row.Type == "event_msg" && p.Type == "task_started") || row.Type == "turn_context" {
			turnID = p.TurnID
		}
		ev := msg.Event{Type: msg.EventSystem, Harness: harness, HarnessSessionID: sessionID, Timestamp: row.Timestamp, TurnID: turnID, Raw: json.RawMessage(line), System: &msg.SystemEvent{Subtype: "codex_rollout_" + row.Type}}
		// Only the bridge may mint canonical message IDs. Native IDs stay native.
		if row.Type == "response_item" {
			ev.HarnessMessageID = p.ID
		}
		switch {
		case row.Type == "response_item" && p.Type == "message" && p.Role == "user":
			text := ""
			for _, block := range p.Content {
				if block.Type == "input_text" {
					text += block.Text
				}
			}
			ev.Type = msg.EventUserMessage
			ev.System = nil
			ev.Result = &msg.ResultEvent{Text: text}
		case row.Type == "event_msg" && p.Type == "task_complete":
			ev.Type = msg.EventResult
			ev.System = nil
			ev.Result = &msg.ResultEvent{}
		case row.Type == "response_item" && p.Type == "message" && p.Role == "assistant":
			text := ""
			for _, b := range p.Content {
				if b.Type == "output_text" {
					text += b.Text
				}
			}
			if text != "" {
				ev.Type = msg.EventBlock
				ev.System = nil
				ev.Block = &msg.BlockEvent{MessageID: p.ID, Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: text}}}
			}
		case row.Type == "response_item" && p.Type == "reasoning":
			text := ""
			var summary []struct {
				Text string `json:"text"`
			}
			if len(p.Summary) > 0 {
				if err := json.Unmarshal(p.Summary, &summary); err != nil {
					return fmt.Errorf("%s:%d reasoning summary: %w", path, lineNumber, err)
				}
			}
			for _, b := range summary {
				text += b.Text
			}
			if text != "" {
				ev.Type = msg.EventThinking
				ev.System = nil
				ev.Thinking = &msg.ThinkingEvent{Text: text, Subtype: "summary"}
			}
		case row.Type == "response_item" && (p.Type == "function_call" || p.Type == "custom_tool_call"):
			var input json.RawMessage
			if p.Type == "function_call" {
				input = json.RawMessage(p.Arguments)
				if !json.Valid(input) {
					return fmt.Errorf("%s:%d: invalid function arguments JSON", path, lineNumber)
				}
			} else {
				input, _ = json.Marshal(p.Input)
			}
			toolNames[p.CallID] = p.Name
			ev.Type = msg.EventToolCall
			ev.System = nil
			ev.ToolCall = &msg.ToolCallEvent{ToolID: p.CallID, Name: p.Name, Input: input, MessageID: p.ID}
		case row.Type == "response_item" && (p.Type == "function_call_output" || p.Type == "custom_tool_call_output"):
			var output string
			if err := json.Unmarshal(p.Output, &output); err != nil {
				output = string(p.Output)
			}
			ev.Type = msg.EventToolResult
			ev.System = nil
			ev.ToolResult = &msg.ToolResultEvent{ToolID: p.CallID, Name: toolNames[p.CallID], Output: output, MessageID: p.ID}
		}
		if err := enc.Encode(ev); err != nil {
			return err
		}
		if readErr == io.EOF {
			return nil
		}
	}
}

func runHistoryImport(args []string, output io.Writer) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("usage: -import-history SESSION_ID [ROLLOUT_PATH]")
	}
	path, err := historyRolloutPath(args)
	if err != nil {
		return err
	}
	if path == "" {
		fmt.Fprintf(os.Stderr, "import-history: session not found: %s (no-op)\n", args[0])
		return nil
	}
	return importCodexHistory(args[0], path, output)
}

func historyRolloutPath(args []string) (string, error) {
	sessionID := args[0]
	path := ""
	if len(args) == 2 {
		path = args[1]
	} else {
		st, err := OpenState(DefaultStatePath())
		if err != nil {
			return "", err
		}
		defer st.Close()
		all, err := st.AllSessions()
		if err != nil {
			return "", err
		}
		for _, sess := range all {
			rollouts, err := st.ListRollouts(sess.BridgeSessionID)
			if err != nil {
				return "", err
			}
			for _, r := range rollouts {
				if r.HarnessSessionID == sessionID {
					path = r.RolloutPath
				}
			}
		}
	}
	return path, nil
}

// The stat token is only a cheap change detector. Log-store's actual records
// remain the authority for what has already been imported.
func runHistoryRevision(args []string, output io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: -history-revision SESSION_ID")
	}
	path, err := historyRolloutPath(args)
	if err != nil {
		return err
	}
	if path == "" {
		return fmt.Errorf("no rollout for session %s", args[0])
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(struct {
		Path     string `json:"path"`
		Size     int64  `json:"size"`
		Modified int64  `json:"modified_ns"`
	}{path, info.Size(), info.ModTime().UnixNano()})
}
