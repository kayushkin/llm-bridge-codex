package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// TestDiscoverColdImportAndIdempotent covers the cold-rollout discover
// contract from the session-chain spec:
//
//   - 2 sessions × 3-rollout chains pre-seeded into state.db (those rollouts
//     also have on-disk files so the walker can see them).
//   - 5 additional rollout files on disk that state.db has never seen.
//
// First discover call must:
//   - emit 7 StoredSessions (2 pre-seeded + 5 cold-imported)
//   - cold-import the 5 unknown rollouts as synthetic single-rollout
//     sessions (bridge_session_id = harness_session_id, sequence 0,
//     kind 'start')
//   - leave the 6 already-chained rollouts untouched
//
// Second call must:
//   - emit 7 again (no duplicates, no drift)
//   - not insert any new rollout rows
//
// Each StoredSession surfaces both BridgeSessionID (chain head) and
// HarnessSessionID (current_harness_id). For pre-seeded bridge-spawned
// chains the two differ; for cold-imports the synthetic chain sets them
// equal. Bridge-server dedupes on HarnessSessionID, so the rotating chain
// rows past sequence 0 must NOT leak through as either field.
func TestDiscoverColdImportAndIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sessionsDir := filepath.Join(home, ".codex", "sessions", "2026", "04", "30")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("mkdir sessionsDir: %v", err)
	}

	// Pre-seed: 2 sessions × 3-rollout chains, with on-disk rollout files
	// for every chain row so the walker would find them and (correctly) skip
	// them as already-known.
	type rolloutSeed struct {
		bsid, hsid string
		seq        int
		kind       string
		parent     string
	}
	seedChains := []rolloutSeed{
		{"bsid-A", "hsid-A0", 0, "start", ""},
		{"bsid-A", "hsid-A1", 1, "resume", "hsid-A0"},
		{"bsid-A", "hsid-A2", 2, "resume", "hsid-A1"},
		{"bsid-B", "hsid-B0", 0, "start", ""},
		{"bsid-B", "hsid-B1", 1, "resume", "hsid-B0"},
		{"bsid-B", "hsid-B2", 2, "fork", "hsid-B1"},
	}

	seedSt, err := OpenState(DefaultStatePath())
	if err != nil {
		t.Fatalf("OpenState seed: %v", err)
	}
	for _, b := range []string{"bsid-A", "bsid-B"} {
		if err := seedSt.UpsertSession(b, ""); err != nil {
			t.Fatalf("UpsertSession %s: %v", b, err)
		}
	}
	for _, r := range seedChains {
		path := writeRolloutFile(t, sessionsDir, r.hsid)
		if err := seedSt.InsertRollout(RolloutRow{
			HarnessSessionID: r.hsid,
			BridgeSessionID:  r.bsid,
			RolloutPath:      path,
			Sequence:         r.seq,
			ParentHarnessID:  r.parent,
			Kind:             r.kind,
		}); err != nil {
			t.Fatalf("InsertRollout %s: %v", r.hsid, err)
		}
	}
	// Mirror production: handler rotates current_harness_id to the latest
	// chain head after each successful turn (see handler.go:237). Without
	// this, discover would emit HarnessSessionID="" for the seeded chains.
	if err := seedSt.UpsertSession("bsid-A", "hsid-A2"); err != nil {
		t.Fatalf("rotate bsid-A current_harness_id: %v", err)
	}
	if err := seedSt.UpsertSession("bsid-B", "hsid-B2"); err != nil {
		t.Fatalf("rotate bsid-B current_harness_id: %v", err)
	}
	if err := seedSt.Close(); err != nil {
		t.Fatalf("seedSt close: %v", err)
	}

	// 5 untracked rollouts on disk — state.db has never seen these.
	coldIDs := []string{"hsid-cold-1", "hsid-cold-2", "hsid-cold-3", "hsid-cold-4", "hsid-cold-5"}
	for _, hid := range coldIDs {
		writeRolloutFile(t, sessionsDir, hid)
	}

	// First call: cold-imports the 5 unknowns and emits 7 sessions total.
	sessions, err := discoverSessions()
	if err != nil {
		t.Fatalf("discoverSessions 1st call: %v", err)
	}
	if len(sessions) != 7 {
		t.Fatalf("want 7 StoredSessions on first call, got %d", len(sessions))
	}

	// state.db now: 7 sessions, 11 rollouts (6 seeded + 5 cold).
	if got := countSessionsAndRollouts(t); got != [2]int{7, 11} {
		t.Fatalf("after 1st call: sessions/rollouts = %v, want [7 11]", got)
	}

	// Second call: idempotent. Same 7 sessions, no new rollout rows.
	sessions2, err := discoverSessions()
	if err != nil {
		t.Fatalf("discoverSessions 2nd call: %v", err)
	}
	if len(sessions2) != 7 {
		t.Fatalf("want 7 StoredSessions on idempotent call, got %d", len(sessions2))
	}
	if got := countSessionsAndRollouts(t); got != [2]int{7, 11} {
		t.Fatalf("after 2nd call: sessions/rollouts = %v, want [7 11] (idempotency broken)", got)
	}

	// Index emitted sessions by BridgeSessionID and assert per-row contract.
	byBridge := map[string]msg.StoredSession{}
	for _, s := range sessions {
		if s.BridgeSessionID == "" {
			t.Errorf("StoredSession with empty BridgeSessionID: %+v", s)
			continue
		}
		byBridge[s.BridgeSessionID] = s
	}

	// Pre-seeded chains: BridgeSessionID is the chain head; HarnessSessionID
	// is the latest rotated chain row (current_harness_id).
	wantPreseed := map[string]string{"bsid-A": "hsid-A2", "bsid-B": "hsid-B2"}
	for bsid, wantHsid := range wantPreseed {
		s, ok := byBridge[bsid]
		if !ok {
			t.Errorf("missing pre-seeded BridgeSessionID %q in discover output", bsid)
			continue
		}
		if s.HarnessSessionID != wantHsid {
			t.Errorf("pre-seeded %s: HarnessSessionID = %q, want %q", bsid, s.HarnessSessionID, wantHsid)
		}
	}

	// Cold-imported chains: synthetic chain sets BridgeSessionID == HarnessSessionID.
	for _, hid := range coldIDs {
		s, ok := byBridge[hid]
		if !ok {
			t.Errorf("missing cold-imported BridgeSessionID %q in discover output", hid)
			continue
		}
		if s.HarnessSessionID != hid {
			t.Errorf("cold-imported %s: HarnessSessionID = %q, want %q", hid, s.HarnessSessionID, hid)
		}
	}

	// Intermediate chain rows must NOT surface as either id field.
	for _, hid := range []string{"hsid-A0", "hsid-A1", "hsid-B0", "hsid-B1"} {
		if _, leaked := byBridge[hid]; leaked {
			t.Errorf("intermediate chain harness id %q leaked as a BridgeSessionID", hid)
		}
		for _, s := range sessions {
			if s.HarnessSessionID == hid {
				t.Errorf("intermediate chain harness id %q leaked as a HarnessSessionID", hid)
			}
		}
	}
}

// TestDiscoverNoSessionsDir covers the fresh-install case: ~/.codex/sessions
// does not exist yet. discover should still succeed (no cold-import work,
// no sessions emitted) without failing the open of state.db.
func TestDiscoverNoSessionsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Note: we never create ~/.codex/sessions in this test.

	sessions, err := discoverSessions()
	if err != nil {
		t.Fatalf("discoverSessions on missing sessions dir: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("want 0 sessions on fresh install, got %d", len(sessions))
	}
}

// TestBuildStoredSession_ConformanceSourceTag covers the structural source
// tagging that lets bridge-server file conformance-leaked sessions under
// the Conformance folder without depending on a populated rollout file.
// The conformance suite pins bridge_session_id to "conformance-<feature>"
// (see llm-bridge-server/conformance/runner.go), so the prefix is the
// authoritative signal — bridge-server's prompt-prefix heuristic was
// missing every leak whose rollout_path was empty (the codex CLI flushes
// session_meta asynchronously, so half of state.db.rollouts had no path).
func TestBuildStoredSession_ConformanceSourceTag(t *testing.T) {
	cases := []struct {
		name            string
		bridgeSessionID string
		wantSource      string
	}{
		{"conformance leak", "conformance-error", "conformance"},
		{"conformance other feature", "conformance-context", "conformance"},
		{"bridge-spawned chain", "bsid-A", ""},
		{"cold-import (synthetic)", "019d7f71-0e3e-7bb3-9cb3-b7e620e5810e", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildStoredSession(SessionRow{
				BridgeSessionID:  tc.bridgeSessionID,
				CurrentHarnessID: "hsid-X",
			}, nil)
			if got.Source != tc.wantSource {
				t.Errorf("Source = %q, want %q", got.Source, tc.wantSource)
			}
		})
	}
}

// writeRolloutFile drops a minimal-but-valid Codex rollout JSONL file in
// dir, named after harnessID. Includes a session_meta line plus one
// user response_item so parseCodexSession can extract id/cwd/turns/prompt.
func writeRolloutFile(t *testing.T, dir, harnessID string) string {
	t.Helper()
	name := fmt.Sprintf("rollout-%s.jsonl", harnessID)
	path := filepath.Join(dir, name)
	body := strings.Join([]string{
		fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"timestamp":"2026-04-30T00:00:00Z","cwd":"/tmp/test"}}`, harnessID),
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"hello"}]}}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// countSessionsAndRollouts opens state.db at the test's HOME and reports
// [#sessions, #rollouts]. Test helper only; opens its own short-lived
// connection.
func countSessionsAndRollouts(t *testing.T) [2]int {
	t.Helper()
	st, err := OpenState(DefaultStatePath())
	if err != nil {
		t.Fatalf("OpenState verify: %v", err)
	}
	defer st.Close()
	all, err := st.AllSessions()
	if err != nil {
		t.Fatalf("AllSessions: %v", err)
	}
	var rollouts int
	for _, s := range all {
		rs, err := st.ListRollouts(s.BridgeSessionID)
		if err != nil {
			t.Fatalf("ListRollouts %s: %v", s.BridgeSessionID, err)
		}
		rollouts += len(rs)
	}
	return [2]int{len(all), rollouts}
}
