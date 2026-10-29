package main

import "github.com/kayushkin/llm-bridge/msg"

// codexCallUsage is what codex reports for one model call: last_token_usage in
// a rollout, tokenUsage.last in an app-server notification. Codex follows
// OpenAI's convention, where InputTokens includes the cached part.
type codexCallUsage struct {
	InputTokens           int
	CachedInputTokens     int
	OutputTokens          int
	ReasoningOutputTokens int
}

// addCallToTurnUsage adds one call to the turn's usage.
//
// A result's usage is the whole turn's: llm-bridge-server adds every result's
// usage into the session's (applyResultUsage), as Claude Code's wrapper reports
// it. Codex reports each call once (its own total equals the sum of the calls,
// checked on six sessions), so the turn is the sum of its calls. This used to
// keep only the latest call, which dropped every other call of the turn, and
// put the thread's running total in ContextTokens, which is a snapshot of the
// current context, not a sum.
//
// InputTokens excludes the cached part, as in the Claude Code wrapper and in
// this wrapper's -oneshot, so the cache is counted once, in CacheReadTokens.
// ContextTokens is the last call's whole prompt, cached part included.
func addCallToTurnUsage(turn *msg.TokenUsage, call codexCallUsage, contextLimit int) {
	turn.InputTokens += call.InputTokens - call.CachedInputTokens
	turn.CacheReadTokens += call.CachedInputTokens
	turn.OutputTokens += call.OutputTokens
	turn.ReasoningTokens += call.ReasoningOutputTokens
	turn.TotalTokens += call.InputTokens + call.OutputTokens
	turn.ContextTokens = call.InputTokens
	if contextLimit != 0 {
		turn.ContextLimit = contextLimit
	}
}
