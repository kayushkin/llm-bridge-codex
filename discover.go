package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

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
// codexHomeDirectory is codex's own home: $CODEX_HOME, or ~/.codex when it is
// unset, as codex itself defaults it. Rollouts, archived rollouts and
// generated images all live under it.
func codexHomeDirectory() (string, error) {
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		return codexHome, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the codex home: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

// codexRolloutDirectories are the trees codex writes rollout files into:
// sessions/ for live conversations and archived_sessions/ for the ones a
// person archived. Discovery reads both, so an archived conversation is still
// a session the dash knows. Until 2026-10-28 only ~/.codex/sessions was read,
// whatever CODEX_HOME said.
func codexRolloutDirectories() ([]string, error) {
	codexHome, err := codexHomeDirectory()
	if err != nil {
		return nil, err
	}
	return []string{
		filepath.Join(codexHome, "sessions"),
		filepath.Join(codexHome, "archived_sessions"),
	}, nil
}

func discoverSessions() ([]msg.StoredSession, error) {
	rolloutDirectories, err := codexRolloutDirectories()
	if err != nil {
		return nil, err
	}

	st, err := OpenState(DefaultStatePath())
	if err != nil {
		return nil, err
	}
	defer st.Close()

	for _, rolloutDirectory := range rolloutDirectories {
		if err := coldImportRollouts(st, rolloutDirectory); err != nil {
			return nil, err
		}
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
		if err := coldImportRollout(st, id, path, ts, info); err != nil {
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

// coldImportRollout records one rollout this harness has no row for.
//
// Synthetic chain: bridge_session_id = harness_session_id, single rollout at
// sequence 0 with kind 'start' and no parent. When the user later resumes via
// the bridge, recordChain will append a real resume row at sequence 1 and the
// chain extends naturally.
func coldImportRollout(st *State, id, path string, created time.Time, info fs.FileInfo) error {
	if created.IsZero() && info != nil {
		created = info.ModTime()
	}
	if err := st.UpsertSession(id, id); err != nil {
		return err
	}
	return st.InsertRollout(RolloutRow{
		HarnessSessionID: id,
		BridgeSessionID:  id,
		RolloutPath:      path,
		Sequence:         0,
		Kind:             "start",
		CreatedAt:        created,
	})
}

// discoverSessionsChangedSince is -changed-since: the sessions whose rollout
// was written after since, in -discover's shape, so llm-bridge-server can
// notice a conversation run in a terminal while it is up. It stats every
// rollout and reads only the changed ones.
func discoverSessionsChangedSince(since time.Time) ([]msg.StoredSession, error) {
	rolloutDirectories, err := codexRolloutDirectories()
	if err != nil {
		return nil, err
	}
	changed := map[string]string{} // thread id → rollout path
	for _, rolloutDirectory := range rolloutDirectories {
		if err := collectRolloutsChangedSince(rolloutDirectory, since, changed); err != nil {
			return nil, err
		}
	}
	out := make([]msg.StoredSession, 0, len(changed))
	if len(changed) == 0 {
		return out, nil
	}

	st, err := OpenState(DefaultStatePath())
	if err != nil {
		return nil, err
	}
	defer st.Close()
	for id, path := range changed {
		bridgeSessionID, known, err := st.BridgeSessionIDForHarness(id)
		if err != nil {
			return nil, err
		}
		if !known {
			_, _, ts, _, _ := parseCodexSession(path)
			info, _ := os.Stat(path)
			if err := coldImportRollout(st, id, path, ts, info); err != nil {
				return nil, err
			}
			bridgeSessionID = id
		}
		sess, err := st.GetSession(bridgeSessionID)
		if err != nil {
			return nil, err
		}
		if sess == nil {
			return nil, fmt.Errorf("rollout %s names bridge session %s, which state.db has no row for", id, bridgeSessionID)
		}
		rollouts, err := st.ListRollouts(bridgeSessionID)
		if err != nil {
			return nil, err
		}
		stored := buildStoredSession(*sess, rollouts)
		// The file that changed names the conversation, and the server
		// matches on it. The state row's current id can be empty, as it is
		// for a session the bridge started that has not reported one.
		stored.HarnessSessionID = id
		out = append(out, stored)
	}
	return out, nil
}

// collectRolloutsChangedSince adds to changed every rollout under
// rolloutDirectory written after since, keyed by thread id. A missing
// directory adds nothing: archived_sessions does not exist until a person
// archives a conversation.
func collectRolloutsChangedSince(rolloutDirectory string, since time.Time, changed map[string]string) error {
	walkErr := filepath.WalkDir(rolloutDirectory, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.ModTime().After(since) {
			return nil
		}
		id := extractIDFromFilename(d.Name())
		if id == "" {
			id, _, _, _, _ = parseCodexSession(path)
		}
		if id != "" {
			changed[id] = path
		}
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
		Harness:          harness,
		CreatedAt:        sess.CreatedAt,
		UpdatedAt:        sess.UpdatedAt,
		Source:           sourceFromBridgeSessionID(sess.BridgeSessionID),
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

// sourceFromBridgeSessionID classifies a session by its bridge_session_id.
// The conformance suite spawns the harness with stable session_ids of the
// form "conformance-<feature>" (see llm-bridge-server/conformance/runner.go),
// so any chain whose bridge_session_id carries that prefix is a leaked test
// session. Returning the structural tag here lets bridge-server file these
// rows under the Conformance folder without having to extract a prompt from
// the on-disk rollout — which is empty when codex hasn't flushed session_meta
// yet, the source of the original "blank codex sessions" leak.
func sourceFromBridgeSessionID(bridgeSessionID string) string {
	if strings.HasPrefix(bridgeSessionID, "conformance-") {
		return "conformance"
	}
	return ""
}

// parseCodexSession reads the session_meta line and scans for user input to extract metadata.
func parseCodexSession(path string) (id, prompt string, ts time.Time, cwd string, turns int) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// The largest line this accepts is 1024*1024 - 1 bytes, not 1024*1024. A
	// line of exactly the cap fills the buffer with no newline in it, so the
	// token-too-long check fires before the split can succeed. The two spellings
	// are the starting buffer and the cap, not two separate ceilings — the
	// effective one is the larger of the two, and the zero-length starting slice
	// only sets how much is allocated up front. Pinned from both sides by
	// TestParseCodexSessionReadsTheLongestLineItsCeilingAllows.
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	metaDone := false
	var typedPrompt, userItemPrompt string
	var typedTurns, userItemTurns int

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

		// What the person typed. codex records each message twice: as an
		// event_msg of type user_message, which carries exactly what was
		// typed, and as a response_item with role "user", alongside the
		// items codex itself writes with that role — the environment, the
		// AGENTS.md instructions it loaded, a turn-aborted marker. Titling
		// from the first response_item is how 115 of one machine's 130
		// discovered sessions came to be named after their instructions.
		// So user_message wins wherever the rollout has it; a rollout from a
		// codex that writes none falls back to the response_items, skipping
		// codex's own text (msg.IsCodexInjectedText).
		if entry.Type == "event_msg" {
			var event struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}
			if json.Unmarshal(entry.Payload, &event) == nil && event.Type == "user_message" && strings.TrimSpace(event.Message) != "" && !msg.IsCodexInjectedText(event.Message) {
				typedTurns++
				if typedPrompt == "" {
					typedPrompt = truncateAtRuneBoundary(event.Message, 200)
				}
			}
		}
		if entry.Type == "response_item" {
			var item struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(entry.Payload, &item) == nil && item.Role == "user" {
				for _, c := range item.Content {
					if c.Type == "input_text" && strings.TrimSpace(c.Text) != "" && !msg.IsCodexInjectedText(c.Text) {
						userItemTurns++
						if userItemPrompt == "" {
							userItemPrompt = truncateAtRuneBoundary(c.Text, 200)
						}
						break
					}
				}
			}
		}
	}

	if typedTurns > 0 {
		prompt, turns = typedPrompt, typedTurns
	} else {
		prompt, turns = userItemPrompt, userItemTurns
	}

	// bufio.Scanner ends a scan on ErrTooLong exactly as it ends one at EOF —
	// Scan() returns false — so without this read an over-long line is
	// indistinguishable from a clean end of file.
	//
	// codex loses more than claudecode and jig do here. Those two return
	// (prompt, ts, turns); this also returns id and cwd off the session_meta
	// line, so an over-long line BEFORE session_meta costs the session its
	// identity and its project directory as well as its turn count. id has a
	// fallback (extractIDFromFilename) and cwd has none — out.Project is simply
	// left empty. Name both in the report so a blank Project has something to
	// be traced back to.
	//
	// Deliberately NOT propagated: coldImportRollouts calls this from inside a
	// filepath.WalkDir callback, where a returned error aborts the whole import
	// and drops every session after this one. The skip-or-kill policy question
	// is 6fbf83b3, open.
	if err := scanner.Err(); err != nil {
		idState := "id was read from session_meta"
		if id == "" {
			idState = "session_meta was never reached — id falls back to the filename"
		}
		cwdState := "cwd was read"
		if cwd == "" {
			cwdState = "cwd is empty — Project will be blank"
		}
		log.Printf("parseCodexSession: %s: scan stopped early: %v; "+
			"reporting the first %d turn(s) only; %s; %s",
			path, err, turns, idState, cwdState)
	}

	return
}

// extractIDFromFilename extracts the UUID from a Codex session filename.
// Format: rollout-2026-04-13T02-00-13-019d8491-627f-79e1-89a7-d32ed21ee93e.jsonl
//
// The return value is a session KEY — it is matched against Codex rollout
// files and stored as a session id — so a trailing 36 bytes that is not a
// UUID is refused rather than shortened. The whole name comes back instead:
// a wrong id that looks well-formed matches nothing while reading as stable,
// which is harder to trace than a name that never parsed.
func extractIDFromFilename(name string) string {
	name = strings.TrimSuffix(name, ".jsonl")
	// The UUID is the last 36 bytes (8-4-4-4-12 with dashes)
	if len(name) >= 36 {
		candidate := name[len(name)-36:]
		if isUUID(candidate) {
			return candidate
		}
	}
	return name
}

// isUUID reports whether s is exactly the 36-byte 8-4-4-4-12 hexadecimal form,
// in either case. Every byte is checked, not just the four dashes: an
// unchecked position accepts anything, including the continuation byte left
// behind when a fixed-width cut opens in the middle of a multi-byte rune.
// Hex digits are single-byte by definition, so a candidate that passes here is
// valid UTF-8 without the question having to be asked separately.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			if !isHexDigit(s[i]) {
				return false
			}
		}
	}
	return true
}

func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// findRolloutForThread does a best-effort scan of codex's rollout trees for a
// rollout file whose name carries the given thread_id. Returns "" if not
// found — caller treats that as "rollout file not yet on disk" and proceeds
// without the path. The path can be backfilled later by re-globbing.
func findRolloutForThread(threadID string) string {
	if threadID == "" {
		return ""
	}
	rolloutDirectories, err := codexRolloutDirectories()
	if err != nil {
		return ""
	}
	var found string
	for _, rolloutDirectory := range rolloutDirectories {
		_ = filepath.WalkDir(rolloutDirectory, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if strings.HasSuffix(d.Name(), ".jsonl") && strings.Contains(d.Name(), threadID) {
				found = path
				return filepath.SkipAll
			}
			return nil
		})
		if found != "" {
			return found
		}
	}
	return found
}

// truncateAtRuneBoundary returns the longest prefix of s that is no longer than
// maxBytes and does not end part-way through a multi-byte UTF-8 sequence.
//
// Cutting a Go string at a fixed byte offset splits whatever rune straddles that
// offset, and the result is not valid UTF-8. Nothing reports it: encoding/json
// substitutes U+FFFD rather than failing, so the reader sees a replacement
// character and no error is raised anywhere along the way. The one caller here
// cuts a discovered session's first user message down to a label, and that label
// is encoded into msg.StoredSession.Prompt and written to stdout as JSON for
// bridge-server, which serves it to the session list — so a split rune survives
// the request and a reload does not fix it.
//
// The walk-back costs at most three byte comparisons and allocates nothing,
// which is why it is preferred here over converting to []rune.
func truncateAtRuneBoundary(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	// s[cut] is the first byte past the prefix. While it is a continuation
	// byte, a rune straddles the cut, so move the cut earlier.
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// runChangedSince parses -changed-since's one argument and prints the
// sessions as one JSON array, as -discover does.
func runChangedSince(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "-changed-since: one RFC 3339 time is required")
		return 2
	}
	since, err := time.Parse(time.RFC3339Nano, args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "-changed-since: %v\n", err)
		return 2
	}
	sessions, err := discoverSessionsChangedSince(since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "-changed-since: %v\n", err)
		return 1
	}
	if err := json.NewEncoder(os.Stdout).Encode(sessions); err != nil {
		fmt.Fprintf(os.Stderr, "-changed-since: %v\n", err)
		return 1
	}
	return 0
}
