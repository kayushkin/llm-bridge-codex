package main

// -oneshot mode: a stateless, single-turn structured LLM call that runs on
// the operator's Codex login (a ChatGPT subscription on this host).
//
// Contract, shared with llm-bridge-claudecode (see llm-bridge-server's
// internal/server/oneshot.go): read a msg.OneShotRequest JSON from stdin,
// write a msg.OneShotResponse JSON to stdout, exit 0. On failure, write
// {"error":"..."} to stdout and exit 1 so the server can surface the message
// verbatim.
//
// The call spawns `codex exec --json` and lets the CLI authenticate from its
// own login ($CODEX_HOME/auth.json). Everything below was measured against
// codex-cli 0.156.1 on 2026-09-24, one probe per decision:
//
//   - A classifier answers and does nothing else. The shell, code mode,
//     browser, apps, plugins and the rest are switched off by feature flag,
//     and web search by config. What the model still lists — exec (code mode,
//     whose host is disabled, so it fails closed), wait and request_user_input
//     — can run nothing. The sandbox is read-only on top of that.
//   - --ignore-user-config and --ignore-rules keep this machine's config.toml
//     (which sets danger-full-access) and execpolicy rules out of the call;
//     auth still comes from CODEX_HOME. --ephemeral writes no session file.
//   - The system prompt goes in as developer_instructions: exec has no flag
//     for it, and the key is honoured.
//   - --output-schema is sent to OpenAI as a strict response format, which
//     refuses a schema unless every object sets additionalProperties:false and
//     lists every property as required. strictSchema rewrites the caller's
//     schema that way, making each optional property nullable, and
//     dropNullOptionals removes those nulls from the answer, so the caller
//     receives the shape it asked for.
//   - There is no setting that caps output tokens (model_max_output_tokens is
//     ignored as unrecognised), so MaxTokens cannot be honoured here. A reply
//     is still validated as exactly one JSON object.
//   - Usage arrives on the turn.completed event. input_tokens includes the
//     cached ones and output_tokens includes the reasoning ones; they are split
//     into msg.TokenUsage's separate fields here.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// oneShotTimeout bounds the spawned CLI, just under the server's own
// six-minute context, so a timeout names the model call rather than a
// killed process.
const oneShotTimeout = 5 * time.Minute

// oneShotDisabledFeatures are the codex features switched off for a oneshot
// call: everything that runs code, reaches outside, or loads more context.
var oneShotDisabledFeatures = []string{
	"shell_tool", "unified_exec", "code_mode_host", "apps", "browser_use", "browser_use_external",
	"computer_use", "image_generation", "in_app_browser", "multi_agent", "plugins", "remote_plugin",
	"skill_search", "skill_mcp_dependency_install", "view_image", "tool_suggest", "goals", "hooks", "sleep_tool",
}

func oneShotWorkingDirectory() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".llm-bridge-codex", "oneshot"), nil
}

func runOneShot() int {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		writeOneShotError("read stdin: " + err.Error())
		return 1
	}
	var request msg.OneShotRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		writeOneShotError("decode request: " + err.Error())
		return 1
	}
	if request.Prompt == "" {
		writeOneShotError("prompt required")
		return 1
	}
	if id := os.Getenv("LLMBRIDGE_CREDENTIAL_ID"); id != "" {
		writeOneShotError("instance has a bound credential (" + id + "); oneshot mode runs on the Codex login and does not resolve auth-store credentials — use a session instead")
		return 1
	}
	if os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("CODEX_API_KEY") != "" {
		writeOneShotError("OPENAI_API_KEY or CODEX_API_KEY is set in the harness environment; oneshot mode exists to use the Codex login and refuses to guess which credential wins — unset it")
		return 1
	}
	cfg := loadConfig()
	model := request.Model
	if model == "" {
		model = cfg.CodexModel
	}
	if model == "" {
		// With the user's config ignored, codex would fall back to a built-in
		// default this code cannot name, and the response could not say which
		// model answered.
		writeOneShotError("no model: the request names none and CODEX_MODEL is unset")
		return 1
	}
	bin, err := exec.LookPath(cfg.CodexPath)
	if err != nil {
		writeOneShotError(fmt.Sprintf("codex binary not found at %q: %v", cfg.CodexPath, err))
		return 1
	}
	workDir, err := oneShotWorkingDirectory()
	if err != nil {
		writeOneShotError("oneshot working directory: " + err.Error())
		return 1
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		writeOneShotError("create oneshot working directory: " + err.Error())
		return 1
	}

	args := []string{"exec", "--json", "--ephemeral", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules",
		"-s", "read-only", "-C", workDir, "-m", model, "-c", `web_search="disabled"`}
	for _, feature := range oneShotDisabledFeatures {
		args = append(args, "--disable", feature)
	}
	if request.SystemPrompt != "" {
		encoded, _ := json.Marshal(request.SystemPrompt)
		args = append(args, "-c", "developer_instructions="+string(encoded))
	}
	var callerSchema any
	if len(request.Schema) > 0 {
		if err := json.Unmarshal(request.Schema, &callerSchema); err != nil {
			writeOneShotError("request schema is not valid JSON: " + err.Error())
			return 1
		}
		strict, err := json.Marshal(strictSchema(callerSchema))
		if err != nil {
			writeOneShotError("encode strict schema: " + err.Error())
			return 1
		}
		schemaFile, err := os.CreateTemp(workDir, "schema-*.json")
		if err != nil {
			writeOneShotError("write schema file: " + err.Error())
			return 1
		}
		defer os.Remove(schemaFile.Name())
		if _, err := schemaFile.Write(strict); err != nil {
			schemaFile.Close()
			writeOneShotError("write schema file: " + err.Error())
			return 1
		}
		schemaFile.Close()
		args = append(args, "--output-schema", schemaFile.Name())
	}
	args = append(args, "-")

	ctx, cancel := context.WithTimeout(context.Background(), oneShotTimeout)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(request.Prompt)
	cmd.Dir = workDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		writeOneShotError(fmt.Sprintf("codex exec exceeded %s", oneShotTimeout))
		return 1
	}
	result, err := readExecEvents(out)
	if err != nil {
		writeOneShotError(fmt.Sprintf("codex exec: %v (stderr: %s)", err, truncateForError(stderr.String())))
		return 1
	}
	if runErr != nil {
		writeOneShotError(fmt.Sprintf("codex exec: %v (stderr: %s)", runErr, truncateForError(stderr.String())))
		return 1
	}

	response := msg.OneShotResponse{DurationMs: time.Since(start).Milliseconds(), Model: model, Usage: result.usage}
	if callerSchema != nil {
		parsed, err := extractJSONObject(result.lastMessage)
		if err != nil {
			writeOneShotError(fmt.Sprintf("reply is not the requested JSON: %v (reply: %s)", err, truncateForError(result.lastMessage)))
			return 1
		}
		var answer any
		if err := json.Unmarshal(parsed, &answer); err != nil {
			writeOneShotError("decode reply: " + err.Error())
			return 1
		}
		cleaned, err := json.Marshal(dropNullOptionals(answer, callerSchema))
		if err != nil {
			writeOneShotError("encode reply: " + err.Error())
			return 1
		}
		response.Parsed = cleaned
	} else {
		response.Text = result.lastMessage
	}
	json.NewEncoder(os.Stdout).Encode(response)
	return 0
}

// execResult is what a codex exec --json run said.
type execResult struct {
	lastMessage string
	usage       msg.TokenUsage
}

// readExecEvents reads codex exec's JSONL events. The answer is the last
// agent_message: a turn can emit more than one. An item of type "error" is a
// warning (an ignored setting, code mode disabled) and does not fail the call;
// a top-level error or turn.failed does, and so does a run with no completed
// turn or no answer.
func readExecEvents(out []byte) (execResult, error) {
	var result execResult
	var failure string
	completed := false
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Usage struct {
				InputTokens           int `json:"input_tokens"`
				CachedInputTokens     int `json:"cached_input_tokens"`
				CacheWriteInputTokens int `json:"cache_write_input_tokens"`
				OutputTokens          int `json:"output_tokens"`
				ReasoningOutputTokens int `json:"reasoning_output_tokens"`
			} `json:"usage"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return result, fmt.Errorf("an event is not JSON: %v (%s)", err, truncateForError(line))
		}
		switch event.Type {
		case "item.completed":
			if event.Item.Type == "agent_message" {
				result.lastMessage = event.Item.Text
			}
		case "turn.completed":
			completed = true
			usage := event.Usage
			result.usage = msg.TokenUsage{
				InputTokens:      usage.InputTokens - usage.CachedInputTokens,
				CacheReadTokens:  usage.CachedInputTokens,
				CacheWriteTokens: usage.CacheWriteInputTokens,
				OutputTokens:     usage.OutputTokens,
				ReasoningTokens:  usage.ReasoningOutputTokens,
				TotalTokens:      usage.InputTokens + usage.OutputTokens,
			}
		case "turn.failed":
			failure = event.Error.Message
			if failure == "" {
				failure = event.Message
			}
		case "error":
			if failure == "" {
				failure = event.Message
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("read events: %w", err)
	}
	switch {
	case failure != "":
		return result, errors.New("the turn failed: " + truncateForError(failure))
	case !completed:
		return result, errors.New("the turn never completed")
	case result.lastMessage == "":
		return result, errors.New("the turn completed with no answer")
	}
	return result, nil
}

// strictSchema rewrites a JSON Schema into the form OpenAI's strict response
// format accepts: every object with properties sets additionalProperties
// false and lists every property as required, and a property the caller
// left optional may be null instead.
func strictSchema(schema any) any {
	switch typed := schema.(type) {
	case map[string]any:
		rewritten := map[string]any{}
		for key, value := range typed {
			rewritten[key] = strictSchema(value)
		}
		properties, hasProperties := rewritten["properties"].(map[string]any)
		if rewritten["type"] == "object" && hasProperties {
			required := map[string]bool{}
			if list, ok := typed["required"].([]any); ok {
				for _, name := range list {
					if text, ok := name.(string); ok {
						required[text] = true
					}
				}
			}
			names := make([]any, 0, len(properties))
			for name, property := range properties {
				names = append(names, name)
				if !required[name] {
					properties[name] = nullable(property)
				}
			}
			rewritten["required"] = names
			rewritten["additionalProperties"] = false
		}
		return rewritten
	case []any:
		rewritten := make([]any, len(typed))
		for index, value := range typed {
			rewritten[index] = strictSchema(value)
		}
		return rewritten
	}
	return schema
}

// nullable lets a property schema also accept null.
func nullable(property any) any {
	schema, ok := property.(map[string]any)
	if !ok {
		return property
	}
	switch kind := schema["type"].(type) {
	case string:
		schema["type"] = []any{kind, "null"}
	case []any:
		schema["type"] = append(kind, "null")
	}
	if values, ok := schema["enum"].([]any); ok {
		schema["enum"] = append(values, nil)
	}
	return schema
}

// dropNullOptionals removes, from an answer, every null the strict schema
// allowed in place of a property the caller's schema left optional.
func dropNullOptionals(answer any, schema any) any {
	schemaMap, _ := schema.(map[string]any)
	switch typed := answer.(type) {
	case map[string]any:
		properties, _ := schemaMap["properties"].(map[string]any)
		required := map[string]bool{}
		if list, ok := schemaMap["required"].([]any); ok {
			for _, name := range list {
				if text, ok := name.(string); ok {
					required[text] = true
				}
			}
		}
		for key, value := range typed {
			if value == nil && !required[key] {
				delete(typed, key)
				continue
			}
			typed[key] = dropNullOptionals(value, properties[key])
		}
		return typed
	case []any:
		for index, value := range typed {
			typed[index] = dropNullOptionals(value, schemaMap["items"])
		}
		return typed
	}
	return answer
}

// extractJSONObject returns the reply as raw JSON, tolerating one markdown
// code fence around the object. Prose, a second object or truncation is an
// error.
func extractJSONObject(text string) (json.RawMessage, error) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```json")
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "```"))
	}
	var probe json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(text))
	if err := decoder.Decode(&probe); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("trailing content after JSON object")
	}
	return probe, nil
}

func writeOneShotError(message string) {
	json.NewEncoder(os.Stdout).Encode(map[string]string{"error": message})
}

// truncateForError caps a blob quoted inside an error message.
func truncateForError(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > 600 {
		return text[:600] + "…"
	}
	return text
}
