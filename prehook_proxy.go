package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// gateViaPrehook proxies a codex approval-request to the bridge-server's
// PreToolUse prehook URL and returns the decision.
//
// Why this exists: codex 0.130's PreToolUse hook firing is broken
// (https://github.com/openai/codex/issues/21639) — codex doesn't call
// the hook command even when one is configured. Instead, we configure
// codex with `approval_policy = on-request` so codex sends its NATIVE
// WebSocket *ApprovalRequest events to us when it's about to do
// something potentially risky. The bridge intercepts those and routes
// them through the same prehook URL CC uses, so bridge-server's
// permission flow is identical regardless of harness.
//
// The payload shape matches ccPrehookPayload on the server side — same
// JSON fields, same response contract — so the existing prehook handler
// works unchanged. The only difference vs CC: the codex BRIDGE makes
// the HTTP call instead of the codex CLI's hook runner.
//
// Returns (approved, reason). On HTTP / decode errors the call is denied
// (fail-closed) — a permission gate that returns "allow" on error would
// silently bypass the rule engine, which is exactly the bug we just
// finished diagnosing.
//
// timeout is intentionally generous (24h) because the prehook may park
// for a human resolver via the parked-asks banner; codex's own WebSocket
// keeps the approval-request open without timing out on our side.
func gateViaPrehook(ctx context.Context, baseURL, bridgeID, toolName string, toolInput any, toolUseID string) (bool, string) {
	payload := map[string]any{
		"session_id":   bridgeID,
		"tool_name":    toolName,
		"tool_input":   toolInput,
		"tool_use_id":  toolUseID,
		"hook_event_name": "PreToolUse",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[prehook-proxy] marshal payload: %v", err)
		return false, fmt.Sprintf("marshal prehook payload: %v", err)
	}

	url := baseURL + "/permission/codex-prehook/" + bridgeID
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Printf("[prehook-proxy] build request: %v", err)
		return false, fmt.Sprintf("build prehook request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// 24h timeout — covers the parked-ask human-resolve window. The bridge
	// prehook blocks until the user clicks Allow/Deny.
	client := &http.Client{Timeout: 24 * time.Hour}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[prehook-proxy] POST %s: %v", url, err)
		return false, fmt.Sprintf("prehook unreachable: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[prehook-proxy] read response: %v", err)
		return false, fmt.Sprintf("read prehook response: %v", err)
	}
	if resp.StatusCode/100 != 2 {
		log.Printf("[prehook-proxy] HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 200))
		return false, fmt.Sprintf("prehook HTTP %d", resp.StatusCode)
	}

	var out struct {
		HookSpecificOutput struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		log.Printf("[prehook-proxy] decode response: %v", err)
		return false, fmt.Sprintf("decode prehook response: %v", err)
	}

	decision := out.HookSpecificOutput.PermissionDecision
	reason := out.HookSpecificOutput.PermissionDecisionReason
	log.Printf("[prehook-proxy] %s → %s (%s)", toolName, decision, truncate(reason, 80))

	// "allow" is the only outcome that permits execution. "deny" and "ask"
	// both block — for the codex approval-request path, "ask" can't be
	// answered via a return value (codex expects approve|deny), so we
	// treat it as deny with a hint in the reason. The parked-ask flow on
	// the bridge side ALREADY surfaces a banner for "ask"; what we return
	// here is just what codex itself sees after the human resolved.
	//
	// Note: in practice, when the bridge parks, gateViaPrehook blocks
	// until the user clicks; if they pick Allow the prehook returns
	// permissionDecision=allow, so this branch fires only on outright
	// denies (rules-based) or on the fail-closed paths above.
	return decision == "allow", reason
}
