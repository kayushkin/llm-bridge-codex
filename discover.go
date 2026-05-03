package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// discoverSessions returns one msg.StoredSession per bridge_session_id known
// to state.db.
//
// Per the session-identity contract (ARCHITECTURE.md "Session Identity &
// Resumption"), state.db is the source of truth: every session the bridge
// has minted shows up there with its full rollout chain. The on-disk
// `~/.codex/sessions/` tree is only used in two ways:
//
//  1. Cold import — any rollout file whose harness_session_id is NOT yet in
//     state.db.rollouts (legacy data, or sessions started outside the
//     bridge) is imported as a synthetic single-rollout session with
//     bridge_session_id = harness_session_id, sequence=0, kind='start'.
//     Lazy + idempotent: a second discover call sees the rows already
//     present and skips them.
//  2. Metadata extraction — for each emitted session we read the LATEST
//     rollout's on-disk file to populate prompt / turns / cwd / timestamps.
//
// StoredSession.HarnessSessionID is the harness UUID (sessions.current_harness_id)
// — the field bridge-server dedupes on. StoredSession.BridgeSessionID is the
// chain head (sessions.bridge_session_id); for cold-imported sessions it
// equals the harness UUID, for bridge-spawned sessions it's the `br_*` id
// minted by bridge-server.
func discoverSessions() ([]msg.StoredSession, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	sessionsDir := filepath.Join(home, ".codex", "sessions")

	st, err := OpenState(DefaultStatePath())
	if err != nil {
		return nil, err
	}
	defer st.Close()

	if err := coldImportRollouts(st, sessionsDir); err != nil {
		return nil, err
	}

	all, err := st.AllSessions()
	if err != nil {
		return nil, err
	}

	out := make([]msg.StoredSession, 0, len(all))
	for _, sess := range all {
		rollouts, err := st.ListRollouts(sess.BridgeSessionID)
		if err != nil {
			return nil, err
		}
		out = append(out, buildStoredSession(sess, rollouts))
	}
	return out, nil
}

// coldImportRollouts walks sessionsDir and inserts a synthetic session +
// rollout row for every rollout file whose harness_session_id is not already
// in state.db.rollouts. Idempotent: re-running on the same tree produces no
// new rows.
//
// A missing or unreadable sessionsDir is not an error — it just means there
// is nothing to cold-import (fresh install, or no codex CLI history yet).
func coldImportRollouts(st *State, sessionsDir string) error {
	known, err := loadKnownHarnessIDs(st)
	if err != nil {
		return err
	}

	walkErr := filepath.WalkDir(sessionsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Permission errors on subdirs shouldn't kill the whole import;
			// keep walking.
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}

		id, _, ts, _, _ := parseCodexSession(path)
		if id == "" {
			id = extractIDFromFilename(d.Name())
		}
		if id == "" {
			return nil
		}
		if _, ok := known[id]; ok {
			return nil
		}

		info, _ := d.Info()
		created := ts
		if created.IsZero() && info != nil {
			created = info.ModTime()
		}

		// Synthetic chain: bridge_session_id = harness_session_id, single
		// rollout at sequence 0 with kind 'start' and no parent. When the
		// user later resumes via the bridge, recordChain will append a real
		// resume row at sequence 1 and the chain extends naturally.
		if err := st.UpsertSession(id, id); err != nil {
			return err
		}
		if err := st.InsertRollout(RolloutRow{
			HarnessSessionID: id,
			BridgeSessionID:  id,
			RolloutPath:      path,
			Sequence:         0,
			Kind:             "start",
			CreatedAt:        created,
		}); err != nil {
			return err
		}
		known[id] = struct{}{}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
		return walkErr
	}
	return nil
}

// loadKnownHarnessIDs returns the set of harness_session_ids already
// present in state.db.rollouts across all sessions.
func loadKnownHarnessIDs(st *State) (map[string]struct{}, error) {
	known := map[string]struct{}{}
	all, err := st.AllSessions()
	if err != nil {
		return nil, err
	}
	for _, sess := range all {
		rs, err := st.ListRollouts(sess.BridgeSessionID)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			known[r.HarnessSessionID] = struct{}{}
		}
	}
	return known, nil
}

// buildStoredSession projects a (session, rollouts) pair into a
// msg.StoredSession. Metadata (prompt, turns, project cwd, created_at)
// comes from the LATEST rollout's on-disk file when available; if the file
// is missing or rollouts are empty the StoredSession still ships with
// whatever the state.db rows themselves carry.
func buildStoredSession(sess SessionRow, rollouts []RolloutRow) msg.StoredSession {
	out := msg.StoredSession{
		HarnessSessionID: sess.CurrentHarnessID,
		BridgeSessionID:  sess.BridgeSessionID,
		Harness:          msg.HarnessCodex,
		CreatedAt:        sess.CreatedAt,
		UpdatedAt:        sess.UpdatedAt,
	}

	if len(rollouts) == 0 {
		return out
	}

	// rollouts is ordered by sequence ASC (per ListRollouts), so the latest
	// is the last element.
	latest := rollouts[len(rollouts)-1]
	if latest.RolloutPath != "" {
		out.Path = latest.RolloutPath
		if info, err := os.Stat(latest.RolloutPath); err == nil {
			out.UpdatedAt = info.ModTime()
		}
		_, prompt, ts, cwd, turns := parseCodexSession(latest.RolloutPath)
		out.Prompt = prompt
		out.TurnCount = turns
		if cwd != "" {
			out.Project = cwd
		}
		// Prefer the rollout file's session_meta timestamp for created_at —
		// it's the harness's own start time, more meaningful than the
		// state.db row insertion time.
		if !ts.IsZero() {
			out.CreatedAt = ts
		}
	}

	return out
}

// parseCodexSession reads the session_meta line and scans for user input to extract metadata.
func parseCodexSession(path string) (id, prompt string, ts time.Time, cwd string, turns int) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	metaDone := false
	promptDone := false

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(line, &entry) != nil {
			continue
		}

		if !metaDone && entry.Type == "session_meta" {
			metaDone = true
			var meta struct {
				ID        string `json:"id"`
				Timestamp string `json:"timestamp"`
				Cwd       string `json:"cwd"`
			}
			if json.Unmarshal(entry.Payload, &meta) == nil {
				id = meta.ID
				cwd = meta.Cwd
				if meta.Timestamp != "" {
					ts, _ = time.Parse(time.RFC3339Nano, meta.Timestamp)
				}
			}
		}

		// Find user input for prompt snippet (skip environment_context preamble).
		if !promptDone && entry.Type == "response_item" {
			var item struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(entry.Payload, &item) == nil && item.Role == "user" {
				for _, c := range item.Content {
					if c.Type == "input_text" && c.Text != "" && !strings.HasPrefix(c.Text, "<environment_context>") {
						promptDone = true
						prompt = truncateStr(c.Text, 200)
						break
					}
				}
			}
		}

		// Count user turns.
		if entry.Type == "response_item" {
			var item struct {
				Role string `json:"role"`
			}
			if json.Unmarshal(entry.Payload, &item) == nil && item.Role == "user" {
				turns++
			}
		}
	}

	return
}

// extractIDFromFilename extracts the UUID from a Codex session filename.
// Format: rollout-2026-04-13T02-00-13-019d8491-627f-79e1-89a7-d32ed21ee93e.jsonl
func extractIDFromFilename(name string) string {
	name = strings.TrimSuffix(name, ".jsonl")
	// The UUID is the last 36 characters (8-4-4-4-12 with dashes)
	if len(name) >= 36 {
		candidate := name[len(name)-36:]
		// Rough UUID check: contains 4 dashes at positions 8, 13, 18, 23
		if len(candidate) == 36 &&
			candidate[8] == '-' && candidate[13] == '-' &&
			candidate[18] == '-' && candidate[23] == '-' {
			return candidate
		}
	}
	return name
}

// findRolloutForThread does a best-effort scan of ~/.codex/sessions/ for a
// rollout file whose name carries the given thread_id. Returns "" if not
// found — caller treats that as "rollout file not yet on disk" and proceeds
// without the path. The path can be backfilled later by re-globbing.
func findRolloutForThread(threadID string) string {
	if threadID == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	sessionsDir := filepath.Join(home, ".codex", "sessions")
	var found string
	_ = filepath.WalkDir(sessionsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".jsonl") && strings.Contains(d.Name(), threadID) {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// truncateStr truncates a string to max bytes.
func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
