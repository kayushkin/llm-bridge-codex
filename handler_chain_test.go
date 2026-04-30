package main

import (
	"errors"
	"path/filepath"
	"testing"
)

// newTestBridge returns a Bridge with only the fields recordChain and the
// orphan-recovery path touch — no AppServer, Codex, or Translator. The
// underlying state.db lives in a temp dir so each test is isolated. HOME is
// also pointed at the temp dir so findRolloutForThread (called inside
// recordChain) cannot stray into the host's real ~/.codex/sessions tree.
func newTestBridge(t *testing.T, bridgeID string) *Bridge {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	st, err := OpenState(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.UpsertSession(bridgeID, ""); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	return &Bridge{state: st, bridgeID: bridgeID}
}

func TestRecordChain_StartHappyPath(t *testing.T) {
	b := newTestBridge(t, "bsid-1")

	got, err := b.recordChain("start", "", 0, func() (string, error) {
		return "thr-a", nil
	})
	if err != nil {
		t.Fatalf("recordChain: %v", err)
	}
	if got != "thr-a" {
		t.Fatalf("returned id = %q, want thr-a", got)
	}

	pending, err := b.state.ListPendingWAL()
	if err != nil {
		t.Fatalf("ListPendingWAL: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("WAL must be committed: %d pending rows remain", len(pending))
	}

	rs, err := b.state.ListRollouts("bsid-1")
	if err != nil {
		t.Fatalf("ListRollouts: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("want 1 rollout, got %d (%+v)", len(rs), rs)
	}
	if rs[0].HarnessSessionID != "thr-a" || rs[0].Sequence != 0 || rs[0].Kind != "start" || rs[0].ParentHarnessID != "" {
		t.Fatalf("rollout shape: %+v", rs[0])
	}

	row, err := b.state.GetSession("bsid-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if row.CurrentHarnessID != "thr-a" {
		t.Fatalf("session current_harness_id = %q, want thr-a", row.CurrentHarnessID)
	}
}

func TestRecordChain_ResumeHappyPath(t *testing.T) {
	b := newTestBridge(t, "bsid-1")

	if _, err := b.recordChain("start", "", 0, func() (string, error) { return "thr-a", nil }); err != nil {
		t.Fatalf("seed start: %v", err)
	}

	got, err := b.recordChain("resume", "thr-a", 1, func() (string, error) {
		return "thr-b", nil
	})
	if err != nil {
		t.Fatalf("recordChain resume: %v", err)
	}
	if got != "thr-b" {
		t.Fatalf("returned id = %q, want thr-b", got)
	}

	rs, err := b.state.ListRollouts("bsid-1")
	if err != nil {
		t.Fatalf("ListRollouts: %v", err)
	}
	if len(rs) != 2 {
		t.Fatalf("want 2 rollouts, got %d (%+v)", len(rs), rs)
	}
	if rs[1].Kind != "resume" || rs[1].Sequence != 1 || rs[1].ParentHarnessID != "thr-a" || rs[1].HarnessSessionID != "thr-b" {
		t.Fatalf("resume rollout shape: %+v", rs[1])
	}

	row, err := b.state.GetSession("bsid-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if row.CurrentHarnessID != "thr-b" {
		t.Fatalf("current_harness_id should rotate to thr-b, got %q", row.CurrentHarnessID)
	}
}

func TestRecordChain_ForkHappyPath(t *testing.T) {
	b := newTestBridge(t, "bsid-2")

	got, err := b.recordChain("fork", "thr-parent", 0, func() (string, error) {
		return "thr-child", nil
	})
	if err != nil {
		t.Fatalf("recordChain fork: %v", err)
	}
	if got != "thr-child" {
		t.Fatalf("returned id = %q, want thr-child", got)
	}

	rs, err := b.state.ListRollouts("bsid-2")
	if err != nil {
		t.Fatalf("ListRollouts: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("want 1 rollout, got %d", len(rs))
	}
	if rs[0].Kind != "fork" || rs[0].ParentHarnessID != "thr-parent" || rs[0].HarnessSessionID != "thr-child" {
		t.Fatalf("fork rollout shape: %+v", rs[0])
	}
}

func TestRecordChain_MintFailureOrphansWAL(t *testing.T) {
	b := newTestBridge(t, "bsid-1")

	// Seed a successful start so we can confirm a later mint failure does
	// NOT rotate current_harness_id away from the last good value.
	if _, err := b.recordChain("start", "", 0, func() (string, error) { return "thr-a", nil }); err != nil {
		t.Fatalf("seed start: %v", err)
	}

	mintErr := errors.New("simulated thread/start failure")
	if _, err := b.recordChain("resume", "thr-a", 1, func() (string, error) { return "", mintErr }); err == nil {
		t.Fatalf("expected error from failing mint")
	} else if !errors.Is(err, mintErr) {
		t.Fatalf("error must propagate: got %v", err)
	}

	pending, err := b.state.ListPendingWAL()
	if err != nil {
		t.Fatalf("ListPendingWAL: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("failed mint must orphan its WAL row: %d still pending", len(pending))
	}

	rs, err := b.state.ListRollouts("bsid-1")
	if err != nil {
		t.Fatalf("ListRollouts: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("failed mint must NOT insert a rollout: got %d", len(rs))
	}

	row, err := b.state.GetSession("bsid-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if row.CurrentHarnessID != "thr-a" {
		t.Fatalf("session must not rotate on failure: got %q", row.CurrentHarnessID)
	}
}

func TestRecoverOrphansOnBoot(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenState(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	defer st.Close()

	if err := st.UpsertSession("bsid-1", ""); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	// Two pending WAL rows, simulating a crash between InsertWAL and the
	// codex thread call returning.
	if _, err := st.InsertWAL(WALRow{BridgeSessionID: "bsid-1", Intent: "start"}); err != nil {
		t.Fatalf("seed pending start: %v", err)
	}
	if _, err := st.InsertWAL(WALRow{BridgeSessionID: "bsid-1", Intent: "resume", ParentHarnessID: "thr-x"}); err != nil {
		t.Fatalf("seed pending resume: %v", err)
	}

	if pending, err := st.ListPendingWAL(); err != nil {
		t.Fatalf("ListPendingWAL pre-recover: %v", err)
	} else if len(pending) != 2 {
		t.Fatalf("pre-recover expected 2 pending, got %d", len(pending))
	}

	if err := recoverOrphansOnBoot(st); err != nil {
		t.Fatalf("recoverOrphansOnBoot: %v", err)
	}

	if pending, err := st.ListPendingWAL(); err != nil {
		t.Fatalf("ListPendingWAL post-recover: %v", err)
	} else if len(pending) != 0 {
		t.Fatalf("post-recover expected 0 pending, got %d (%+v)", len(pending), pending)
	}

	// Idempotent: second call on a clean store must succeed and remain a no-op.
	if err := recoverOrphansOnBoot(st); err != nil {
		t.Fatalf("recoverOrphansOnBoot second call: %v", err)
	}
	if pending, err := st.ListPendingWAL(); err != nil {
		t.Fatalf("ListPendingWAL after second call: %v", err)
	} else if len(pending) != 0 {
		t.Fatalf("idempotent recovery left %d pending", len(pending))
	}
}
