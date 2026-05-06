package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// emitEvent writes a canonical msg.Event as NDJSON to stdout.
// This is the sole output channel — llm-bridge reads these events.
func emitEvent(mu *sync.Mutex, enc *json.Encoder, event msg.Event) {
	mu.Lock()
	defer mu.Unlock()
	if err := enc.Encode(event); err != nil {
		log.Printf("[emit] encode error: %v", err)
	}
}

// execCodexPTY replaces this process with the upstream `codex` CLI so the
// inherited pseudoterminal is wired straight through to its native TUI. The
// caller (llm-bridge-server.StartProcessPTY) already set the harness's auth
// path env (and inherited cwd); pty mode bypasses the AppServer/WebSocket
// path entirely so the user gets the unmodified `codex` experience.
func execCodexPTY() {
	cfg := loadConfig()
	bin, err := exec.LookPath(cfg.CodexPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "llm-bridge-codex pty: codex binary not found at %q: %v\n", cfg.CodexPath, err)
		os.Exit(127)
	}
	if err := syscall.Exec(bin, []string{bin}, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "llm-bridge-codex pty: exec %s: %v\n", bin, err)
		os.Exit(127)
	}
}

func main() {
	// PTY mode hand-off. llm-bridge-server's StartProcessPTY launches us
	// inside a pseudoterminal with LLMBRIDGE_PTY_MODE=1; the contract is
	// that we exec into the upstream `codex` CLI so the pty fd connects
	// directly to its TUI. The harness wrapper has nothing to do in pty
	// mode — there's no AppServer connection, no msg.Event translation.
	if os.Getenv("LLMBRIDGE_PTY_MODE") == "1" {
		execCodexPTY()
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "-discover" {
		sessions, err := discoverSessions()
		if err != nil {
			fmt.Fprintf(os.Stderr, "discover: %v\n", err)
			os.Exit(1)
		}
		json.NewEncoder(os.Stdout).Encode(sessions)
		os.Exit(0)
	}

	// -import-history is part of the conformance contract but not yet
	// implemented for codex. Exit 2 to signal "unsupported" rather than
	// silently falling through to the JSON-RPC loop, which would otherwise
	// show up as a false-positive PASS on the conformance dashboard.
	if len(os.Args) > 1 && os.Args[1] == "-import-history" {
		fmt.Fprintln(os.Stderr, "llm-bridge-codex: -import-history not yet implemented")
		os.Exit(2)
	}

	// All log output goes to stderr — stdout is reserved for NDJSON events.
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[llm-bridge-codex] ")

	cfg := loadConfig()

	log.Printf("starting: codex=%s port=%d workdir=%s model=%s approval=%s sandbox=%s effort=%s",
		cfg.CodexPath, cfg.CodexWSPort, cfg.CodexWorkdir, cfg.CodexModel, cfg.ApprovalMode, cfg.SandboxPolicy, cfg.Effort)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var emitMu sync.Mutex
	enc := json.NewEncoder(os.Stdout)

	emit := func(event msg.Event) {
		emitEvent(&emitMu, enc, event)
	}

	bridge := NewBridge(cfg, emit)

	// Handle SIGINT: interrupt current turn. Resulting state is
	// derived centrally — bridge-server tracks pause/abort calls
	// and flips SessionState accordingly.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigCh {
			log.Printf("received %s", sig)
			if sig == syscall.SIGINT {
				if err := bridge.HandleInterrupt(ctx); err != nil {
					log.Printf("interrupt: %v", err)
				}
				continue
			}
			// SIGTERM: clean shutdown.
			cancel()
		}
	}()

	// Blocking stdin read loop — process one JSON-RPC request at a time.
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB buffer

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params,omitempty"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("parse request: %v", err)
			continue
		}

		switch req.Method {
		case "start":
			var params StartParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				log.Printf("parse start params: %v", err)
				emitError(emit, "", "", "INVALID_PARAMS", err.Error())
				continue
			}

			// Prefer the new BridgeSessionID; fall back to legacy SessionID
			// for older bridge-server binaries.
			bsid := params.BridgeSessionID
			if bsid == "" {
				bsid = params.SessionID
			}
			if err := bridge.Init(ctx, bsid, params.ClientID, emit); err != nil {
				log.Printf("init: %v", err)
				emitError(emit, bsid, params.ClientID, "INIT_FAILED", err.Error())
				continue
			}

			if params.Resume {
				if err := bridge.HandleResumeThread(ctx, params.SessionID); err != nil {
					log.Printf("resume: %v", err)
					emitError(emit, bridge.currentSessionID(), bridge.clientID, "RESUME_FAILED", err.Error())
				}
			} else {
				if err := bridge.HandleStart(ctx, params); err != nil {
					log.Printf("start: %v", err)
					emitError(emit, bridge.currentSessionID(), bridge.clientID, "START_FAILED", err.Error())
				}
			}

		case "message":
			var params MessageParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				log.Printf("parse message params: %v", err)
				emitError(emit, bridge.currentSessionID(), bridge.clientID, "INVALID_PARAMS", err.Error())
				continue
			}

			if err := bridge.HandleMessage(ctx, params.Content); err != nil {
				log.Printf("message: %v", err)
				emitError(emit, bridge.currentSessionID(), bridge.clientID, "MESSAGE_FAILED", err.Error())
			}

		case "compact":
			if err := bridge.HandleCompact(ctx); err != nil {
				log.Printf("compact: %v", err)
				emitError(emit, bridge.currentSessionID(), bridge.clientID, "COMPACT_FAILED", err.Error())
			}

		case "resume":
			if err := bridge.HandleResume(ctx); err != nil {
				log.Printf("resume: %v", err)
				emitError(emit, bridge.currentSessionID(), bridge.clientID, "RESUME_FAILED", err.Error())
			}

		case "set_model":
			var params SetModelParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				log.Printf("parse set_model params: %v", err)
				emitError(emit, bridge.currentSessionID(), bridge.clientID, "INVALID_PARAMS", err.Error())
				continue
			}
			bridge.HandleSetModel(params.Model)

		case "set_permission_mode":
			var params SetPermissionModeParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				log.Printf("parse set_permission_mode params: %v", err)
				emitError(emit, bridge.currentSessionID(), bridge.clientID, "INVALID_PARAMS", err.Error())
				continue
			}
			bridge.HandleSetPermissionMode(params.Mode)

		case "control":
			var params ControlParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				log.Printf("parse control params: %v", err)
				emitError(emit, bridge.currentSessionID(), bridge.clientID, "INVALID_PARAMS", err.Error())
				continue
			}
			if err := bridge.HandleControl(ctx, params); err != nil {
				log.Printf("control: %v", err)
				emitError(emit, bridge.currentSessionID(), bridge.clientID, "CONTROL_FAILED", err.Error())
			}

		default:
			// Handle config:<json> method (sent by llm-bridge-server for mid-session config).
			if strings.HasPrefix(req.Method, "config:") {
				configJSON := req.Method[len("config:"):]
				bridge.HandleConfig(configJSON)
			} else {
				log.Printf("unknown method: %s", req.Method)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("stdin read error: %v", err)
	}

	// stdin closed — llm-bridge killed us or crashed. Clean up.
	log.Printf("stdin closed, shutting down")
	bridge.Shutdown()
}

func emitError(emit func(msg.Event), sessionID, clientID, code, message string) {
	now := time.Now()
	// On init-failed paths sessionID may be the bridge id we received in
	// StartParams; mirror it onto both fields so bridge-server can route.
	emit(msg.Event{
		Type:             msg.EventError,
		Harness:          msg.HarnessCodex,
		BridgeSessionID:  sessionID,
		HarnessSessionID: sessionID,
		ClientID:         clientID,
		Timestamp:        now,
		Error:            &msg.ErrorEvent{Code: code, Message: message},
	})
	// SessionError is derived centrally by llm-bridge-server from EventError.
}
