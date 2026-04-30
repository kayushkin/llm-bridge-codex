package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// discoverSessions scans Codex's on-disk session storage and returns all found sessions.
// Sessions are stored at ~/.codex/sessions/<year>/<month>/<day>/rollout-<timestamp>-<uuid>.jsonl
//
// Rollouts that the bridge has already chained in state.db (i.e. they're a
// resume/fork continuation of a known bridge_session_id, not the originating
// rollout) are skipped — they belong to their parent ManagedSession on the
// bridge-server side and should not appear as standalone entries. Cold
// rollouts that aren't in state.db pass through unchanged so existing
// pre-fix data still surfaces.
func discoverSessions() ([]msg.StoredSession, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	sessionsDir := filepath.Join(home, ".codex", "sessions")

	// Build the set of harness_session_ids that state.db has chained as
	// non-originating rollouts (sequence > 0). These are stub continuations
	// and should be hidden from the discover output. If state.db can't be
	// opened for any reason we proceed without filtering — fail-safe toward
	// surfacing rollouts rather than losing them.
	chained := map[string]string{} // thread_id → bridge_session_id (sequence > 0 only)
	if st, err := OpenState(DefaultStatePath()); err == nil {
		defer st.Close()
		if all, err := st.AllSessions(); err == nil {
			for _, s := range all {
				if rs, err := st.ListRollouts(s.BridgeSessionID); err == nil {
					for _, r := range rs {
						if r.Sequence > 0 {
							chained[r.HarnessSessionID] = s.BridgeSessionID
						}
					}
				}
			}
		}
	}

	var sessions []msg.StoredSession

	err = filepath.WalkDir(sessionsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		sess := msg.StoredSession{
			Harness:   msg.HarnessCodex,
			UpdatedAt: info.ModTime(),
			Path:      path,
		}

		// Extract session ID and metadata from file.
		id, prompt, ts, cwd, turns := parseCodexSession(path)
		sess.ID = id
		sess.Prompt = prompt
		sess.TurnCount = turns
		if cwd != "" {
			sess.Project = cwd
		}
		if !ts.IsZero() {
			sess.CreatedAt = ts
		} else {
			sess.CreatedAt = info.ModTime()
		}

		// Fall back: extract ID from filename if not in content.
		// Format: rollout-<timestamp>-<uuid>.jsonl
		if sess.ID == "" {
			sess.ID = extractIDFromFilename(d.Name())
		}

		// Skip rollouts that state.db knows are non-originating continuations
		// (resume/fork sequence > 0). They belong to their parent
		// bridge_session_id and shouldn't appear as standalone discovered
		// sessions — that's how the stub-rollout problem stops surfacing.
		if _, isChain := chained[sess.ID]; isChain {
			return nil
		}

		sessions = append(sessions, sess)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return sessions, nil
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
			Type    string `json:"type"`
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

		// Stop scanning after we have everything we need and a reasonable sample.
		if metaDone && promptDone && turns > 0 {
			// Continue counting turns but skip heavy parsing.
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
