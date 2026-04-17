package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Bridge holds the state for one session managed by this process.
type Bridge struct {
	cfg       Config
	app       *AppServer
	codex     *Codex
	trans     *Translator
	bridgeID  string // server-assigned bridge_id (stable PK)
	clientID  string // frontend correlation key
	threadID  string // Codex thread ID — becomes the harness_id
	turnDone  chan struct{}
}

func NewBridge(cfg Config, emit func(msg.Event)) *Bridge {
	app := NewAppServer(cfg.CodexPath, cfg.CodexWSPort, cfg.CodexWorkdir)
	b := &Bridge{
		cfg:      cfg,
		app:      app,
		turnDone: make(chan struct{}, 1),
	}
	return b
}

// currentSessionID returns the thread ID if known, otherwise the bridge ID.
// This is the value emitted as SessionID on events — the server uses the first
// event where SessionID != bridge_id to set harness_id.
func (b *Bridge) currentSessionID() string {
	if b.threadID != "" {
		return b.threadID
	}
	return b.bridgeID
}

// event creates a base event with the correct session IDs populated.
func (b *Bridge) event(typ msg.EventType) msg.Event {
	return msg.Event{
		Type:      typ,
		Harness:   msg.HarnessCodex,
		SessionID: b.currentSessionID(),
		BridgeID:  b.bridgeID,
		ClientID:  b.clientID,
		Timestamp: time.Now(),
	}
}

// Init starts the app-server, connects, authenticates, and registers handlers.
func (b *Bridge) Init(ctx context.Context, sessionID, clientID string, emit func(msg.Event)) error {
	b.bridgeID = sessionID
	b.clientID = clientID
	b.trans = NewTranslator(sessionID, clientID, emit)

	// Register all notification → event translations.
	b.trans.RegisterHandlers(b.app)
	b.trans.RegisterApprovalHandlers(b.app)

	// Wire turn completion to signal the handler.
	origTurnCompleted := b.app.notifHandlers["turn/completed"]
	log.Printf("[handler] origTurnCompleted registered: %v", origTurnCompleted != nil)
	b.app.OnNotification("turn/completed", func(method string, params json.RawMessage) {
		log.Printf("[handler] turn/completed wrapper called")
		if origTurnCompleted != nil {
			origTurnCompleted(method, params)
		}
		log.Printf("[handler] signaling turnDone")
		select {
		case b.turnDone <- struct{}{}:
			log.Printf("[handler] turnDone signaled")
		default:
			log.Printf("[handler] turnDone channel full")
		}
	})

	origTurnFailed := b.app.notifHandlers["turn/failed"]
	b.app.OnNotification("turn/failed", func(method string, params json.RawMessage) {
		if origTurnFailed != nil {
			origTurnFailed(method, params)
		}
		select {
		case b.turnDone <- struct{}{}:
		default:
		}
	})

	if err := b.app.Start(ctx); err != nil {
		return fmt.Errorf("start app-server: %w", err)
	}

	b.codex = NewCodex(b.app.Client())

	// Try to authenticate from stored tokens.
	if err := b.initAuth(ctx); err != nil {
		log.Printf("[bridge] auth init: %v (continuing without auth)", err)
	}

	return nil
}

func (b *Bridge) initAuth(ctx context.Context) error {
	authPath := filepath.Join(os.Getenv("HOME"), ".codex", "auth.json")
	data, err := os.ReadFile(authPath)
	if err != nil {
		return err
	}

	var authFile struct {
		AuthMode string `json:"auth_mode"`
		Tokens   struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &authFile); err != nil {
		return err
	}

	if authFile.AuthMode == "chatgpt" && authFile.Tokens.AccessToken != "" {
		_, err := b.codex.AccountLoginStart(ctx, &AccountLoginTokensParams{
			Type:             "chatgptAuthTokens",
			AccessToken:      authFile.Tokens.AccessToken,
			ChatGPTAccountID: authFile.Tokens.AccountID,
			ChatGPTPlanType:  "plus",
		})
		if err != nil {
			return err
		}

		acct, err := b.codex.AccountRead(ctx, &AccountReadParams{})
		if err != nil {
			return err
		}
		log.Printf("[bridge] authenticated: plan=%s auth=%s email=%s", acct.Plan, acct.AuthMode, acct.Email)
	}

	return nil
}

// HandleStart creates a new thread and starts the first turn.
func (b *Bridge) HandleStart(ctx context.Context, params StartParams) error {
	// Apply start-time overrides from params.
	if params.Model != "" {
		b.cfg.CodexModel = params.Model
	}
	if params.WorkDir != "" {
		b.cfg.CodexWorkdir = params.WorkDir
	}
	if params.ApprovalMode != "" {
		b.cfg.ApprovalMode = params.ApprovalMode
	}
	if params.Sandbox != "" {
		b.cfg.SandboxPolicy = params.Sandbox
	}
	if params.Effort != "" {
		b.cfg.Effort = params.Effort
	}
	if params.AutoApprove != nil && *params.AutoApprove {
		b.cfg.ApprovalMode = "never"
		b.cfg.SandboxPolicy = "workspace-write"
	}

	// If forking from a parent session, use thread/fork.
	if params.Fork != "" {
		result, err := b.codex.ThreadFork(ctx, &ThreadForkParams{ThreadID: params.Fork})
		if err != nil {
			return fmt.Errorf("thread/fork: %w", err)
		}
		b.threadID = result.GetThreadID()
		b.trans.SetSessionID(b.threadID)
		log.Printf("[bridge] forked thread %s from %s", b.threadID, params.Fork)
	} else {
		result, err := b.codex.ThreadStart(ctx, &ThreadStartParams{
			Model:                 b.cfg.CodexModel,
			ApprovalPolicy:        b.cfg.ApprovalMode,
			Sandbox:               b.cfg.SandboxPolicy,
			Cwd:                   b.cfg.CodexWorkdir,
			DeveloperInstructions: params.SystemPrompt,
		})
		if err != nil {
			return fmt.Errorf("thread/start: %w", err)
		}
		b.threadID = result.GetThreadID()
		b.trans.SetSessionID(b.threadID)
		log.Printf("[bridge] started thread %s", b.threadID)
	}

	if params.Prompt == "" {
		return nil
	}

	return b.startTurn(ctx, params.Prompt)
}

// HandleMessage sends a follow-up message to the existing thread.
func (b *Bridge) HandleMessage(ctx context.Context, content string) error {
	if b.threadID == "" {
		return fmt.Errorf("no active thread")
	}
	return b.startTurn(ctx, content)
}

// HandleResume resumes a previously interrupted thread by sending a continuation prompt.
func (b *Bridge) HandleResume(ctx context.Context) error {
	if b.threadID == "" {
		return fmt.Errorf("no active thread")
	}
	return b.startTurn(ctx, "Continue where you left off.")
}

// HandleResumeThread resumes an existing thread by ID using the ThreadResume API.
func (b *Bridge) HandleResumeThread(ctx context.Context, threadID string) error {
	result, err := b.codex.ThreadResume(ctx, &ThreadResumeParams{ThreadID: threadID})
	if err != nil {
		return fmt.Errorf("thread/resume: %w", err)
	}
	b.threadID = result.GetThreadID()
	b.trans.SetSessionID(b.threadID)
	log.Printf("[bridge] resumed thread %s", b.threadID)
	return b.startTurn(ctx, "Continue where you left off.")
}

// HandleCompact triggers context compaction on the thread.
func (b *Bridge) HandleCompact(ctx context.Context) error {
	if b.threadID == "" {
		return fmt.Errorf("no active thread")
	}
	return b.codex.ThreadCompactStart(ctx, &ThreadCompactParams{ThreadID: b.threadID})
}

// HandleInterrupt interrupts the current turn.
func (b *Bridge) HandleInterrupt(ctx context.Context) error {
	if b.threadID == "" {
		return nil
	}
	return b.codex.TurnInterrupt(ctx, &TurnInterruptParams{ThreadID: b.threadID})
}

// HandleSetModel changes the model used for subsequent turns.
func (b *Bridge) HandleSetModel(model string) {
	b.cfg.CodexModel = model
	log.Printf("[bridge] model changed to %s", model)
}

// HandleSetPermissionMode changes the approval policy for subsequent turns.
func (b *Bridge) HandleSetPermissionMode(mode string) {
	b.cfg.ApprovalMode = mode
	log.Printf("[bridge] approval mode changed to %s", mode)
}

// HandleConfig handles mid-session config updates (config:<json> method).
func (b *Bridge) HandleConfig(configJSON string) {
	var cfg struct {
		Model  string `json:"model,omitempty"`
		Effort string `json:"effort,omitempty"`
	}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		log.Printf("[bridge] parse config: %v", err)
		return
	}
	if cfg.Model != "" {
		b.HandleSetModel(cfg.Model)
	}
	if cfg.Effort != "" {
		b.cfg.Effort = cfg.Effort
		log.Printf("[bridge] effort changed to %s", cfg.Effort)
	}
}

// HandleControl dispatches generic control commands.
func (b *Bridge) HandleControl(ctx context.Context, params ControlParams) error {
	switch params.Subtype {
	case "set_model":
		if m, ok := params.Payload["model"]; ok {
			b.HandleSetModel(m)
		}
		return nil
	case "set_permission_mode":
		if m, ok := params.Payload["mode"]; ok {
			b.HandleSetPermissionMode(m)
		}
		return nil
	case "interrupt":
		return b.HandleInterrupt(ctx)
	default:
		log.Printf("[bridge] unknown control subtype: %s", params.Subtype)
		return nil
	}
}

// sandboxModeToPolicy converts user-facing sandbox mode strings to the tagged enum
// format required by turn/start. Thread/start uses the string directly (SandboxMode).
func sandboxModeToPolicy(mode string) *SandboxPolicy {
	switch mode {
	case "workspace-write":
		return &SandboxPolicy{Type: "workspaceWrite"}
	case "read-only":
		return &SandboxPolicy{Type: "readOnly"}
	case "danger-full-access":
		return &SandboxPolicy{Type: "dangerFullAccess"}
	default:
		if mode != "" {
			return &SandboxPolicy{Type: mode}
		}
		return nil
	}
}

func (b *Bridge) startTurn(ctx context.Context, prompt string) error {
	// Drain any stale turnDone signals.
	select {
	case <-b.turnDone:
	default:
	}

	if err := b.codex.TurnStart(ctx, &TurnStartParams{
		ThreadID:       b.threadID,
		Input:          TextInput(prompt),
		Model:          b.cfg.CodexModel,
		ApprovalPolicy: b.cfg.ApprovalMode,
		SandboxPolicy:  sandboxModeToPolicy(b.cfg.SandboxPolicy),
		Effort:         b.cfg.Effort,
	}); err != nil {
		return fmt.Errorf("turn/start: %w", err)
	}

	// Block until the turn completes or fails.
	select {
	case <-b.turnDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Minute):
		return fmt.Errorf("turn timed out after 30 minutes")
	}
}

// Shutdown gracefully disconnects from the app-server.
func (b *Bridge) Shutdown() {
	if b.threadID != "" && b.codex != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = b.codex.TurnInterrupt(ctx, &TurnInterruptParams{ThreadID: b.threadID})
	}
	b.app.Shutdown()
}

// StartParams matches the harness protocol start request.
type StartParams struct {
	SessionID      string `json:"session_id"`
	ClientID       string `json:"client_id,omitempty"`
	DisplayName    string `json:"display_name,omitempty"`
	AgentID        string `json:"agent_id,omitempty"`
	Prompt         string `json:"prompt,omitempty"`
	Resume         bool   `json:"resume,omitempty"`
	Fork           string `json:"fork,omitempty"`
	Model          string `json:"model,omitempty"`
	WorkDir        string `json:"work_dir,omitempty"`
	ApprovalMode   string `json:"permission_mode,omitempty"`
	Sandbox        string `json:"sandbox,omitempty"`
	SystemPrompt   string `json:"system_prompt,omitempty"`
	Effort         string `json:"effort,omitempty"`
	AutoApprove    *bool  `json:"auto_approve,omitempty"`
}

// MessageParams matches the harness protocol message request.
type MessageParams struct {
	Content string `json:"content"`
}

// SetModelParams matches the harness protocol set_model request.
type SetModelParams struct {
	Model string `json:"model"`
}

// SetPermissionModeParams matches the harness protocol set_permission_mode request.
type SetPermissionModeParams struct {
	Mode string `json:"mode"`
}

// ControlParams matches the harness protocol control request.
type ControlParams struct {
	Subtype string            `json:"subtype"`
	Payload map[string]string `json:"payload,omitempty"`
}
