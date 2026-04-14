package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
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

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-discover" {
		sessions, err := discoverSessions()
		if err != nil {
			fmt.Fprintf(os.Stderr, "discover: %v\n", err)
			os.Exit(1)
		}
		json.NewEncoder(os.Stdout).Encode(sessions)
		os.Exit(0)
	}

	// All log output goes to stderr — stdout is reserved for NDJSON events.
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[llm-bridge-codex] ")

	cfg := loadConfig()

	log.Printf("starting: codex=%s port=%d workdir=%s model=%s approval=%s",
		cfg.CodexPath, cfg.CodexWSPort, cfg.CodexWorkdir, cfg.CodexModel, cfg.ApprovalMode)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var emitMu sync.Mutex
	enc := json.NewEncoder(os.Stdout)

	emit := func(event msg.Event) {
		emitEvent(&emitMu, enc, event)
	}

	bridge := NewBridge(cfg, emit)

	// Handle SIGINT: interrupt current turn, emit idle state.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigCh {
			log.Printf("received %s", sig)
			if sig == syscall.SIGINT {
				if err := bridge.HandleInterrupt(ctx); err != nil {
					log.Printf("interrupt: %v", err)
				}
				e := bridge.event(msg.EventSessionState)
				e.State = &msg.StateEvent{State: msg.SessionIdle}
				emit(e)
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

			// Initialize bridge on first start request.
			if err := bridge.Init(ctx, params.SessionID, params.ClientID, emit); err != nil {
				log.Printf("init: %v", err)
				emitError(emit, params.SessionID, params.ClientID, "INIT_FAILED", err.Error())
				continue
			}

			if params.Resume {
				if err := bridge.HandleResume(ctx); err != nil {
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

		default:
			log.Printf("unknown method: %s", req.Method)
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
	e := msg.Event{
		Type:      msg.EventError,
		Harness:   msg.HarnessCodex,
		SessionID: sessionID,
		ClientID:  clientID,
		Timestamp: now,
	}
	e.Error = &msg.ErrorEvent{Code: code, Message: message}
	emit(e)

	se := msg.Event{
		Type:      msg.EventSessionState,
		Harness:   msg.HarnessCodex,
		SessionID: sessionID,
		ClientID:  clientID,
		Timestamp: now,
	}
	se.State = &msg.StateEvent{State: msg.SessionError}
	emit(se)
}
