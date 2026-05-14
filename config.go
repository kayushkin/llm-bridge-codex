package main

import (
	"encoding/json"
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

	// CodexHooks is the per-session hooks tree forwarded by bridge-server
	// (HarnessConfig.codex_hooks). Stored as raw JSON; translated to
	// `-c hooks.<EventName>=<inline-toml>` args at app-server spawn time.
	// Empty / nil → no overrides.
	CodexHooks json.RawMessage

	// DisableSandbox is a host-level escape hatch for environments where
	// codex's bwrap-based sandbox can't initialize (e.g. unprivileged
	// user namespaces without CAP_NET_ADMIN, raising
	// "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted").
	//
	// When true, every codex session's SandboxPolicy is pinned to
	// "danger-full-access" regardless of the canonical permission mode
	// (so Plan / Read / Auto / Rules / Ask All all run unsandboxed at
	// the codex layer). The bridge prehook + permission-store remain
	// the security gate; codex's sandbox was always defense-in-depth.
	//
	// Set via CODEX_DISABLE_SANDBOX=1. Off by default.
	DisableSandbox bool
}

func loadConfig() Config {
	cfg := Config{
		CodexPath:      envOr("CODEX_PATH", "codex"),
		CodexModel:     os.Getenv("CODEX_MODEL"),
		CodexWorkdir:   os.Getenv("CODEX_WORKDIR"),
		ApprovalMode:   envOr("CODEX_APPROVAL_MODE", "never"),
		SandboxPolicy:  envOr("CODEX_SANDBOX", "workspace-write"),
		Effort:         os.Getenv("CODEX_EFFORT"),
		DisableSandbox: envBool("CODEX_DISABLE_SANDBOX"),
	}

	// CODEX_WS_PORT="0" (the default) makes the bridge pick an ephemeral
	// port via the kernel — each codex bridge process gets its own port
	// and never collides with another concurrent session on the same host.
	// Pin an explicit port (e.g. "19836") only when an external client
	// needs to attach to a known port.
	port, err := strconv.Atoi(envOr("CODEX_WS_PORT", "0"))
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

// envBool returns true if the named env var is set to a truthy value
// (1, true, yes — case-insensitive). Unset / empty / anything else → false.
func envBool(key string) bool {
	switch v := os.Getenv(key); v {
	case "1", "true", "TRUE", "True", "yes", "YES", "Yes":
		return true
	}
	return false
}
