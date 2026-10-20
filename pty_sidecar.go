package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// runPTYSidecar is -pty-sidecar: the co-process llm-bridge-server starts
// beside a codex pty session. In pty mode the wrapper execs straight into
// codex's own screen, so nothing translates what happens there into events,
// and the dash showed nothing of a turn typed in it. This follows the
// session's rollout file as codex writes it and posts each event to
// {LLMBRIDGE_BRIDGE_SERVER_URL}/sidecar/event/{LLMBRIDGE_BRIDGE_SESSION_ID},
// the route Claude Code's sidecar uses.
//
// Which rollout:
//   - LLMBRIDGE_PTY_RESUME_ID set: that thread's rollout, which `codex resume`
//     appends to. What it already holds the dash already has, so it is read
//     only to learn the thread's model, and events start after it.
//   - unset: the first rollout created after this process started whose
//     session_meta cwd is LLMBRIDGE_PTY_CWD. Codex writes the file when the
//     first message is sent, which can be any time, so this waits until it
//     appears or the session ends.
//
// It runs until stdin closes (the server stopping it) or SIGTERM, then reads
// once more so the turn's last lines are not lost.
func runPTYSidecar() int {
	log.SetOutput(os.Stderr)
	log.SetPrefix("[llm-bridge-codex pty-sidecar] ")
	bridgeSessionID := os.Getenv("LLMBRIDGE_BRIDGE_SESSION_ID")
	bridgeServerURL := os.Getenv("LLMBRIDGE_BRIDGE_SERVER_URL")
	if bridgeSessionID == "" || bridgeServerURL == "" {
		log.Printf("LLMBRIDGE_BRIDGE_SESSION_ID and LLMBRIDGE_BRIDGE_SERVER_URL are both required")
		return 2
	}
	cwd := os.Getenv("LLMBRIDGE_PTY_CWD")
	resumeID := os.Getenv("LLMBRIDGE_PTY_RESUME_ID")

	stop := make(chan struct{})
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
		stdinClosed := make(chan struct{})
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			close(stdinClosed)
		}()
		select {
		case <-stdinClosed:
		case <-sigs:
		}
		close(stop)
	}()

	post := newSidecarPoster(bridgeServerURL, bridgeSessionID)
	started := time.Now()

	var path string
	var skipExisting bool
	if resumeID != "" {
		path = findRolloutForThread(resumeID)
		if path == "" {
			log.Printf("no rollout for resumed thread %s; codex opened a new one, so looking for that instead", resumeID)
		} else {
			skipExisting = true
		}
	}
	if path == "" {
		path = waitForNewRollout(cwd, started, stop)
		if path == "" {
			log.Printf("session ended before codex wrote a rollout in %s", cwd)
			return 0
		}
	}
	log.Printf("following %s", path)
	if err := followRollout(path, resumeID, skipExisting, post, stop); err != nil {
		log.Printf("ERROR following %s: %v", path, err)
		return 1
	}
	return 0
}

// followRollout reads path as it grows and posts each event. With
// skipExisting, entries already in the file when it was opened update the
// reader's state but are not posted.
func followRollout(path, threadID string, skipExisting bool, post func(msg.Event), stop <-chan struct{}) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var existing int64
	if skipExisting {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		existing = info.Size()
	}

	reader := newRolloutReader(threadID)
	buffered := bufio.NewReader(f)
	var offset int64
	var pending []byte
	stopping := false
	for {
		chunk, err := buffered.ReadBytes('\n')
		pending = append(pending, chunk...)
		offset += int64(len(chunk))
		if err == nil {
			events, translateErr := reader.translateLine(pending)
			if translateErr != nil {
				log.Printf("ERROR %s: %v", path, translateErr)
			} else if offset > existing {
				for _, ev := range events {
					post(ev)
				}
			}
			pending = pending[:0]
			continue
		}
		if err != io.EOF {
			return err
		}
		if stopping {
			return nil
		}
		select {
		case <-stop:
			// One more pass picks up what codex wrote as it was stopped.
			stopping = true
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// waitForNewRollout polls the date folders codex writes into until a rollout
// created at or after since, for cwd, appears.
func waitForNewRollout(cwd string, since time.Time, stop <-chan struct{}) string {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("ERROR home directory: %v", err)
		return ""
	}
	sessionsDir := filepath.Join(home, ".codex", "sessions")
	for {
		if path := newRolloutFor(sessionsDir, cwd, since); path != "" {
			return path
		}
		select {
		case <-stop:
			return ""
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func newRolloutFor(sessionsDir, cwd string, since time.Time) string {
	// Codex files a rollout under the local date it was created; look at
	// today's and yesterday's folders so a session started near midnight is
	// still found.
	for _, day := range []time.Time{time.Now(), time.Now().Add(-24 * time.Hour)} {
		dir := filepath.Join(sessionsDir, day.Format("2006"), day.Format("01"), day.Format("02"))
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), "rollout-") || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil || info.ModTime().Before(since) {
				continue
			}
			path := filepath.Join(dir, e.Name())
			meta, ok := readRolloutSessionMeta(path)
			if !ok || meta.timestamp.Before(since.Add(-time.Second)) {
				continue
			}
			if cwd == "" || meta.cwd == cwd {
				return path
			}
		}
	}
	return ""
}

type rolloutSessionMeta struct {
	cwd       string
	timestamp time.Time
}

func readRolloutSessionMeta(path string) (rolloutSessionMeta, bool) {
	f, err := os.Open(path)
	if err != nil {
		return rolloutSessionMeta{}, false
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil {
		return rolloutSessionMeta{}, false
	}
	var entry struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &entry) != nil || entry.Type != "session_meta" {
		return rolloutSessionMeta{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, entry.Payload.Timestamp)
	if err != nil {
		return rolloutSessionMeta{}, false
	}
	return rolloutSessionMeta{cwd: entry.Payload.Cwd, timestamp: ts}, true
}

// newSidecarPoster posts one event to the bridge's sidecar route. A failed
// post is logged and the next event still goes: losing one beats losing all.
func newSidecarPoster(bridgeServerURL, bridgeSessionID string) func(msg.Event) {
	endpoint := fmt.Sprintf("%s/sidecar/event/%s", strings.TrimRight(bridgeServerURL, "/"), url.PathEscape(bridgeSessionID))
	client := &http.Client{Timeout: 5 * time.Second}
	return func(ev msg.Event) {
		ev.BridgeSessionID = bridgeSessionID
		body, err := json.Marshal(ev)
		if err != nil {
			log.Printf("ERROR marshal %s event: %v", ev.Type, err)
			return
		}
		resp, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
		if err != nil {
			log.Printf("ERROR post %s event: %v", ev.Type, err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("ERROR post %s event: status %d", ev.Type, resp.StatusCode)
		}
	}
}
