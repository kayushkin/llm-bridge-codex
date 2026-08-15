package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// gateViaPrehook's REQUEST contract and its five fail-closed returns had no test
// until this file. prehook_proxy_test.go (190th pass) covers the two log budgets
// and says so in its own header; the card that filed this one, 07215b2e, is the
// honest remainder of that night.
//
// Two different things live here, and the split is the point:
//
//   - The request contract. gateViaPrehook's doc comment claims the payload
//     "matches ccPrehookPayload on the server side — same JSON fields, same
//     response contract". That is a CROSS-REPO claim, and before this file
//     nothing on either side held it: no fixture decoded the body at all, so all
//     five fields, the path, the method and the Content-Type were free to drift
//     into a shape the server would reject at runtime and no suite would notice.
//
//   - Fail-closed. The comment's own words: "a permission gate that returns
//     'allow' on error would silently bypass the rule engine, which is exactly
//     the bug we just finished diagnosing." There are five error returns and only
//     the decode-adjacent one was ever reached. A gate that fails OPEN on four of
//     five paths and a gate that fails closed on all five are the same green tick
//     without these tests.
//
// Measured before this file existed, by running the scorer's 28 mutations
// against the WHOLE pre-existing suite: 10/28. All five fail-closed paths, all
// five payload fields, both sides of the status check and the "ask" decision read
// UNNOTICED; this file closes 18 of them.
//
// The other 10 are worth naming, because the card that filed this work listed
// four of them as unmeasured and they were not. prehook_proxy_test.go's
// assertShippedRequest already pinned the path, the method and the Content-Type,
// and the budget tests already caught a timeout shortened below one round trip
// and two of the three response-shape tags. Those assertions were firing all
// along — they were merely never SCORED, because the truncation scorer's -run
// filter never selected a mutation that could move them. Asserted and unscored
// is not the same as unmeasured, and only an unfiltered run tells them apart.
//
// None of these tests read log text. That is deliberate — the log lines are
// sabotage-truncation.py's target, and a second suite asserting on the same
// strings would make either one's failure ambiguous.

const (
	contractBridgeID  = "bridge-191"
	contractToolName  = "unified_exec"
	contractToolUseID = "call-191"
)

// contractToolInput is a nested value rather than a flat string on purpose: the
// field is typed `any` and is the one part of the payload whose shape the bridge
// does not control, so a fixture that only ever sends a string would not notice
// the field being flattened or re-encoded on the way out.
func contractToolInput() map[string]any {
	return map[string]any{"command": []any{"bash", "-lc", "ls"}, "timeout_ms": float64(5000)}
}

// wantPayload is the body the server side is promised, spelled out as data.
//
// Comparing the whole decoded map — rather than picking fields out of it — is
// what makes a RENAMED key visible. A per-field lookup answers "is session_id
// right?" and cannot answer "is there a key here nobody expects?", and a renamed
// field is precisely the drift that keeps this side compiling while the server
// silently reads a zero value.
func wantPayload() map[string]any {
	return map[string]any{
		"session_id":      contractBridgeID,
		"tool_name":       contractToolName,
		"tool_input":      contractToolInput(),
		"tool_use_id":     contractToolUseID,
		"hook_event_name": "PreToolUse",
	}
}

// prehookCall is what the stub bridge-server saw.
type prehookCall struct {
	method      string
	path        string
	contentType string
	rawBody     []byte
	decodeErr   error
}

// recordingPrehook starts a stub bridge-server that records the request
// gateViaPrehook sends and then hands the response over to respond.
//
// The recorded call is written in the handler goroutine and read by the test
// after gateViaPrehook returns; the response the handler writes is what releases
// the client, so the write happens-before the read.
func recordingPrehook(t *testing.T, respond http.HandlerFunc) (*httptest.Server, *prehookCall) {
	t.Helper()
	got := &prehookCall{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.contentType = r.Header.Get("Content-Type")
		got.rawBody, got.decodeErr = io.ReadAll(r.Body)
		respond(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// respondAllow writes the response shape the server side returns for an approval.
func respondAllow(reason string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := writeDecision(w, "allow", reason); err != nil {
			panic(err)
		}
	}
}

// gateWithContractArgs calls gateViaPrehook with this file's fixed arguments, so
// every test drives the same payload and only the response varies.
func gateWithContractArgs(baseURL string) (bool, string) {
	return gateViaPrehook(context.Background(), baseURL, contractBridgeID,
		contractToolName, contractToolInput(), contractToolUseID)
}

// TestPrehookRequestCarriesTheServerSideContract pins where the request goes,
// how it is framed, and every field in it.
func TestPrehookRequestCarriesTheServerSideContract(t *testing.T) {
	srv, got := recordingPrehook(t, respondAllow("approved by rule"))

	if approved, _ := gateWithContractArgs(srv.URL); !approved {
		t.Fatalf("an allow decision over a 200 must approve; gateViaPrehook denied")
	}

	if got.method != http.MethodPost {
		t.Errorf("the prehook must be POSTed; the request arrived as %s", got.method)
	}
	if want := "/permission/codex-prehook/" + contractBridgeID; got.path != want {
		t.Errorf("the prehook was posted to %q, want %q", got.path, want)
	}
	if got.contentType != "application/json" {
		t.Errorf("the prehook Content-Type is %q, want application/json", got.contentType)
	}

	if got.decodeErr != nil {
		t.Fatalf("fixture is malformed: could not read the recorded body: %v", got.decodeErr)
	}
	var payload map[string]any
	if err := json.Unmarshal(got.rawBody, &payload); err != nil {
		t.Fatalf("the prehook body is not valid JSON: %v (body %q)", err, got.rawBody)
	}
	if want := wantPayload(); !reflect.DeepEqual(payload, want) {
		t.Errorf("the prehook payload does not match the server-side contract:\n got %#v\nwant %#v",
			payload, want)
	}
}

// TestPrehookApprovesOnlyAnAllowDecision drives every decision the server can
// return. "ask" is the row worth having: gateViaPrehook's own comment promises
// "deny" and "ask" both block, and a gate written as `decision != "deny"` keeps
// the deny test green while letting every parked ask through.
func TestPrehookApprovesOnlyAnAllowDecision(t *testing.T) {
	for _, tc := range []struct {
		decision     string
		wantApproved bool
	}{
		{"allow", true},
		{"deny", false},
		{"ask", false},
		{"", false},
		// Case matters: the server sends a lowercase literal, and a gate that
		// accepted either spelling would approve on a value the server never sends.
		{"Allow", false},
	} {
		t.Run("decision="+tc.decision, func(t *testing.T) {
			reason := "because " + tc.decision
			srv, _ := recordingPrehook(t, func(w http.ResponseWriter, r *http.Request) {
				if err := writeDecision(w, tc.decision, reason); err != nil {
					t.Errorf("fixture is malformed: encode decision: %v", err)
				}
			})

			approved, gotReason := gateWithContractArgs(srv.URL)
			if approved != tc.wantApproved {
				t.Errorf("permissionDecision %q: approved = %v, want %v",
					tc.decision, approved, tc.wantApproved)
			}
			// The reason is what codex shows the user, and it is returned whole
			// on the approve path too — not only on the deny path the budget
			// tests already drive.
			if gotReason != reason {
				t.Errorf("permissionDecision %q: reason = %q, want the full %q",
					tc.decision, gotReason, reason)
			}
		})
	}
}

// TestPrehookAcceptsTheWholeTwoHundredFamily pins the /100 in the status check.
// Every other test here answers on a bare 200, so none of them can tell a gate
// that accepts the 2xx family from one that accepts exactly 200.
func TestPrehookAcceptsTheWholeTwoHundredFamily(t *testing.T) {
	srv, _ := recordingPrehook(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		if err := writeDecision(w, "allow", "approved late"); err != nil {
			t.Errorf("fixture is malformed: encode decision: %v", err)
		}
	})

	approved, reason := gateWithContractArgs(srv.URL)
	if !approved {
		t.Fatalf("HTTP 202 is a success and its allow decision must be honoured; got denied (%q)", reason)
	}
}

// TestPrehookFailsClosedOnARedirect covers the other side of that check. A 3xx
// carries no decision, so a gate that treated "not an error" as success would
// read the decision out of a redirect body.
//
// The stub sends no Location header on purpose: measured 2026-08-15, http.Client
// follows a 302 that HAS one and would turn this into a 200 before the code
// under test ever sees a 3xx.
func TestPrehookFailsClosedOnARedirect(t *testing.T) {
	srv, _ := recordingPrehook(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusFound)
		if err := writeDecision(w, "allow", "should never be read"); err != nil {
			t.Errorf("fixture is malformed: encode decision: %v", err)
		}
	})

	approved, reason := gateWithContractArgs(srv.URL)
	if approved {
		t.Fatalf("a 302 carries no decision and must fail closed; gateViaPrehook approved")
	}
	if !strings.Contains(reason, "302") {
		t.Errorf("the denial reason must name the status codex was refused on; got %q", reason)
	}
}

// TestPrehookFailsClosedWhenThePayloadCannotBeMarshalled reaches the first of the
// five error returns. tool_input is typed `any` and is filled from the wire, so a
// value json cannot encode is the caller's to hand over, not a hypothetical.
func TestPrehookFailsClosedWhenThePayloadCannotBeMarshalled(t *testing.T) {
	srv, _ := recordingPrehook(t, respondAllow("must not be reached"))

	approved, reason := gateViaPrehook(context.Background(), srv.URL, contractBridgeID,
		contractToolName, make(chan int), contractToolUseID)
	if approved {
		t.Fatalf("an unmarshalable tool_input must fail closed; gateViaPrehook approved")
	}
	if !strings.Contains(reason, "marshal") {
		t.Errorf("the denial reason must name the marshal failure; got %q", reason)
	}
}

// TestPrehookFailsClosedWhenTheRequestCannotBeBuilt reaches the second. baseURL
// arrives from configuration, so a URL net/url rejects is a deployment mistake
// rather than an impossible input — and a gate that approved on one would open
// on every call.
func TestPrehookFailsClosedWhenTheRequestCannotBeBuilt(t *testing.T) {
	approved, reason := gateWithContractArgs("http://\x7f")
	if approved {
		t.Fatalf("an unparseable base URL must fail closed; gateViaPrehook approved")
	}
	if !strings.Contains(reason, "build") {
		t.Errorf("the denial reason must name the request-building failure; got %q", reason)
	}
}

// TestPrehookFailsClosedWhenTheBridgeIsUnreachable reaches the third. This is the
// path a restarted or crashed bridge-server takes, which makes it the likeliest
// of the five in production and the worst one to fail open on.
func TestPrehookFailsClosedWhenTheBridgeIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close()

	approved, reason := gateWithContractArgs(deadURL)
	if approved {
		t.Fatalf("an unreachable prehook must fail closed; gateViaPrehook approved")
	}
	if !strings.Contains(reason, "unreachable") {
		t.Errorf("the denial reason must name the transport failure; got %q", reason)
	}
}

// TestPrehookFailsClosedWhenTheResponseBodyCannotBeRead reaches the fourth: the
// headers arrive, the connection dies mid-body, and io.ReadAll fails after
// client.Do already succeeded.
//
// The status is deliberately 500. gateViaPrehook reads the body BEFORE it checks
// the status, so this fixture also pins that order: a version that checked the
// status first would deny with "prehook HTTP 500" and never touch the read error.
// Both fail closed, so only the reason can tell them apart.
func TestPrehookFailsClosedWhenTheResponseBodyCannotBeRead(t *testing.T) {
	srv, _ := recordingPrehook(t, func(w http.ResponseWriter, r *http.Request) {
		// Promise more than is written, flush the headers, then abort the
		// connection: the client sees the headers, then an unexpected EOF.
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusInternalServerError)
		if _, err := io.WriteString(w, "trunc"); err != nil {
			t.Errorf("fixture is malformed: write short body: %v", err)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	})

	approved, reason := gateWithContractArgs(srv.URL)
	if approved {
		t.Fatalf("an unreadable response body must fail closed; gateViaPrehook approved")
	}
	if !strings.Contains(reason, "read") {
		t.Errorf("the body is read before the status is judged, so the denial reason must name the read failure; got %q", reason)
	}
}

// TestPrehookFailsClosedOnAnUndecodableResponse reaches the fifth. A 200 whose
// body is not the decision shape is what a misrouted request or an HTML error
// page from a proxy looks like.
func TestPrehookFailsClosedOnAnUndecodableResponse(t *testing.T) {
	srv, _ := recordingPrehook(t, func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, "<html>not the prehook</html>"); err != nil {
			t.Errorf("fixture is malformed: write body: %v", err)
		}
	})

	approved, reason := gateWithContractArgs(srv.URL)
	if approved {
		t.Fatalf("an undecodable 200 must fail closed; gateViaPrehook approved")
	}
	if !strings.Contains(reason, "decode") {
		t.Errorf("the denial reason must name the decode failure; got %q", reason)
	}
}
