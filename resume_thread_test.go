package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func resumeTestState(t *testing.T) *State {
	t.Helper()
	st, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func insertRollout(t *testing.T, st *State, row RolloutRow) {
	t.Helper()
	if err := st.UpsertSession(row.BridgeSessionID, row.HarnessSessionID); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertRollout(row); err != nil {
		t.Fatal(err)
	}
}

// A session the bridge started: state.db files its chain under the bridge id,
// and the latest rollout wins over the thread id bridge-server sent.
func TestThreadToResumePrefersTheLatestRolloutOfTheBridgeSession(t *testing.T) {
	st := resumeTestState(t)
	insertRollout(t, st, RolloutRow{HarnessSessionID: "thread-a", BridgeSessionID: "br_1", Sequence: 0, Kind: "start"})
	insertRollout(t, st, RolloutRow{HarnessSessionID: "thread-b", BridgeSessionID: "br_1", Sequence: 1, ParentHarnessID: "thread-a", Kind: "resume"})
	got, err := threadToResume(st, "br_1", StartParams{HarnessSessionID: "thread-a"})
	if err != nil || got != "thread-b" {
		t.Fatalf("threadToResume = %q, %v; want thread-b", got, err)
	}
}

// A session discovered on disk: state.db filed its chain under the thread id,
// bridge-server knows it by a br_ id, and sends the thread id as
// harness_session_id. Before 2026-10-28 this resumed with no thread id.
func TestThreadToResumeFindsTheChainOfADiscoveredSessionByTheThreadIDTheServerSent(t *testing.T) {
	st := resumeTestState(t)
	insertRollout(t, st, RolloutRow{HarnessSessionID: "thread-x", BridgeSessionID: "thread-x", Sequence: 0, Kind: "start"})
	insertRollout(t, st, RolloutRow{HarnessSessionID: "thread-y", BridgeSessionID: "thread-x", Sequence: 1, ParentHarnessID: "thread-x", Kind: "resume"})
	got, err := threadToResume(st, "br_discovered", StartParams{BridgeSessionID: "br_discovered", HarnessSessionID: "thread-x"})
	if err != nil || got != "thread-y" {
		t.Fatalf("threadToResume = %q, %v; want thread-y", got, err)
	}
}

// No chain anywhere (state.db never saw the thread): the sent thread id is it.
func TestThreadToResumeUsesTheSentThreadIDWhenStateHasNoChain(t *testing.T) {
	st := resumeTestState(t)
	got, err := threadToResume(st, "br_2", StartParams{HarnessSessionID: "thread-only-on-the-server"})
	if err != nil || got != "thread-only-on-the-server" {
		t.Fatalf("threadToResume = %q, %v", got, err)
	}
}

// Nothing to resume is an error that says so, never an empty thread id that
// codex answers with "missing field threadId".
func TestThreadToResumeRefusesWhenThereIsNoThreadAtAll(t *testing.T) {
	st := resumeTestState(t)
	got, err := threadToResume(st, "br_3", StartParams{})
	if err == nil || got != "" || !strings.Contains(err.Error(), "no codex thread to resume") {
		t.Fatalf("threadToResume = %q, %v; want an error naming the missing thread", got, err)
	}
}
