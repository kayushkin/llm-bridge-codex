package main

import (
	"fmt"
	"sort"
	"strings"
)

// appServerArgsByDisabledToolName maps each tool name tool-store gives the
// codex harness to the `codex app-server` arguments that turn that tool off.
//
// Codex has no deny-list flag; its built-in tools hang off feature flags.
// `--disable <feature>` is codex's own spelling of `-c features.<feature>=false`.
// Web search is not a feature but a top-level setting whose values are
// disabled / cached / indexed / live. Both measured on codex-cli 0.156.1
// against a live app-server's config/read.
var appServerArgsByDisabledToolName = map[string][]string{
	"shell_tool":       {"--disable", "shell_tool"},
	"unified_exec":     {"--disable", "unified_exec"},
	"view_image":       {"--disable", "view_image"},
	"image_generation": {"--disable", "image_generation"},
	"multi_agent":      {"--disable", "multi_agent"},
	"browser_use":      {"--disable", "browser_use"},
	"computer_use":     {"--disable", "computer_use"},
	"apps":             {"--disable", "apps"},
	"plugins":          {"--disable", "plugins"},
	"sleep_tool":       {"--disable", "sleep_tool"},
	"code_mode_host":   {"--disable", "code_mode_host"},
	"web_search":       {"-c", `web_search="disabled"`},
}

// appServerArgsDisablingTools renders a session's disabled_tools as
// `codex app-server` arguments, in the order given. A name this wrapper
// cannot map is an error naming it: dropping it would leave on a tool the
// session asked to have off.
func appServerArgsDisablingTools(disabledTools []string) ([]string, error) {
	var args []string
	for _, name := range disabledTools {
		toolArgs, known := appServerArgsByDisabledToolName[name]
		if !known {
			return nil, fmt.Errorf("disabled_tools: codex has no tool %q (known: %s)", name, strings.Join(knownDisabledToolNames(), ", "))
		}
		args = append(args, toolArgs...)
	}
	return args, nil
}

func knownDisabledToolNames() []string {
	names := make([]string, 0, len(appServerArgsByDisabledToolName))
	for name := range appServerArgsByDisabledToolName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
