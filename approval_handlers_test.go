package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// gateApproval and the five *ApprovalRequest handlers that call it had no test
// of any kind until this file. Measured 2026-08-15 by the 191st pass and
// re-measured here before a line was written: the string "gateApproval" appears
// in exactly one place under *_test.go on this branch, and it is a COMMENT — the
// call-graph note at the top of prehook_proxy_test.go. Nothing drove the
// function.
//
// prehook_contract_test.go (191st pass) scores the layer below this one:
// gateViaPrehook's request contract and its five fail-closed returns, 28/28. This
// file is about the frame above it, and the split matters more than usual here,
// because the two frames disagree about what to do when something is wrong.
//
// # The one path on this file that fails OPEN
//
//	if baseURL == "" {
//	    log.Printf("[approval] bridgeServerURL unset — falling back to auto-approve %s", toolName)
//	    approved = true
//
// gateViaPrehook is fail-closed on all five of its error returns. Its caller then
// auto-approves EVERY tool call when the bridge URL is empty — by design, with a
// loud log, and reached by a plain empty string. Whatever one thinks of that
// trade, it is the single branch on this path that lets a tool run without a rule
// ever being consulted. Nothing asserted it approved, nothing asserted it logged,
// and nothing asserted it was confined to the empty-URL case, so a condition
// drifting to something reachable in a CONFIGURED session would have been a
// silent, fleet-wide permission bypass with every suite green.
//
// How reachable it is TODAY, measured rather than assumed, because the paragraph
// above reads worse than the truth and the next reader deserves the number:
// SetBridgeServerURL has exactly one caller (handler.go:68, immediately after
// NewTranslator), and it is passed cfg.BridgeServerURL, which comes from
// envOr("LLMBRIDGE_SERVER_URL", "http://localhost:8160") — and envOr returns its
// fallback for an empty env var as well as an unset one. So no configuration can
// currently produce an empty URL, and the branch is unreachable in production.
//
// It is worth testing anyway, and the reason is the whole point of this file:
// NOTHING KEEPS IT UNREACHABLE. Its unreachability is a property of one caller
// and one helper's empty-string handling, both several files away, neither
// mentioning the gate. A second construction site, a switch from envOr to
// os.LookupEnv, or a config field that legitimately allows "" would each make it
// live, and none of them would look like a permissions change while being made.
//
// # The five tool_input shapes permission-store actually matches on
//
// Each handler builds its own map, and those maps are what reach the rule engine
// as tool_input. prehook_contract_test.go pins the payload envelope for ONE
// synthetic tool_input; it is not a claim about any of the five that ship. A key
// renamed in one of these handlers changes which permission-store rules match,
// silently, with every suite still green — so the assertions below compare the
// WHOLE decoded map rather than picking fields out of it. A per-field lookup
// answers "is command right?" and cannot answer "is there a key here nobody
// expects, and did the one the server matches on just disappear?"
//
// # Beyond the card's list
//
// The card that filed this work (59e81f00) named the five handlers, the two empty
// tool_use_ids and execCommandApproval's discarded unmarshal error, and warned in
// its own body to read that list as candidates. It was right to: RegisterApprovalHandlers
// registers NINE methods, and the other four — the two headless refusals, the
// elicitation cancel and the auth-token refusal — were untested for the same
// reason and are covered here too. So is the EventApproval every gated call emits,
// which is the only record of the decision that reaches the event log and the UI.
//
// None of these tests read the [prehook-proxy] log lines. That is deliberate and
// carried over from prehook_contract_test.go: those strings are
// sabotage-truncation.py's target, and a second suite asserting on them would make
// either one's failure ambiguous. The [approval] line IS read here — it belongs to
// this frame, no other scorer touches it, and it is the entire user-visible signal
// that gating has been switched off.

const (
	approvalBridgeID = "bridge-192"
	approvalItemID   = "item-192"
	// approvalThreadID is the codex-side session id, and it is deliberately NOT
	// equal to the bridge id. NewTranslator sets both fields from one argument,
	// so a fixture that skipped SetSessionID could not tell the two apart: the
	// snapshot in gateApproval reading t.sessionID instead of t.bridgeID would
	// gate under a session permission-store has never heard of, and every
	// assertion on session_id would still pass.
	approvalThreadID = "thread-192"
)

// approvalFixture is a Translator with the approval handlers registered, plus
// every event it emitted.
type approvalFixture struct {
	translator *Translator
	server     *AppServer
	events     []msg.Event
}

// newApprovalFixture wires a Translator to baseURL and registers its handlers.
//
// Events are appended from whichever goroutine calls a handler; every test here
// calls them serially and reads the slice afterwards, so no lock is needed.
func newApprovalFixture(t *testing.T, baseURL string) *approvalFixture {
	t.Helper()
	f := &approvalFixture{}
	f.translator = NewTranslator(approvalBridgeID, "client-192", func(e msg.Event) {
		f.events = append(f.events, e)
	})
	f.translator.SetSessionID(approvalThreadID)
	f.translator.SetBridgeServerURL(baseURL)
	f.server = NewAppServer("codex", 0, t.TempDir())
	f.translator.RegisterApprovalHandlers(f.server)
	return f
}

// call drives one registered handler with the exact bytes codex would send.
//
// Taking the handler out of the AppServer's own map — rather than calling the
// closure directly — is what makes a method name part of the test. A handler
// registered under a name codex never sends is a handler that never runs, and it
// looks identical to a working one from inside the closure.
func (f *approvalFixture) call(t *testing.T, method, params string) (json.RawMessage, error) {
	t.Helper()
	h, ok := f.server.reqHandlers[method]
	if !ok {
		t.Fatalf("no handler registered for %q; registered: %v", method, f.registeredMethods())
	}
	return h(method, json.RawMessage(params))
}

func (f *approvalFixture) registeredMethods() []string {
	methods := make([]string, 0, len(f.server.reqHandlers))
	for m := range f.server.reqHandlers {
		methods = append(methods, m)
	}
	return methods
}

// decodeApprovalResponse reads what codex itself receives — the wire bytes, not a
// Go struct. The field names are the contract with codex's app-server; decoding
// into ApprovalResponse would make a renamed json tag invisible, because both
// sides of the comparison would move together.
func decodeApprovalResponse(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the approval response is not valid JSON: %v (body %q)", err, raw)
	}
	return got
}

// decodePrehookPayload lifts the body the stub bridge-server received.
func decodePrehookPayload(t *testing.T, got *prehookCall) map[string]any {
	t.Helper()
	if got.method == "" {
		t.Fatalf("no request reached the prehook at all; the gate was never consulted")
	}
	if got.decodeErr != nil {
		t.Fatalf("fixture is malformed: could not read the recorded body: %v", got.decodeErr)
	}
	var payload map[string]any
	if err := json.Unmarshal(got.rawBody, &payload); err != nil {
		t.Fatalf("the prehook body is not valid JSON: %v (body %q)", err, got.rawBody)
	}
	return payload
}

// --- the nine registered methods ------------------------------------------

// TestRegisterApprovalHandlersRegistersEveryMethodCodexSends pins the method
// names themselves.
//
// This is the one defect no other test in this file can reach. Every assertion
// below drives a handler BY name, so a handler registered under a misspelled
// method is caught only if some test names the correct spelling and finds
// nothing. Comparing the whole key set also catches the opposite drift — a
// handler quietly dropped — which would leave codex's request unanswered and the
// session hung, with no test red anywhere.
func TestRegisterApprovalHandlersRegistersEveryMethodCodexSends(t *testing.T) {
	f := newApprovalFixture(t, "http://unused.invalid")

	want := map[string]bool{
		"item/commandExecution/requestApproval": true,
		"item/fileChange/requestApproval":       true,
		"item/permissions/requestApproval":      true,
		"applyPatchApproval":                    true,
		"execCommandApproval":                   true,
		"item/tool/requestUserInput":            true,
		"item/tool/call":                        true,
		"mcpServer/elicitation/request":         true,
		"account/chatgptAuthTokens/refresh":     true,
	}
	got := map[string]bool{}
	for _, m := range f.registeredMethods() {
		got[m] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RegisterApprovalHandlers registered the wrong method set:\n got %v\nwant %v", got, want)
	}
}

// --- what reaches permission-store ----------------------------------------

// approvalHandlerCases are the five gated handlers, each with the exact JSON
// codex sends and the exact tool_name / tool_input / tool_use_id that must come
// out the other side.
//
// The params are spelled as raw wire bytes rather than built from the Go request
// structs on purpose: the structs carry the json tags under test, so a fixture
// that marshalled them would rename the incoming field and the outgoing one
// together and never fail.
var approvalHandlerCases = []struct {
	name      string
	method    string
	params    string
	wantTool  string
	wantInput map[string]any
	wantUseID string
}{
	{
		name:      "commandExecution",
		method:    "item/commandExecution/requestApproval",
		params:    `{"threadId":"thread-1","itemId":"` + approvalItemID + `","command":"rm -rf /tmp/x"}`,
		wantTool:  "unified_exec",
		wantInput: map[string]any{"command": "rm -rf /tmp/x"},
		wantUseID: approvalItemID,
	},
	{
		name:      "fileChange",
		method:    "item/fileChange/requestApproval",
		params:    `{"threadId":"thread-1","itemId":"` + approvalItemID + `","path":"/etc/hosts","patch":"@@ -1 +1 @@"}`,
		wantTool:  "apply_patch",
		wantInput: map[string]any{"path": "/etc/hosts", "patch": "@@ -1 +1 @@"},
		wantUseID: approvalItemID,
	},
	{
		name:      "permissions",
		method:    "item/permissions/requestApproval",
		params:    `{"threadId":"thread-1","itemId":"` + approvalItemID + `","permissions":["network","disk"]}`,
		wantTool:  "request_permissions",
		wantInput: map[string]any{"permissions": []any{"network", "disk"}},
		wantUseID: approvalItemID,
	},
	{
		// The odd one out: this handler forwards the RAW params as tool_input
		// rather than building a map, so every field codex sent reaches
		// permission-store — including threadId, which the three handlers above
		// drop. A rule matching on tool_input keys sees a different shape here
		// than it does from item/fileChange, for the same apply_patch tool name.
		name:     "applyPatchApproval",
		method:   "applyPatchApproval",
		params:   `{"threadId":"thread-1","itemId":"ignored","path":"/etc/hosts","patch":"@@ -1 +1 @@"}`,
		wantTool: "apply_patch",
		wantInput: map[string]any{
			"threadId": "thread-1", "itemId": "ignored",
			"path": "/etc/hosts", "patch": "@@ -1 +1 @@",
		},
		wantUseID: "",
	},
	{
		name:      "execCommandApproval",
		method:    "execCommandApproval",
		params:    `{"command":"curl evil.example"}`,
		wantTool:  "unified_exec",
		wantInput: map[string]any{"command": "curl evil.example"},
		wantUseID: "",
	},
}

// TestApprovalHandlersSendTheShapePermissionStoreMatchesOn is the core of this
// file: for each of the five handlers, the tool name and the decoded tool_input
// that reach the rule engine.
func TestApprovalHandlersSendTheShapePermissionStoreMatchesOn(t *testing.T) {
	for _, tc := range approvalHandlerCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := recordingPrehook(t, respondAllow("approved by rule"))
			f := newApprovalFixture(t, srv.URL)

			if _, err := f.call(t, tc.method, tc.params); err != nil {
				t.Fatalf("%s returned an error for a well-formed request: %v", tc.method, err)
			}

			payload := decodePrehookPayload(t, got)
			if payload["tool_name"] != tc.wantTool {
				t.Errorf("%s gated as tool_name %q, want %q — permission-store rules are keyed on this",
					tc.method, payload["tool_name"], tc.wantTool)
			}
			if !reflect.DeepEqual(payload["tool_input"], map[string]any(tc.wantInput)) {
				t.Errorf("%s sent a tool_input the rules will not match:\n got %#v\nwant %#v",
					tc.method, payload["tool_input"], tc.wantInput)
			}
			if payload["tool_use_id"] != tc.wantUseID {
				t.Errorf("%s sent tool_use_id %q, want %q — this correlates the approval with its parked-ask banner",
					tc.method, payload["tool_use_id"], tc.wantUseID)
			}
			// The session the gate is asked about is the bridge id, never the
			// codex-side thread or item id. Getting this wrong asks
			// permission-store about a session that does not exist, which no
			// per-handler assertion above would notice.
			if payload["session_id"] != approvalBridgeID {
				t.Errorf("%s gated under session_id %q, want the bridge id %q",
					tc.method, payload["session_id"], approvalBridgeID)
			}
		})
	}
}

// TestApprovalHandlersReturnCodexTheDecision pins the response envelope — the
// bytes codex reads to decide whether the tool runs.
//
// Both directions are driven for every handler. A handler wired to return a
// constant true would pass a deny-only test and an allow-only test tells nothing
// apart at all; only running the same handler against both decisions shows the
// value is carried rather than invented.
func TestApprovalHandlersReturnCodexTheDecision(t *testing.T) {
	for _, tc := range approvalHandlerCases {
		for _, decision := range []struct {
			name         string
			wire         string
			wantApproved bool
		}{
			{"allowed", "allow", true},
			{"denied", "deny", false},
		} {
			t.Run(tc.name+"/"+decision.name, func(t *testing.T) {
				reason := "because " + decision.wire
				srv, _ := recordingPrehook(t, func(w http.ResponseWriter, r *http.Request) {
					if err := writeDecision(w, decision.wire, reason); err != nil {
						t.Errorf("fixture is malformed: encode decision: %v", err)
					}
				})
				f := newApprovalFixture(t, srv.URL)

				raw, err := f.call(t, tc.method, tc.params)
				if err != nil {
					t.Fatalf("%s returned an error for a well-formed request: %v", tc.method, err)
				}

				want := map[string]any{"approved": decision.wantApproved, "reason": reason}
				// reason is omitempty, so an empty one would drop the key
				// entirely; every decision here carries a reason, so the whole
				// map is comparable and a renamed key is visible.
				if got := decodeApprovalResponse(t, raw); !reflect.DeepEqual(got, want) {
					t.Errorf("%s answered codex with the wrong envelope:\n got %#v\nwant %#v",
						tc.method, got, want)
				}
			})
		}
	}
}

// --- the branch that fails open -------------------------------------------

// TestUnconfiguredBridgeURLAutoApprovesWithoutConsultingAnyRule is the row this
// card was filed for. An empty bridgeServerURL approves every tool call, and
// before this test nothing said so.
//
// The stub is started and then deliberately NOT given to the translator: it is
// there so "no rule was consulted" is a measurement rather than an inference. A
// version that still called the gate and ignored its answer would pass an
// approved-is-true assertion on its own.
func TestUnconfiguredBridgeURLAutoApprovesWithoutConsultingAnyRule(t *testing.T) {
	_, got := recordingPrehook(t, func(w http.ResponseWriter, r *http.Request) {
		if err := writeDecision(w, "deny", "the gate must never be reached"); err != nil {
			t.Errorf("fixture is malformed: encode decision: %v", err)
		}
	})

	f := newApprovalFixture(t, "")

	var raw json.RawMessage
	var err error
	out := captureLog(t, func() {
		raw, err = f.call(t, "item/commandExecution/requestApproval",
			`{"threadId":"thread-1","itemId":"`+approvalItemID+`","command":"rm -rf /"}`)
	})
	if err != nil {
		t.Fatalf("the unconfigured fallback returned an error: %v", err)
	}

	resp := decodeApprovalResponse(t, raw)
	if resp["approved"] != true {
		t.Errorf("an unset bridgeServerURL must auto-approve (legacy behaviour); got %#v", resp)
	}
	if got.method != "" {
		t.Errorf("the unconfigured fallback consulted the prehook at %s %q; it must not make the call at all",
			got.method, got.path)
	}

	// The log line is the whole user-visible signal that gating is off. A silent
	// fallback and a loud one are the same green tick without this.
	if !strings.Contains(out, "[approval]") {
		t.Errorf("the auto-approve fallback logged nothing under [approval]; captured %q", out)
	}
	if !strings.Contains(out, "auto-approve") {
		t.Errorf("the fallback log must say it is auto-approving; captured %q", out)
	}
	// Naming the tool is what makes the line usable: a bare "gating is off"
	// cannot tell an operator which calls went ungated.
	if !strings.Contains(out, "unified_exec") {
		t.Errorf("the fallback log must name the tool it let through; captured %q", out)
	}
	// The reason is what codex shows the user, and it is the only place the
	// answer says WHY it was approved.
	if reason, _ := resp["reason"].(string); !strings.Contains(reason, "no bridge URL") {
		t.Errorf("the fallback reason must say no bridge URL was configured; got %q", reason)
	}
}

// TestTheAutoApproveFallbackIsConfinedToAnEmptyURL is the other half, and the
// half that catches drift.
//
// The test above pins what happens when the URL is empty; on its own it cannot
// tell that condition from one that is true more often. A gate rewritten to
// `baseURL == "" || err != nil`, or to any condition a configured session can
// reach, keeps that test green and switches permissions off in production. So
// this one configures a URL, has the gate deny, and requires the denial to
// survive.
func TestTheAutoApproveFallbackIsConfinedToAnEmptyURL(t *testing.T) {
	srv, got := recordingPrehook(t, func(w http.ResponseWriter, r *http.Request) {
		if err := writeDecision(w, "deny", "blocked by rule"); err != nil {
			t.Errorf("fixture is malformed: encode decision: %v", err)
		}
	})
	f := newApprovalFixture(t, srv.URL)

	out := captureLog(t, func() {
		raw, err := f.call(t, "item/commandExecution/requestApproval",
			`{"threadId":"thread-1","itemId":"`+approvalItemID+`","command":"rm -rf /"}`)
		if err != nil {
			t.Fatalf("a configured gate returned an error: %v", err)
		}
		if resp := decodeApprovalResponse(t, raw); resp["approved"] != false {
			t.Errorf("a configured gate denied the call and the handler approved it anyway: %#v", resp)
		}
	})

	if got.method == "" {
		t.Error("a configured bridgeServerURL must consult the prehook; no request arrived")
	}
	// The fallback's own log must be absent. Without this, a gate that took the
	// fallback branch AND then called through would read as correct.
	if strings.Contains(out, "[approval]") {
		t.Errorf("a configured session took the unconfigured auto-approve branch; captured %q", out)
	}
}

// TestAFailClosedGateStaysClosedThroughTheHandler checks the two frames agree.
//
// gateViaPrehook fails closed on all five of its error returns and
// prehook_contract_test.go scores that. What neither that file nor the tests
// above can show is that the denial SURVIVES the frame above it: gateApproval
// chooses between the fallback and the gate, and a handler that treated an
// unreachable bridge the way it treats an unconfigured one would turn every one
// of those fail-closed returns into an approval.
func TestAFailClosedGateStaysClosedThroughTheHandler(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	f := newApprovalFixture(t, deadURL)
	raw, err := f.call(t, "item/commandExecution/requestApproval",
		`{"threadId":"thread-1","itemId":"`+approvalItemID+`","command":"rm -rf /"}`)
	if err != nil {
		t.Fatalf("an unreachable bridge returned an error instead of a denial: %v", err)
	}
	if resp := decodeApprovalResponse(t, raw); resp["approved"] != false {
		t.Errorf("an unreachable bridge must deny, not fall back to auto-approve; got %#v", resp)
	}
}

// --- the canonical event --------------------------------------------------

// TestEveryGatedCallEmitsTheCanonicalApprovalEvent pins the record of the
// decision that reaches the event log and the UI.
//
// This is a separate surface from the response codex gets, and it can drift on
// its own: the event carries the decision as two strings (action and status)
// where the wire carries a bool, so a denial that answers codex correctly can
// still be written into the log as approved.
func TestEveryGatedCallEmitsTheCanonicalApprovalEvent(t *testing.T) {
	for _, tc := range []struct {
		decision   string
		wantAction string
		wantStatus string
	}{
		{"allow", "approve", "approved"},
		{"deny", "deny", "denied"},
		{"ask", "deny", "denied"},
	} {
		t.Run("decision="+tc.decision, func(t *testing.T) {
			reason := "because " + tc.decision
			srv, _ := recordingPrehook(t, func(w http.ResponseWriter, r *http.Request) {
				if err := writeDecision(w, tc.decision, reason); err != nil {
					t.Errorf("fixture is malformed: encode decision: %v", err)
				}
			})
			f := newApprovalFixture(t, srv.URL)

			params := `{"threadId":"thread-1","itemId":"` + approvalItemID + `","command":"ls"}`
			if _, err := f.call(t, "item/commandExecution/requestApproval", params); err != nil {
				t.Fatalf("handler returned an error: %v", err)
			}

			var approvals []msg.Event
			for _, e := range f.events {
				if e.Type == msg.EventApproval {
					approvals = append(approvals, e)
				}
			}
			if len(approvals) != 1 {
				t.Fatalf("want exactly one approval event per gated call, got %d", len(approvals))
			}
			got := approvals[0].Approval
			if got == nil {
				t.Fatalf("the approval event carries no Approval payload")
			}
			if got.Action != tc.wantAction || got.Status != tc.wantStatus {
				t.Errorf("decision %q logged as action=%q status=%q, want action=%q status=%q",
					tc.decision, got.Action, got.Status, tc.wantAction, tc.wantStatus)
			}
			if got.ToolName != "unified_exec" {
				t.Errorf("the approval event names tool %q, want unified_exec", got.ToolName)
			}
			if got.Detail != reason {
				t.Errorf("the approval event's detail is %q, want the gate's whole reason %q",
					got.Detail, reason)
			}
			// Raw is the original WebSocket payload, preserved so a consumer can
			// see what codex actually asked about. Dropping it costs nothing at
			// compile time and everything at diagnosis time.
			if string(approvals[0].Raw) != params {
				t.Errorf("the approval event's Raw is %q, want the untouched codex payload %q",
					approvals[0].Raw, params)
			}
		})
	}
}

// TestTheUnconfiguredFallbackIsAlsoRecordedAsAnApproval covers the same event on
// the branch that never calls the gate. An auto-approved call that emitted no
// event would be invisible to the event log — ungated AND unlogged, which is the
// worst pair of the four.
func TestTheUnconfiguredFallbackIsAlsoRecordedAsAnApproval(t *testing.T) {
	f := newApprovalFixture(t, "")

	captureLog(t, func() {
		if _, err := f.call(t, "execCommandApproval", `{"command":"ls"}`); err != nil {
			t.Fatalf("handler returned an error: %v", err)
		}
	})

	var approvals []msg.Event
	for _, e := range f.events {
		if e.Type == msg.EventApproval {
			approvals = append(approvals, e)
		}
	}
	if len(approvals) != 1 {
		t.Fatalf("the auto-approve fallback must still emit one approval event, got %d", len(approvals))
	}
	if got := approvals[0].Approval; got.Status != "approved" || got.Action != "approve" {
		t.Errorf("the fallback logged action=%q status=%q, want approve/approved", got.Action, got.Status)
	}
}

// --- malformed payloads ---------------------------------------------------

// TestTypedApprovalHandlersRefuseAMalformedPayload covers the three handlers that
// decode into a struct and return the decode error.
//
// The assertion that matters is the second one: the gate must not be consulted.
// Returning an error and ALSO gating would ask permission-store about a
// zero-valued command, and a rule written to allow a harmless one would match it.
func TestTypedApprovalHandlersRefuseAMalformedPayload(t *testing.T) {
	for _, method := range []string{
		"item/commandExecution/requestApproval",
		"item/fileChange/requestApproval",
		"item/permissions/requestApproval",
	} {
		t.Run(method, func(t *testing.T) {
			srv, got := recordingPrehook(t, respondAllow("must not be reached"))
			f := newApprovalFixture(t, srv.URL)

			raw, err := f.call(t, method, `{"itemId":`)
			if err == nil {
				t.Errorf("%s accepted a truncated payload and answered %q", method, raw)
			}
			if got.method != "" {
				t.Errorf("%s gated on an undecodable payload; the prehook was called", method)
			}
		})
	}
}

// TestExecCommandApprovalGatesOnAnEmptyCommandWhenItsPayloadIsMalformed pins the
// behaviour of the one handler that does NOT refuse.
//
//	_ = json.Unmarshal(params, &req)
//
// The error is discarded, so a payload codex could not encode gates on an empty
// command instead of failing. This test asserts the behaviour that ships rather
// than the one that might be preferred — an empty command reaching the rule
// engine is a real event, and a rule that allows an empty string would let it
// through. Pinning it is what makes changing it a decision rather than an
// accident.
func TestExecCommandApprovalGatesOnAnEmptyCommandWhenItsPayloadIsMalformed(t *testing.T) {
	srv, got := recordingPrehook(t, respondAllow("approved by rule"))
	f := newApprovalFixture(t, srv.URL)

	if _, err := f.call(t, "execCommandApproval", `{"command":`); err != nil {
		t.Fatalf("execCommandApproval discards its unmarshal error, so it must not return one: %v", err)
	}

	payload := decodePrehookPayload(t, got)
	want := map[string]any{"command": ""}
	if !reflect.DeepEqual(payload["tool_input"], want) {
		t.Errorf("a malformed payload gated as %#v, want %#v — the discarded unmarshal error leaves an empty command",
			payload["tool_input"], want)
	}
}

// --- the four handlers that are not approvals -----------------------------

// TestHeadlessHandlersRefuseWhatNoOneCanAnswer covers the three methods
// RegisterApprovalHandlers answers with an RPC error.
//
// These share the function with the approval gate and were untested for the same
// reason. The failure they prevent is a hang: codex sends a server->client
// request and waits, and a handler that returned a nil result and a nil error
// would answer with something codex cannot act on.
func TestHeadlessHandlersRefuseWhatNoOneCanAnswer(t *testing.T) {
	for _, tc := range []struct {
		method   string
		wantWord string
	}{
		{"item/tool/requestUserInput", "user input"},
		{"item/tool/call", "dynamic tool calls"},
		{"account/chatgptAuthTokens/refresh", "auth tokens"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			f := newApprovalFixture(t, "http://unused.invalid")

			raw, err := f.call(t, tc.method, `{}`)
			if err == nil {
				t.Fatalf("%s must refuse in headless mode; it answered %q", tc.method, raw)
			}
			rpcErr, ok := err.(*RPCError)
			if !ok {
				t.Fatalf("%s returned a plain error (%v); codex needs an *RPCError to answer the request",
					tc.method, err)
			}
			if rpcErr.Code != -32000 {
				t.Errorf("%s refused with code %d, want -32000", tc.method, rpcErr.Code)
			}
			if !strings.Contains(rpcErr.Message, tc.wantWord) {
				t.Errorf("%s refused with %q, which does not say what was refused (want it to mention %q)",
					tc.method, rpcErr.Message, tc.wantWord)
			}
		})
	}
}

// TestElicitationIsCancelledRatherThanRefused covers the fourth, and it is the
// odd one out on purpose: an MCP elicitation is answered with a successful
// "cancel" action, not an RPC error.
//
// Nothing else distinguishes the two shapes. A handler rewritten to refuse this
// one the way the three above are refused would compile, and the MCP server would
// see a protocol error where it expected a user declining.
func TestElicitationIsCancelledRatherThanRefused(t *testing.T) {
	f := newApprovalFixture(t, "http://unused.invalid")

	raw, err := f.call(t, "mcpServer/elicitation/request", `{"message":"pick one"}`)
	if err != nil {
		t.Fatalf("an elicitation must be cancelled, not refused with an error: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the elicitation answer is not valid JSON: %v (body %q)", err, raw)
	}
	if want := map[string]any{"action": "cancel"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the elicitation was answered %#v, want %#v", got, want)
	}
}
