package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// gateViaPrehook had no test of any kind until this file. Not a suite that
// failed to separate its two budgets — no suite at all: the only reference to
// "prehook" under any _test.go was a comment in truncate_test.go, and the call
// graph agrees (gateViaPrehook <- gateApproval <- five *ApprovalRequest
// handlers, none of them named by a test).
//
// That matters because the helper those budgets are handed to, translate.go's
// truncateAtRuneBoundaryWithEllipsis, scores 15/15 by sabotage. A helper can be
// perfect while every number its callers pass is free to drift, and the two
// numbers here are the ones that ship:
//
//	prehook_proxy.go   truncateAtRuneBoundaryWithEllipsis(string(respBody), 200)
//	prehook_proxy.go   truncateAtRuneBoundaryWithEllipsis(reason, 80)
//
// Measured before these tests existed: all four drift rows (200->199, 200->201,
// 80->79, 80->81) read UNNOTICED, as did dropping either cut entirely.
//
// Both budgets are log lines rather than stored data, which is why this cut
// outlived the one in discover.go and why the card filing it labelled itself
// lower-tier. A drifted budget here costs a slightly shorter log message and
// nothing more.
const (
	lastByteInsideBudget        = "~"
	firstByteOutsideBudget      = "^"
	shippedErrorBodyBudget      = 200
	shippedDecisionReasonBudget = 80
)

// straddleBudget builds a string long enough to be cut, whose byte at
// budget-1 is the last one inside the budget and whose byte at budget is the
// first one outside it, each marked with a distinct ASCII character.
//
// The markers are ASCII on purpose: these tests are about the two numbers, and
// must not depend on the rune mechanism truncate_test.go exercises.
func straddleBudget(t *testing.T, budget int) string {
	t.Helper()
	s := strings.Repeat("a", budget-1) + lastByteInsideBudget + firstByteOutsideBudget +
		strings.Repeat("b", 50)
	// Fixture guard, not an assertion: if the markers are not where the rest of
	// this file believes they are, every verdict below is meaningless.
	if s[budget-1:budget] != lastByteInsideBudget || s[budget:budget+1] != firstByteOutsideBudget {
		t.Fatalf("fixture is malformed: markers are not at the budget boundary")
	}
	if strings.Count(s, lastByteInsideBudget) != 1 || strings.Count(s, firstByteOutsideBudget) != 1 {
		t.Fatalf("fixture is malformed: markers must each appear exactly once")
	}
	return s
}

// captureLog runs fn with the standard logger redirected, and returns what it
// wrote. Timestamps are switched off so the caller can match on the line's own
// text. These tests therefore cannot run in parallel with anything that logs.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()
	fn()
	return buf.String()
}

// loggedPayload lifts the truncated text out of one prehook-proxy log line,
// rather than searching the whole capture for a marker. The difference matters:
// a marker test against the whole line silently depends on the line's fixed
// text — the tool name, the decision, the word "HTTP" — never containing the
// marker byte, and nothing would report it if that changed. Failing to find the
// line is a fixture guard, so the sabotage scorer does not count it as
// detection.
func loggedPayload(t *testing.T, out, prefix, suffix string) string {
	t.Helper()
	i := strings.Index(out, prefix)
	if i < 0 {
		t.Fatalf("prehook log line not found: no %q in captured log %q", prefix, out)
	}
	rest := strings.TrimRight(out[i+len(prefix):], "\n")
	if suffix != "" {
		j := strings.LastIndex(rest, suffix)
		if j < 0 {
			t.Fatalf("prehook log line not found: no closing %q in %q", suffix, rest)
		}
		rest = rest[:j]
	}
	return rest
}

// assertStraddled judges a logged payload against the boundary its fixture was
// built around, without naming the budget again. A length assertion would have
// to spell the number under test, and a test that spells the constant it is
// meant to pin moves with it.
func assertStraddled(t *testing.T, what, logged string) {
	t.Helper()
	if !strings.Contains(logged, lastByteInsideBudget) {
		t.Fatalf("%s dropped the last byte inside the budget: the shipped budget is smaller than it was: %q",
			what, logged)
	}
	if strings.Contains(logged, firstByteOutsideBudget) {
		t.Fatalf("%s kept the first byte outside the budget: the shipped budget is larger than it was, or the cut was dropped: %q",
			what, logged)
	}
}

// assertShippedRequest checks that the call arrived where production sends it.
//
// An httptest stub serves every path alike, so a fixture that only reads the
// body cannot fail on a wrong URL — the fleet-wide shape card 64766783 sweeps
// for. These tests are about two budgets and do not score the request contract,
// but a fixture blind to it is worse than one that is not, and the assertion is
// two lines. What is NOT yet measured here is whether anything would catch the
// path drifting: sabotage-truncation.py scores the cut, and stays about the cut.
func assertShippedRequest(t *testing.T, r *http.Request, bridgeID string) {
	t.Helper()
	if r.Method != http.MethodPost {
		t.Errorf("prehook must be POSTed; got %s", r.Method)
	}
	if want := "/permission/codex-prehook/" + bridgeID; r.URL.Path != want {
		t.Errorf("prehook posted to %q, want %q", r.URL.Path, want)
	}
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("prehook Content-Type is %q, want application/json", got)
	}
}

// writeDecision writes the response shape gateViaPrehook decodes — the same
// fields ccPrehookPayload's handler returns on the server side.
func writeDecision(w http.ResponseWriter, decision, reason string) error {
	return json.NewEncoder(w).Encode(map[string]any{
		"hookSpecificOutput": map[string]string{
			"permissionDecision":       decision,
			"permissionDecisionReason": reason,
		},
	})
}

// TestPrehookErrorBodyLogKeepsExactlyTheShippedBudget pins the 200 that
// prehook_proxy.go hands the ellipsis helper for a non-2xx response body.
func TestPrehookErrorBodyLogKeepsExactlyTheShippedBudget(t *testing.T) {
	body := straddleBudget(t, shippedErrorBodyBudget)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertShippedRequest(t, r, "bridge-1")
		w.WriteHeader(http.StatusInternalServerError)
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("write response body: %v", err)
		}
	}))
	defer srv.Close()

	var approved bool
	out := captureLog(t, func() {
		approved, _ = gateViaPrehook(context.Background(), srv.URL, "bridge-1", "unified_exec", nil, "call-1")
	})

	// The gate is fail-closed, and a test that drove the error path without
	// checking that would pass just as well against a version that allowed.
	if approved {
		t.Fatalf("a 500 from the prehook must fail closed; gateViaPrehook approved")
	}
	assertStraddled(t, "the logged error body",
		loggedPayload(t, out, "[prehook-proxy] HTTP 500: ", ""))
}

// TestPrehookDecisionReasonLogKeepsExactlyTheShippedBudget pins the 80 that
// prehook_proxy.go hands the ellipsis helper for a decision reason. The reason
// is model-written text on the wire, so this is the budget of the two more
// likely to meet a multi-byte rune in production.
func TestPrehookDecisionReasonLogKeepsExactlyTheShippedBudget(t *testing.T) {
	reason := straddleBudget(t, shippedDecisionReasonBudget)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertShippedRequest(t, r, "bridge-1")
		w.Header().Set("Content-Type", "application/json")
		if err := writeDecision(w, "deny", reason); err != nil {
			t.Errorf("encode decision: %v", err)
		}
	}))
	defer srv.Close()

	var approved bool
	var gotReason string
	out := captureLog(t, func() {
		approved, gotReason = gateViaPrehook(context.Background(), srv.URL, "bridge-1", "unified_exec", nil, "call-1")
	})

	if approved {
		t.Fatalf(`permissionDecision "deny" must not approve`)
	}
	// The RETURNED reason is not truncated — only the log line is. Asserting
	// this keeps the two apart: a cut that leaked from the log into the return
	// value would change what codex is told, which is stored data, not a log.
	if gotReason != reason {
		t.Fatalf("the returned reason must be the full one, not the logged cut: got %q", gotReason)
	}
	assertStraddled(t, "the logged decision reason",
		loggedPayload(t, out, "[prehook-proxy] unified_exec → deny (", ")"))
}
