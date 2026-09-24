package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStartParamsReadDisabledTools(t *testing.T) {
	var params StartParams
	if err := json.Unmarshal([]byte(`{"disabled_tools":["shell_tool","web_search"]}`), &params); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if strings.Join(params.DisabledTools, ",") != "shell_tool,web_search" {
		t.Fatalf("DisabledTools = %v", params.DisabledTools)
	}
}

func TestDisabledToolsReachAppServerArgs(t *testing.T) {
	b := &Bridge{cfg: Config{}}
	if err := b.applyStartConfig(StartParams{DisabledTools: []string{"shell_tool", "web_search", "code_mode_host"}}); err != nil {
		t.Fatalf("applyStartConfig: %v", err)
	}
	args, err := b.buildAppServerExtraArgs()
	if err != nil {
		t.Fatalf("buildAppServerExtraArgs: %v", err)
	}
	got := strings.Join(args, " ")
	want := `--disable shell_tool -c web_search="disabled" --disable code_mode_host`
	if got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestEveryDisabledToolNameMapsToArgs(t *testing.T) {
	names := []string{"shell_tool", "unified_exec", "view_image", "image_generation", "multi_agent",
		"browser_use", "computer_use", "apps", "plugins", "sleep_tool", "code_mode_host", "web_search"}
	for _, name := range names {
		args, err := appServerArgsDisablingTools([]string{name})
		if err != nil || len(args) != 2 {
			t.Errorf("%s: args %v, err %v", name, args, err)
		}
	}
	if len(appServerArgsByDisabledToolName) != len(names) {
		t.Errorf("map holds %d names, test lists %d", len(appServerArgsByDisabledToolName), len(names))
	}
}

// An unknown name must fail the start and say which name, never be dropped.
func TestUnknownDisabledToolFailsTheStart(t *testing.T) {
	b := &Bridge{cfg: Config{}}
	err := b.applyStartConfig(StartParams{DisabledTools: []string{"shell_tool", "Bash"}})
	if err == nil {
		t.Fatalf("applyStartConfig accepted an unknown tool name")
	}
	if !strings.Contains(err.Error(), `"Bash"`) {
		t.Fatalf("error does not name the tool: %v", err)
	}
	if b.cfg.DisabledTools != nil {
		t.Fatalf("a refused list was stored: %v", b.cfg.DisabledTools)
	}
}
