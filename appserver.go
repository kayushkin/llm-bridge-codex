package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// AppServer manages the codex app-server process and WebSocket connection.
type AppServer struct {
	mu sync.Mutex

	cmd    *exec.Cmd
	client *WSClient

	codexPath string
	port      int
	workdir   string

	notifHandlers map[string]NotificationHandler
	reqHandlers   map[string]RequestHandler
}

func NewAppServer(codexPath string, port int, workdir string) *AppServer {
	return &AppServer{
		codexPath:     codexPath,
		port:          port,
		workdir:       workdir,
		notifHandlers: make(map[string]NotificationHandler),
		reqHandlers:   make(map[string]RequestHandler),
	}
}

func (a *AppServer) OnNotification(method string, h NotificationHandler) {
	a.notifHandlers[method] = h
}

func (a *AppServer) OnRequest(method string, h RequestHandler) {
	a.reqHandlers[method] = h
}

// Client returns a function that yields the current WebSocket client.
// This matches the Codex constructor pattern.
func (a *AppServer) Client() func() *WSClient {
	return func() *WSClient {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.client
	}
}

// Start spawns the app-server (if needed) and connects via WebSocket.
func (a *AppServer) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Try connecting to an already-running instance first.
	if err := a.connect(); err == nil {
		log.Printf("[appserver] connected to existing instance on port %d", a.port)
		return nil
	}

	// Spawn the process.
	listenAddr := fmt.Sprintf("ws://127.0.0.1:%d", a.port)
	cmd := exec.Command(a.codexPath, "app-server", "--listen", listenAddr)
	cmd.Dir = a.workdir
	cmd.Stdout = os.Stderr // Bridge stdout is reserved for NDJSON events
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn codex app-server: %w", err)
	}
	a.cmd = cmd
	log.Printf("[appserver] spawned pid=%d on %s", cmd.Process.Pid, listenAddr)

	// Poll for the WebSocket to become available.
	backoff := 100 * time.Millisecond
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		if err := a.connect(); err == nil {
			log.Printf("[appserver] connected after startup")
			return nil
		}

		if backoff < 2*time.Second {
			backoff *= 2
		}
	}

	return fmt.Errorf("app-server did not become ready within 15s")
}

// Shutdown closes the WebSocket but leaves the app-server process running.
func (a *AppServer) Shutdown() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil {
		a.client.Close()
		a.client = nil
	}
}

// Kill stops the app-server process entirely.
func (a *AppServer) Kill() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.client != nil {
		a.client.Close()
		a.client = nil
	}

	if a.cmd != nil && a.cmd.Process != nil {
		_ = a.cmd.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- a.cmd.Wait() }()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = a.cmd.Process.Kill()
		}
		a.cmd = nil
	}
}

func (a *AppServer) connect() error {
	u := url.URL{Scheme: "ws", Host: fmt.Sprintf("127.0.0.1:%d", a.port)}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return err
	}

	client := NewWSClient(conn)

	for method, h := range a.notifHandlers {
		client.OnNotification(method, h)
	}
	for method, h := range a.reqHandlers {
		client.OnRequest(method, h)
	}

	go client.ReadPump()

	// Two-step handshake: initialize + initialized.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	initParams := map[string]any{
		"clientInfo": map[string]any{
			"name":    "llm-bridge-codex",
			"version": "0.1.0",
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	}
	if _, err := client.Call(ctx, "initialize", initParams); err != nil {
		client.Close()
		return fmt.Errorf("initialize handshake: %w", err)
	}
	if err := client.Notify("initialized", nil); err != nil {
		client.Close()
		return fmt.Errorf("initialized notification: %w", err)
	}

	a.client = client
	return nil
}
