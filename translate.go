package main

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Translator converts Codex app-server notifications into canonical msg.Event
// and emits them via the provided emit function.
type Translator struct {
	mu        sync.Mutex
	sessionID string // harness session ID (thread ID once known, bridge ID before)
	bridgeID  string // server-assigned bridge_id (stable PK)
	clientID  string // frontend correlation key (passed through unchanged)
	emit      func(msg.Event)

	// bridgeServerURL is bridge-server's base URL (no trailing slash).
	// Used by approval-request handlers to proxy gating decisions through
	// /permission/codex-prehook/{bridge_id} since codex's hook firing
	// is broken upstream. Set via SetBridgeServerURL after construction.
	bridgeServerURL string

	// Per-turn accumulators.
	text           map[string]*strings.Builder // threadID → accumulated text
	toolCalls      map[string]int              // threadID → tool count
	usage          map[string]*msg.TokenUsage  // threadID → latest usage
	model          string                      // current model (from thread info or turn completion)
	finalAnswerIDs map[string]struct{}         // item IDs that are "final_answer" phase
}

// SetBridgeServerURL configures where approval-request handlers POST
// their gating decisions. Empty / unset disables the prehook proxy and
// falls back to auto-approve (the legacy behavior).
func (t *Translator) SetBridgeServerURL(url string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.bridgeServerURL = url
}

func NewTranslator(sessionID, clientID string, emit func(msg.Event)) *Translator {
	return &Translator{
		sessionID:      sessionID,
		bridgeID:       sessionID,
		clientID:       clientID,
		emit:           emit,
		text:           make(map[string]*strings.Builder),
		toolCalls:      make(map[string]int),
		usage:          make(map[string]*msg.TokenUsage),
		finalAnswerIDs: make(map[string]struct{}),
	}
}

// SetSessionID updates the session ID emitted on events. Called once the
// Codex thread ID is known — this becomes the harness_id on the server side.
func (t *Translator) SetSessionID(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sessionID = id
}

func (t *Translator) event(typ msg.EventType) msg.Event {
	return msg.Event{
		Type:             typ,
		Harness:          msg.HarnessCodex,
		BridgeSessionID:  t.bridgeID,
		HarnessSessionID: t.sessionID,
		ClientRequestID:  t.clientID,
		Timestamp:        time.Now(),
	}
}

func (t *Translator) resetTurn(threadID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.text, threadID)
	delete(t.toolCalls, threadID)
	delete(t.usage, threadID)
	t.finalAnswerIDs = make(map[string]struct{})
}

func (t *Translator) setUsage(threadID string, usage *msg.TokenUsage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.usage[threadID] = usage
}

func (t *Translator) getUsage(threadID string) msg.TokenUsage {
	t.mu.Lock()
	defer t.mu.Unlock()
	if u := t.usage[threadID]; u != nil {
		return *u
	}
	return msg.TokenUsage{}
}

func (t *Translator) accumulatedText(threadID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if b := t.text[threadID]; b != nil {
		return b.String()
	}
	return ""
}

func (t *Translator) toolCallCount(threadID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.toolCalls[threadID]
}

// RegisterHandlers wires up all Codex notification→event translations.
func (t *Translator) RegisterHandlers(srv *AppServer) {
	// --- Thread lifecycle ---
	srv.OnNotification("thread/started", func(_ string, params json.RawMessage) {
		var n ThreadStartedNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		// Extract model provider from thread info.
		var threadInfo struct {
			ModelProvider string `json:"modelProvider"`
			CliVersion    string `json:"cliVersion"`
		}
		if err := json.Unmarshal(n.Thread, &threadInfo); err == nil && threadInfo.ModelProvider != "" {
			t.model = threadInfo.ModelProvider
		}
		// SessionState is derived centrally by llm-bridge-server; no
		// EventSessionState emission here.
	})

	srv.OnNotification("thread/status/changed", func(_ string, params json.RawMessage) {
		var n ThreadStatusChangedNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		e := t.event(msg.EventSystem)
		e.System = &msg.SystemEvent{Subtype: "thread_status", Message: n.Status.Type}
		e.Raw = params
		t.emit(e)
	})

	// --- Turn lifecycle ---
	srv.OnNotification("turn/started", func(_ string, params json.RawMessage) {
		var n TurnStartedNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		_ = n
		// Turn-start state is derived centrally by llm-bridge-server
		// from EventUserMessage; no EventSessionState emission here.
	})

	// Token usage is reported separately before turn/completed.
	srv.OnNotification("thread/tokenUsage/updated", func(_ string, params json.RawMessage) {
		var n TokenUsageNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		usage := &msg.TokenUsage{
			InputTokens:     n.TokenUsage.Last.InputTokens,
			OutputTokens:    n.TokenUsage.Last.OutputTokens,
			TotalTokens:     n.TokenUsage.Last.TotalTokens,
			CacheReadTokens:  n.TokenUsage.Last.CachedInputTokens,
			ReasoningTokens:  n.TokenUsage.Last.ReasoningOutputTokens,
			ContextTokens:    n.TokenUsage.Total.TotalTokens,
			ContextLimit:     n.TokenUsage.ModelContextWindow,
		}
		t.setUsage(n.ThreadID, usage)
	})

	srv.OnNotification("turn/completed", func(_ string, params json.RawMessage) {
		var n TurnCompletedNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		// Update model from turn completion if provided.
		if n.Model != "" {
			t.model = n.Model
		}
		finalText := t.accumulatedText(n.ThreadID)
		usage := t.getUsage(n.ThreadID)
		toolCount := t.toolCallCount(n.ThreadID)

		// Emit result event with usage from thread/tokenUsage/updated.
		e := t.event(msg.EventResult)
		e.Result = &msg.ResultEvent{
			Text:       finalText,
			NumTurns:   1,
			APICalls:   toolCount + 1,
			Model:      t.model,
			DurationMS: int64(n.Turn.DurationMs),
			Usage:      usage,
		}
		e.Raw = params
		t.emit(e)

		// Turn-end state derived centrally from EventResult.
		t.resetTurn(n.ThreadID)
	})

	srv.OnNotification("turn/failed", func(_ string, params json.RawMessage) {
		var n TurnFailedNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		errMsg := "turn failed"
		if n.Turn.Error != nil {
			errMsg = n.Turn.Error.Message
		}
		e := t.event(msg.EventError)
		e.Error = &msg.ErrorEvent{Code: "TURN_FAILED", Message: errMsg}
		e.Raw = params
		t.emit(e)

		// SessionError derived centrally from EventError.
		t.resetTurn(n.ThreadID)
	})

	// --- Generic item lifecycle (provides metadata for all item types) ---
	srv.OnNotification("item/started", func(_ string, params json.RawMessage) {
		var n ItemNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}

		// Track final_answer phase messages so we only accumulate their text.
		if n.Item.Type == "agentMessage" && n.Item.Phase == "final_answer" {
			t.mu.Lock()
			t.finalAnswerIDs[n.Item.ID] = struct{}{}
			t.mu.Unlock()
		}

		// Emit system event for item start (useful for debugging/observability).
		e := t.event(msg.EventSystem)
		e.System = &msg.SystemEvent{
			Subtype: "item_started",
			Message: n.Item.Type,
		}
		e.Raw = params
		t.emit(e)
	})

	srv.OnNotification("item/completed", func(_ string, params json.RawMessage) {
		var n ItemNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		// For agentMessage items, emit the full completed text as a system event.
		// This provides the complete message without cut-up streaming.
		if n.Item.Type == "agentMessage" && n.Item.Text != "" {
			e := t.event(msg.EventSystem)
			e.System = &msg.SystemEvent{
				Subtype: "agent_message_complete",
				Message: n.Item.Text,
			}
			e.Raw = params
			t.emit(e)
		}
	})

	// --- Agent message (text streaming) ---
	srv.OnNotification("item/agentMessage/delta", func(_ string, params json.RawMessage) {
		var n AgentMessageDeltaNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}

		// Only accumulate text from final_answer phase messages for the result.
		t.mu.Lock()
		_, isFinalAnswer := t.finalAnswerIDs[n.ItemID]
		if isFinalAnswer {
			if t.text[n.ThreadID] == nil {
				t.text[n.ThreadID] = &strings.Builder{}
			}
			t.text[n.ThreadID].WriteString(n.Delta)
		}
		t.mu.Unlock()

		e := t.event(msg.EventStream)
		e.Stream = &msg.HarnessStream{
			Delta: &msg.BlockDelta{
				Index: 0,
				Type:  msg.DeltaText,
				Text:  n.Delta,
			},
		}
		e.Raw = params
		t.emit(e)
	})

	srv.OnNotification("item/agentMessage/completed", func(_ string, params json.RawMessage) {
		// Full text already accumulated from deltas — no event needed.
	})

	// --- Reasoning / thinking ---
	srv.OnNotification("item/reasoning/textDelta", func(_ string, params json.RawMessage) {
		var n ReasoningTextDeltaNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		e := t.event(msg.EventThinking)
		e.Thinking = &msg.ThinkingEvent{Text: n.Delta}
		e.Raw = params
		t.emit(e)
	})

	srv.OnNotification("item/reasoning/summaryTextDelta", func(_ string, params json.RawMessage) {
		var n ReasoningSummaryTextDeltaNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		e := t.event(msg.EventThinking)
		e.Thinking = &msg.ThinkingEvent{Text: n.Delta, Subtype: "summary"}
		e.Raw = params
		t.emit(e)
	})

	// --- Command execution ---
	srv.OnNotification("item/commandExecution/started", func(_ string, params json.RawMessage) {
		var n CommandExecutionStartedNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		t.mu.Lock()
		t.toolCalls[n.ThreadID]++
		t.mu.Unlock()

		input, _ := json.Marshal(map[string]string{"command": n.Command})
		e := t.event(msg.EventToolCall)
		e.ToolCall = &msg.ToolCallEvent{
			ToolID: n.ItemID,
			Name:   "command_execution",
			Input:  input,
		}
		e.Raw = params
		t.emit(e)
	})

	srv.OnNotification("item/commandExecution/outputDelta", func(_ string, params json.RawMessage) {
		var n CommandExecutionOutputDeltaNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		e := t.event(msg.EventStream)
		e.Stream = &msg.HarnessStream{
			Delta: &msg.BlockDelta{
				Index: 0,
				Type:  msg.DeltaText,
				Text:  n.Delta,
			},
			MessageID: n.ItemID,
			Hidden:    true,
		}
		e.Raw = params
		t.emit(e)
	})

	srv.OnNotification("item/commandExecution/completed", func(_ string, params json.RawMessage) {
		var n CommandExecutionCompletedNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		e := t.event(msg.EventToolResult)
		e.ToolResult = &msg.ToolResultEvent{
			ToolID:  n.ItemID,
			Name:    "command_execution",
			Output:  n.Output,
			IsError: n.ExitCode != 0,
		}
		e.Raw = params
		t.emit(e)
	})

	// --- File changes ---
	srv.OnNotification("item/fileChange/started", func(_ string, params json.RawMessage) {
		var n FileChangeStartedNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		t.mu.Lock()
		t.toolCalls[n.ThreadID]++
		t.mu.Unlock()

		input, _ := json.Marshal(map[string]string{"path": n.Path})
		e := t.event(msg.EventToolCall)
		e.ToolCall = &msg.ToolCallEvent{
			ToolID: n.ItemID,
			Name:   "file_change",
			Input:  input,
		}
		e.Raw = params
		t.emit(e)
	})

	srv.OnNotification("item/fileChange/outputDelta", func(_ string, params json.RawMessage) {
		var n FileChangeOutputDeltaNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		e := t.event(msg.EventStream)
		e.Stream = &msg.HarnessStream{
			Delta: &msg.BlockDelta{
				Index: 0,
				Type:  msg.DeltaText,
				Text:  n.Delta,
			},
			MessageID: n.ItemID,
			Hidden:    true,
		}
		e.Raw = params
		t.emit(e)
	})

	srv.OnNotification("item/fileChange/completed", func(_ string, params json.RawMessage) {
		var n FileChangeCompletedNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		e := t.event(msg.EventToolResult)
		e.ToolResult = &msg.ToolResultEvent{
			ToolID: n.ItemID,
			Name:   "file_change",
			Output: n.Path,
		}
		e.Raw = params
		t.emit(e)
	})

	// --- MCP tool calls ---
	srv.OnNotification("item/mcpToolCall/started", func(_ string, params json.RawMessage) {
		var n MCPToolCallStartedNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		t.mu.Lock()
		t.toolCalls[n.ThreadID]++
		t.mu.Unlock()

		e := t.event(msg.EventToolCall)
		e.ToolCall = &msg.ToolCallEvent{
			ToolID: n.ItemID,
			Name:   n.ToolName,
			Input:  json.RawMessage(n.Arguments),
		}
		e.Raw = params
		t.emit(e)
	})

	srv.OnNotification("item/mcpToolCall/completed", func(_ string, params json.RawMessage) {
		var n MCPToolCallCompletedNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		e := t.event(msg.EventToolResult)
		e.ToolResult = &msg.ToolResultEvent{
			ToolID: n.ItemID,
			Name:   "mcp_tool_call",
			Output: n.Output,
		}
		e.Raw = params
		t.emit(e)
	})

	// --- Collab tool calls ---
	srv.OnNotification("item/collabToolCall/started", func(_ string, params json.RawMessage) {
		var n CollabToolCallStartedNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		t.mu.Lock()
		t.toolCalls[n.ThreadID]++
		t.mu.Unlock()

		e := t.event(msg.EventToolCall)
		e.ToolCall = &msg.ToolCallEvent{
			ToolID: n.ItemID,
			Name:   n.ToolName,
			Input:  json.RawMessage(n.Arguments),
		}
		e.Raw = params
		t.emit(e)
	})

	srv.OnNotification("item/collabToolCall/completed", func(_ string, params json.RawMessage) {
		var n CollabToolCallCompletedNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		e := t.event(msg.EventToolResult)
		e.ToolResult = &msg.ToolResultEvent{
			ToolID: n.ItemID,
			Name:   "collab_tool_call",
			Output: n.Output,
		}
		e.Raw = params
		t.emit(e)
	})

	// --- Web search ---
	srv.OnNotification("item/webSearch/started", func(_ string, params json.RawMessage) {
		var n WebSearchStartedNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		t.mu.Lock()
		t.toolCalls[n.ThreadID]++
		t.mu.Unlock()

		input, _ := json.Marshal(map[string]string{"query": n.Query})
		e := t.event(msg.EventToolCall)
		e.ToolCall = &msg.ToolCallEvent{
			ToolID: n.ItemID,
			Name:   "web_search",
			Input:  input,
		}
		e.Raw = params
		t.emit(e)
	})

	srv.OnNotification("item/webSearch/completed", func(_ string, params json.RawMessage) {
		var n WebSearchCompletedNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		e := t.event(msg.EventToolResult)
		e.ToolResult = &msg.ToolResultEvent{
			ToolID: n.ItemID,
			Name:   "web_search",
		}
		e.Raw = params
		t.emit(e)
	})

	// --- Plan ---
	srv.OnNotification("item/plan/delta", func(_ string, params json.RawMessage) {
		var n PlanDeltaNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		e := t.event(msg.EventPlan)
		e.Plan = &msg.PlanEvent{Text: n.Delta}
		e.Raw = params
		t.emit(e)
	})

	// --- Item errors ---
	srv.OnNotification("item/error", func(_ string, params json.RawMessage) {
		var n ItemErrorNotification
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		e := t.event(msg.EventError)
		e.Error = &msg.ErrorEvent{Code: "ITEM_ERROR", Message: n.Message}
		e.Raw = params
		t.emit(e)
	})

	// --- Top-level error ---
	srv.OnNotification("error", func(_ string, params json.RawMessage) {
		var n struct {
			Message string `json:"message"`
			Code    string `json:"code,omitempty"`
		}
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		code := n.Code
		if code == "" {
			code = "SERVER_ERROR"
		}
		e := t.event(msg.EventError)
		e.Error = &msg.ErrorEvent{Code: code, Message: n.Message}
		e.Raw = params
		t.emit(e)
	})

	// --- Hook lifecycle ---
	srv.OnNotification("hook/started", func(_ string, params json.RawMessage) {
		e := t.event(msg.EventSystem)
		e.System = &msg.SystemEvent{Subtype: "hook_started"}
		e.Raw = params
		t.emit(e)
	})

	srv.OnNotification("hook/completed", func(_ string, params json.RawMessage) {
		e := t.event(msg.EventSystem)
		e.System = &msg.SystemEvent{Subtype: "hook_completed"}
		e.Raw = params
		t.emit(e)
	})

	// --- Turn diff/plan updates ---
	srv.OnNotification("turn/diff/updated", func(_ string, params json.RawMessage) {
		e := t.event(msg.EventSystem)
		e.System = &msg.SystemEvent{Subtype: "turn_diff_updated"}
		e.Raw = params
		t.emit(e)
	})

	srv.OnNotification("turn/plan/updated", func(_ string, params json.RawMessage) {
		var n struct {
			Plan string `json:"plan,omitempty"`
		}
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		e := t.event(msg.EventPlan)
		e.Plan = &msg.PlanEvent{Text: n.Plan}
		e.Raw = params
		t.emit(e)
	})

	// --- Thread compaction completed ---
	srv.OnNotification("thread/compacted", func(_ string, params json.RawMessage) {
		e := t.event(msg.EventSystem)
		e.System = &msg.SystemEvent{Subtype: "thread_compacted"}
		e.Raw = params
		t.emit(e)
	})

	// --- Model rerouted ---
	srv.OnNotification("model/rerouted", func(_ string, params json.RawMessage) {
		var n struct {
			Model string `json:"model,omitempty"`
		}
		if err := json.Unmarshal(params, &n); err != nil {
			return
		}
		if n.Model != "" {
			t.model = n.Model
		}
		e := t.event(msg.EventSystem)
		e.System = &msg.SystemEvent{Subtype: "model_rerouted", Message: n.Model}
		e.Raw = params
		t.emit(e)
	})

	// --- Auto-approval review ---
	srv.OnNotification("item/autoApprovalReview/started", func(_ string, params json.RawMessage) {
		e := t.event(msg.EventSystem)
		e.System = &msg.SystemEvent{Subtype: "auto_approval_review_started"}
		e.Raw = params
		t.emit(e)
	})

	srv.OnNotification("item/autoApprovalReview/completed", func(_ string, params json.RawMessage) {
		e := t.event(msg.EventSystem)
		e.System = &msg.SystemEvent{Subtype: "auto_approval_review_completed"}
		e.Raw = params
		t.emit(e)
	})

	// --- MCP tool call progress ---
	srv.OnNotification("item/mcpToolCall/progress", func(_ string, params json.RawMessage) {
		e := t.event(msg.EventSystem)
		e.System = &msg.SystemEvent{Subtype: "mcp_tool_call_progress"}
		e.Raw = params
		t.emit(e)
	})

	// --- Reasoning summary part added ---
	srv.OnNotification("item/reasoning/summaryPartAdded", func(_ string, params json.RawMessage) {
		e := t.event(msg.EventSystem)
		e.System = &msg.SystemEvent{Subtype: "reasoning_summary_part_added"}
		e.Raw = params
		t.emit(e)
	})

	// --- Command execution terminal interaction ---
	srv.OnNotification("item/commandExecution/terminalInteraction", func(_ string, params json.RawMessage) {
		e := t.event(msg.EventSystem)
		e.System = &msg.SystemEvent{Subtype: "terminal_interaction"}
		e.Raw = params
		t.emit(e)
	})

}

// RegisterApprovalHandlers wires codex's WebSocket *ApprovalRequest events
// to the bridge-server prehook URL.
//
// Why this isn't auto-approve any more: codex 0.130's PreToolUse hook
// firing is broken upstream (issue #21639). Instead of routing gating
// through codex's hook layer, we use codex's NATIVE approval-request
// flow (sent when approval_policy = on-request) and proxy each request
// to the bridge-server prehook. The user-visible result is identical to
// what working hooks would give us: parked-ask banner in Rules mode,
// allow/deny per permission-store rules, etc.
//
// The bridge MUST be configured with approval_policy = on-request (or
// untrusted) for codex to actually send these requests — see
// applyCanonicalPermissionMode. Bypass mode keeps approval_policy = never
// and never reaches these handlers.
//
// fail-closed: if t.bridgeServerURL is empty (legacy / unconfigured), we
// fall back to auto-approve so we don't silently break sessions; loud log.
func (t *Translator) RegisterApprovalHandlers(srv *AppServer) {
	srv.OnRequest("item/commandExecution/requestApproval", func(_ string, params json.RawMessage) (json.RawMessage, error) {
		var req CommandApprovalRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, err
		}
		approved, reason := t.gateApproval("unified_exec", map[string]any{
			"command": req.Command,
		}, req.ItemID, params)
		return json.Marshal(ApprovalResponse{Approved: approved, Reason: reason})
	})

	srv.OnRequest("item/fileChange/requestApproval", func(_ string, params json.RawMessage) (json.RawMessage, error) {
		var req FileChangeApprovalRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, err
		}
		approved, reason := t.gateApproval("apply_patch", map[string]any{
			"path":  req.Path,
			"patch": req.Patch,
		}, req.ItemID, params)
		return json.Marshal(ApprovalResponse{Approved: approved, Reason: reason})
	})

	srv.OnRequest("item/permissions/requestApproval", func(_ string, params json.RawMessage) (json.RawMessage, error) {
		var req PermissionsApprovalRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, err
		}
		approved, reason := t.gateApproval("request_permissions", map[string]any{
			"permissions": req.Permissions,
		}, req.ItemID, params)
		return json.Marshal(ApprovalResponse{Approved: approved, Reason: reason})
	})

	srv.OnRequest("applyPatchApproval", func(_ string, params json.RawMessage) (json.RawMessage, error) {
		approved, reason := t.gateApproval("apply_patch", json.RawMessage(params), "", params)
		return json.Marshal(ApprovalResponse{Approved: approved, Reason: reason})
	})

	srv.OnRequest("execCommandApproval", func(_ string, params json.RawMessage) (json.RawMessage, error) {
		var req struct {
			Command string `json:"command,omitempty"`
		}
		_ = json.Unmarshal(params, &req)
		approved, reason := t.gateApproval("unified_exec", map[string]any{
			"command": req.Command,
		}, "", params)
		return json.Marshal(ApprovalResponse{Approved: approved, Reason: reason})
	})

	// Headless: reject user input and dynamic tool calls.
	srv.OnRequest("item/tool/requestUserInput", func(_ string, _ json.RawMessage) (json.RawMessage, error) {
		return nil, &RPCError{Code: -32000, Message: "headless mode: user input not available"}
	})

	srv.OnRequest("item/tool/call", func(_ string, _ json.RawMessage) (json.RawMessage, error) {
		return nil, &RPCError{Code: -32000, Message: "headless mode: dynamic tool calls not supported"}
	})

	srv.OnRequest("mcpServer/elicitation/request", func(_ string, _ json.RawMessage) (json.RawMessage, error) {
		return json.Marshal(map[string]string{"action": "cancel"})
	})

	srv.OnRequest("account/chatgptAuthTokens/refresh", func(_ string, _ json.RawMessage) (json.RawMessage, error) {
		return nil, &RPCError{Code: -32000, Message: "bridge does not manage auth tokens"}
	})
}

// gateApproval is the shared body of every *ApprovalRequest handler:
// proxy the request to bridge-server's prehook URL (or fall back to
// auto-approve when no URL is configured), then emit an EventApproval
// so the canonical event stream stays unchanged regardless of which
// path was taken.
//
// toolName is the codex-side tool name — passed straight through to
// permission-store, no remapping. tool_use_id correlates the approval
// with its parked-ask banner in the UI (Item-ID from codex's payload
// is the natural fit). rawParams is the original WebSocket payload
// preserved on event.Raw.
func (t *Translator) gateApproval(toolName string, toolInput any, toolUseID string, rawParams json.RawMessage) (bool, string) {
	// Snapshot the URL + bridgeID under the lock — they can change as the
	// session evolves (SetBridgeServerURL / SetSessionID), but each
	// approval should use a consistent snapshot.
	t.mu.Lock()
	baseURL := t.bridgeServerURL
	bridgeID := t.bridgeID
	t.mu.Unlock()

	var approved bool
	var reason string
	if baseURL == "" {
		// Legacy / unconfigured: fall back to auto-approve so we don't
		// silently break sessions. Loud log so this stays visible.
		log.Printf("[approval] bridgeServerURL unset — falling back to auto-approve %s", toolName)
		approved = true
		reason = "auto-approved (no bridge URL configured)"
	} else {
		approved, reason = gateViaPrehook(context.Background(), baseURL, bridgeID, toolName, toolInput, toolUseID)
	}

	// Emit EventApproval so consumers (event log, UI, etc.) see the
	// canonical record of the decision. Status reflects what we actually
	// sent back to codex — "approved" when allowed, "denied" otherwise.
	e := t.event(msg.EventApproval)
	status := "approved"
	action := "approve"
	if !approved {
		status = "denied"
		action = "deny"
	}
	e.Approval = &msg.ApprovalEvent{
		Action:   action,
		Status:   status,
		ToolName: toolName,
		Detail:   reason,
	}
	e.Raw = rawParams
	t.emit(e)

	return approved, reason
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
