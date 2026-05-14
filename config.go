package main

import (
	"log"
	"os"
	"strconv"
)

// Config holds all bridge configuration loaded from env vars.
type Config struct {
	CodexPath     string // Path to codex binary
	CodexWSPort   int    // WebSocket port for app-server
	CodexModel    string // Model override
	CodexWorkdir  string // Working directory for codex
	ApprovalMode  string // "on-request", "never", "granular", "untrusted"
	SandboxPolicy string // "read-only", "workspace-write", "danger-full-access"
	Effort        string // Effort level for turns

	// DisableNetwork toggles codex's sandbox network gate. When true, the
	// harness adds `-c sandbox_workspace_write.network_access=false` to
	// the app-server spawn. Orthogonal to ApprovalMode/SandboxPolicy.
	DisableNetwork bool
}

func loadConfig() Config {
	cfg := Config{
		CodexPath:     envOr("CODEX_PATH", "codex"),
		CodexModel:    os.Getenv("CODEX_MODEL"),
		CodexWorkdir:  os.Getenv("CODEX_WORKDIR"),
		ApprovalMode:  envOr("CODEX_APPROVAL_MODE", "never"),
		SandboxPolicy: envOr("CODEX_SANDBOX", "workspace-write"),
		Effort:        os.Getenv("CODEX_EFFORT"),
	}

	port, err := strconv.Atoi(envOr("CODEX_WS_PORT", "19836"))
	if err != nil {
		log.Fatalf("invalid CODEX_WS_PORT: %v", err)
	}
	cfg.CodexWSPort = port

	if cfg.CodexWorkdir == "" {
		cfg.CodexWorkdir, _ = os.Getwd()
	}

	return cfg
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
