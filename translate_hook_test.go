package main

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func newHookTranslator() *Translator {
	return NewTranslator("thread-1", "client-1", func(msg.Event) {})
}

const startedPayload = `{"threadId":"th-1","turnId":"turn-1","run":{"id":"run-9","eventName":"preToolUse","status":"running","entries":[]}}`

func TestHookEventStartedCarriesNativeEventName(t *testing.T) {
	event := hookEvent(newHookTranslator(), json.RawMessage(startedPayload), "started")

	if event.Type != msg.EventHook {
		t.Fatalf("type = %v, want %v", event.Type, msg.EventHook)
	}
	if event.Hook == nil {
		t.Fatal("hook detail missing")
	}
	// The canonical contract asks for the harness's own spelling. Codex says
	// "preToolUse"; rewriting it to Claude Code's "PreToolUse" would assert an
	// equivalence the two harnesses have not agreed on.
	if event.Hook.Event != "preToolUse" {
		t.Errorf("event = %q, want preToolUse", event.Hook.Event)
	}
	if event.Hook.Phase != "started" {
		t.Errorf("phase = %q, want started", event.Hook.Phase)
	}
	if event.Hook.HookID != "run-9" {
		t.Errorf("hook id = %q, want run-9", event.Hook.HookID)
	}
	if len(event.Hook.Input) == 0 {
		t.Error("started event should carry the payload as Input")
	}
	if len(event.Hook.Output) != 0 {
		t.Error("started event should not carry Output")
	}
	if len(event.Raw) == 0 {
		t.Error("raw payload should pass through")
	}
}

func TestHookEventCompletedCarriesOutput(t *testing.T) {
	payload := `{"threadId":"th-1","run":{"id":"run-9","eventName":"postToolUse","status":"completed"}}`
	event := hookEvent(newHookTranslator(), json.RawMessage(payload), "completed")

	if event.Hook.Phase != "completed" {
		t.Errorf("phase = %q, want completed", event.Hook.Phase)
	}
	if len(event.Hook.Output) == 0 {
		t.Error("completed event should carry the payload as Output")
	}
	if len(event.Hook.Input) != 0 {
		t.Error("completed event should not carry Input")
	}
	// A run that simply finished reports no decision, and inventing "allow"
	// would claim the hook approved something it never ruled on.
	if event.Hook.Decision != "" {
		t.Errorf("decision = %q, want empty for a completed run", event.Hook.Decision)
	}
}

// "blocked" is the one run status that maps onto the canonical decision without
// guesswork: the hook stopped the tool call.
func TestHookEventBlockedRunReportsDeny(t *testing.T) {
	payload := `{"threadId":"th-1","run":{"id":"run-9","eventName":"preToolUse","status":"blocked"}}`
	event := hookEvent(newHookTranslator(), json.RawMessage(payload), "completed")

	if event.Hook.Decision != "deny" {
		t.Errorf("decision = %q, want deny", event.Hook.Decision)
	}
}

// Statuses other than "blocked" have no agreed canonical decision, so they must
// not be forced into one.
func TestHookEventOtherStatusesReportNoDecision(t *testing.T) {
	for _, status := range []string{"running", "completed", "failed", "stopped"} {
		payload := `{"threadId":"th-1","run":{"id":"r","eventName":"stop","status":"` + status + `"}}`
		event := hookEvent(newHookTranslator(), json.RawMessage(payload), "completed")
		if event.Hook.Decision != "" {
			t.Errorf("status %q gave decision %q, want empty", status, event.Hook.Decision)
		}
	}
}

// A payload we cannot decode still means a hook ran. Dropping the event would
// hide that from the client entirely; Raw keeps the original bytes readable.
func TestHookEventMalformedPayloadStillReportsTheRun(t *testing.T) {
	event := hookEvent(newHookTranslator(), json.RawMessage(`{"run":`), "started")

	if event.Type != msg.EventHook {
		t.Fatalf("type = %v, want %v", event.Type, msg.EventHook)
	}
	if event.Hook == nil || event.Hook.Phase != "started" {
		t.Fatalf("hook = %+v, want phase started", event.Hook)
	}
	if len(event.Raw) == 0 {
		t.Error("raw payload should pass through even when it does not decode")
	}
}
