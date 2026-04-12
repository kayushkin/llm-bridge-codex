package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// Codex wraps the WSClient with typed methods for the app-server JSON-RPC API.
// Only the subset needed for the bridge is included.
type Codex struct {
	client func() *WSClient
}

func NewCodex(clientFn func() *WSClient) *Codex {
	return &Codex{client: clientFn}
}

func (c *Codex) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ws := c.client()
	if ws == nil {
		return nil, fmt.Errorf("codex: not connected")
	}
	if params == nil {
		params = struct{}{}
	}
	return ws.Call(ctx, method, params)
}

func callTyped[T any](c *Codex, ctx context.Context, method string, params any) (*T, error) {
	raw, err := c.call(ctx, method, params)
	if err != nil {
		return nil, err
	}
	var result T
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("codex %s: unmarshal result: %w", method, err)
	}
	return &result, nil
}

// ---------------------------------------------------------------------------
// Thread lifecycle
// ---------------------------------------------------------------------------

type ThreadStartParams struct {
	Model          string `json:"model,omitempty"`
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
	SandboxPolicy  string `json:"sandbox,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
}

type ThreadStartResult struct {
	ThreadID string `json:"threadId,omitempty"`
	Thread   struct {
		ID string `json:"id"`
	} `json:"thread,omitempty"`
}

func (r *ThreadStartResult) GetThreadID() string {
	if r.ThreadID != "" {
		return r.ThreadID
	}
	return r.Thread.ID
}

func (c *Codex) ThreadStart(ctx context.Context, params *ThreadStartParams) (*ThreadStartResult, error) {
	return callTyped[ThreadStartResult](c, ctx, "thread/start", params)
}

type ThreadResumeParams struct {
	ThreadID   string `json:"threadId,omitempty"`
	ThreadPath string `json:"threadPath,omitempty"`
}

type ThreadResumeResult struct {
	ThreadID string `json:"threadId,omitempty"`
	Thread   struct {
		ID string `json:"id"`
	} `json:"thread,omitempty"`
}

func (r *ThreadResumeResult) GetThreadID() string {
	if r.ThreadID != "" {
		return r.ThreadID
	}
	return r.Thread.ID
}

func (c *Codex) ThreadResume(ctx context.Context, params *ThreadResumeParams) (*ThreadResumeResult, error) {
	return callTyped[ThreadResumeResult](c, ctx, "thread/resume", params)
}

type ThreadForkParams struct {
	ThreadID string `json:"threadId"`
}

type ThreadForkResult struct {
	ThreadID string `json:"threadId,omitempty"`
	Thread   struct {
		ID string `json:"id"`
	} `json:"thread,omitempty"`
}

func (r *ThreadForkResult) GetThreadID() string {
	if r.ThreadID != "" {
		return r.ThreadID
	}
	return r.Thread.ID
}

func (c *Codex) ThreadFork(ctx context.Context, params *ThreadForkParams) (*ThreadForkResult, error) {
	return callTyped[ThreadForkResult](c, ctx, "thread/fork", params)
}

type ThreadCompactParams struct {
	ThreadID string `json:"threadId"`
}

func (c *Codex) ThreadCompactStart(ctx context.Context, params *ThreadCompactParams) error {
	_, err := c.call(ctx, "thread/compact/start", params)
	return err
}

// ---------------------------------------------------------------------------
// Turn control
// ---------------------------------------------------------------------------

type TurnStartParams struct {
	ThreadID       string `json:"threadId"`
	Input          any    `json:"input"`
	Model          string `json:"model,omitempty"`
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
	Sandbox        string `json:"sandbox,omitempty"`
	Effort         string `json:"effort,omitempty"`
}

type TurnInputItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func TextInput(text string) []TurnInputItem {
	return []TurnInputItem{{Type: "text", Text: text}}
}

func (c *Codex) TurnStart(ctx context.Context, params *TurnStartParams) error {
	_, err := c.call(ctx, "turn/start", params)
	return err
}

type TurnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId,omitempty"`
}

func (c *Codex) TurnInterrupt(ctx context.Context, params *TurnInterruptParams) error {
	_, err := c.call(ctx, "turn/interrupt", params)
	return err
}

// ---------------------------------------------------------------------------
// Account / auth
// ---------------------------------------------------------------------------

type AccountReadParams struct {
	RefreshToken bool `json:"refreshToken,omitempty"`
}

type AccountInfo struct {
	Email    string `json:"email,omitempty"`
	Plan     string `json:"plan,omitempty"`
	AuthMode string `json:"authMode,omitempty"`
}

func (c *Codex) AccountRead(ctx context.Context, params *AccountReadParams) (*AccountInfo, error) {
	return callTyped[AccountInfo](c, ctx, "account/read", params)
}

type AccountLoginTokensParams struct {
	Type             string `json:"type"`
	AccessToken      string `json:"accessToken"`
	ChatGPTAccountID string `json:"chatgptAccountId,omitempty"`
	ChatGPTPlanType  string `json:"chatgptPlanType,omitempty"`
}

func (c *Codex) AccountLoginStart(ctx context.Context, params any) (json.RawMessage, error) {
	return c.call(ctx, "account/login/start", params)
}

// ---------------------------------------------------------------------------
// Notification types — received from the app-server.
// ---------------------------------------------------------------------------

type TurnInfo struct {
	ID          string     `json:"id"`
	Status      string     `json:"status"` // "inProgress", "completed", "failed"
	DurationMs  int        `json:"durationMs,omitempty"`
	Error       *TurnError `json:"error,omitempty"`
	StartedAt   int64      `json:"startedAt,omitempty"`
	CompletedAt int64      `json:"completedAt,omitempty"`
}

type TurnError struct {
	Message string `json:"message"`
}

type TurnStartedNotification struct {
	ThreadID string   `json:"threadId"`
	Turn     TurnInfo `json:"turn"`
}

type TurnUsage struct {
	InputTokens       int `json:"inputTokens,omitempty"`
	CachedInputTokens int `json:"cachedInputTokens,omitempty"`
	OutputTokens      int `json:"outputTokens,omitempty"`
}

type TurnCompletedNotification struct {
	ThreadID string    `json:"threadId"`
	Turn     TurnInfo  `json:"turn"`
	Usage    TurnUsage `json:"usage"`
	Model    string    `json:"model,omitempty"`
}

type TurnFailedNotification struct {
	ThreadID string   `json:"threadId"`
	Turn     TurnInfo `json:"turn"`
}

type ThreadStartedNotification struct {
	Thread json.RawMessage `json:"thread,omitempty"` // full thread object
}

type ThreadStatusChangedNotification struct {
	ThreadID string `json:"threadId"`
	Status   struct {
		Type string `json:"type"`
	} `json:"status"`
}

// TokenUsageNotification is sent via thread/tokenUsage/updated with full token counts.
type TokenUsageNotification struct {
	ThreadID   string `json:"threadId"`
	TurnID     string `json:"turnId"`
	TokenUsage struct {
		Total struct {
			TotalTokens           int `json:"totalTokens"`
			InputTokens           int `json:"inputTokens"`
			CachedInputTokens     int `json:"cachedInputTokens"`
			OutputTokens          int `json:"outputTokens"`
			ReasoningOutputTokens int `json:"reasoningOutputTokens"`
		} `json:"total"`
		Last struct {
			TotalTokens           int `json:"totalTokens"`
			InputTokens           int `json:"inputTokens"`
			CachedInputTokens     int `json:"cachedInputTokens"`
			OutputTokens          int `json:"outputTokens"`
			ReasoningOutputTokens int `json:"reasoningOutputTokens"`
		} `json:"last"`
		ModelContextWindow int `json:"modelContextWindow"`
	} `json:"tokenUsage"`
}

// ItemNotification is the generic item lifecycle event (item/started, item/completed).
type ItemNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Item     struct {
		Type           string          `json:"type"` // "userMessage", "reasoning", "agentMessage", "commandExecution", "fileChange", etc.
		ID             string          `json:"id"`
		Text           string          `json:"text,omitempty"`
		Phase          string          `json:"phase,omitempty"` // "final_answer", etc.
		Content        json.RawMessage `json:"content,omitempty"`
		MemoryCitation json.RawMessage `json:"memoryCitation,omitempty"`
	} `json:"item"`
}

type AgentMessageDeltaNotification struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

type AgentMessageCompletedNotification struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Text     string `json:"text"`
}

type ReasoningTextDeltaNotification struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

type ReasoningSummaryTextDeltaNotification struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

type CommandExecutionStartedNotification struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Command  string `json:"command"`
}

type CommandExecutionOutputDeltaNotification struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

type CommandExecutionCompletedNotification struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	ExitCode int    `json:"exitCode"`
	Output   string `json:"output,omitempty"`
}

type FileChangeStartedNotification struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Path     string `json:"path"`
}

type FileChangeOutputDeltaNotification struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

type FileChangeCompletedNotification struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Path     string `json:"path"`
}

type MCPToolCallStartedNotification struct {
	ThreadID  string `json:"threadId"`
	ItemID    string `json:"itemId"`
	Server    string `json:"server,omitempty"`
	ToolName  string `json:"toolName"`
	Arguments string `json:"arguments,omitempty"`
}

type MCPToolCallCompletedNotification struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Output   string `json:"output,omitempty"`
}

type CollabToolCallStartedNotification struct {
	ThreadID  string `json:"threadId"`
	ItemID    string `json:"itemId"`
	ToolName  string `json:"toolName"`
	Arguments string `json:"arguments,omitempty"`
}

type CollabToolCallCompletedNotification struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Output   string `json:"output,omitempty"`
}

type WebSearchStartedNotification struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Query    string `json:"query,omitempty"`
}

type WebSearchCompletedNotification struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
}

type PlanDeltaNotification struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

type ItemErrorNotification struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Message  string `json:"message"`
}

// --- Approval request types ---

type CommandApprovalRequest struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Command  string `json:"command"`
}

type FileChangeApprovalRequest struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Path     string `json:"path"`
	Patch    string `json:"patch,omitempty"`
}

type PermissionsApprovalRequest struct {
	ThreadID    string   `json:"threadId"`
	ItemID      string   `json:"itemId"`
	Permissions []string `json:"permissions"`
}

type ApprovalResponse struct {
	Approved bool   `json:"approved"`
	Reason   string `json:"reason,omitempty"`
}
