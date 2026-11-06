package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func decodeJSON(t *testing.T, text string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestStrictSchemaRequiresEveryPropertyAndLetsOptionalOnesBeNull(t *testing.T) {
	schema := decodeJSON(t, `{"type":"object","properties":{"merges":{"type":"array","items":{"type":"object","properties":{
		"survivor_card_id":{"type":"string"},"category":{"type":"string","enum":["a","b"]},"card_ids":{"type":"array","items":{"type":"string"}}},
		"required":["card_ids"]}}},"required":["merges"]}`)
	strict := strictSchema(schema).(map[string]any)
	if strict["additionalProperties"] != false {
		t.Fatalf("top level: %v", strict)
	}
	item := strict["properties"].(map[string]any)["merges"].(map[string]any)["items"].(map[string]any)
	required := map[string]bool{}
	for _, name := range item["required"].([]any) {
		required[name.(string)] = true
	}
	properties := item["properties"].(map[string]any)
	if !required["survivor_card_id"] || !required["category"] || !required["card_ids"] || item["additionalProperties"] != false {
		t.Fatalf("item %v", item)
	}
	if !reflect.DeepEqual(properties["survivor_card_id"].(map[string]any)["type"], []any{"string", "null"}) {
		t.Fatalf("optional string: %v", properties["survivor_card_id"])
	}
	if !reflect.DeepEqual(properties["category"].(map[string]any)["enum"], []any{"a", "b", nil}) {
		t.Fatalf("optional enum: %v", properties["category"])
	}
	if properties["card_ids"].(map[string]any)["type"] != "array" {
		t.Fatalf("a required property must not become nullable: %v", properties["card_ids"])
	}
}

func TestNullsInOptionalPropertiesAreDroppedAndOthersKept(t *testing.T) {
	schema := decodeJSON(t, `{"type":"object","properties":{"merges":{"type":"array","items":{"type":"object","properties":{
		"survivor_card_id":{"type":"string"},"card_ids":{"type":"array","items":{"type":"string"}},"note":{"type":"string"}},
		"required":["card_ids","note"]}}},"required":["merges"]}`)
	answer := decodeJSON(t, `{"merges":[{"survivor_card_id":null,"card_ids":["a","b"],"note":null},{"survivor_card_id":"a","card_ids":["a"],"note":"x"}]}`)
	got, _ := json.Marshal(dropNullOptionals(answer, schema))
	want := `{"merges":[{"card_ids":["a","b"],"note":null},{"card_ids":["a"],"note":"x","survivor_card_id":"a"}]}`
	if string(got) != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestExecEventsGiveTheLastAnswerAndSplitUsage(t *testing.T) {
	events := `{"type":"thread.started"}
{"type":"item.completed","item":{"id":"item_0","type":"error","message":"Code Mode is unavailable"}}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"{\"answer\":\"draft\"}"}}
{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"{\"answer\":\"final\"}"}}
{"type":"turn.completed","usage":{"input_tokens":17015,"cached_input_tokens":7936,"cache_write_input_tokens":0,"output_tokens":326,"reasoning_output_tokens":148}}
`
	result, err := readExecEvents([]byte(events))
	if err != nil {
		t.Fatal(err)
	}
	want := msg.TokenUsage{InputTokens: 9079, CacheReadTokens: 7936, OutputTokens: 326, ReasoningTokens: 148, TotalTokens: 17341}
	if result.lastMessage != `{"answer":"final"}` || !reflect.DeepEqual(result.usage, want) {
		t.Fatalf("%q %+v", result.lastMessage, result.usage)
	}
	for name, events := range map[string]string{
		"failed turn":    `{"type":"turn.failed","error":{"message":"Invalid schema"}}`,
		"top-level":      `{"type":"error","message":"stream disconnected"}` + "\n" + `{"type":"turn.completed","usage":{}}`,
		"never finished": `{"type":"item.completed","item":{"type":"agent_message","text":"{}"}}`,
		"no answer":      `{"type":"turn.completed","usage":{}}`,
	} {
		if _, err := readExecEvents([]byte(events)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A fake codex records its arguments and stdin, then answers like the real
// one did when measured, with a null in an optional property.
const fakeCodex = `#!/bin/sh
printf '%s\n' "$@" > "$FAKE_CODEX_RECORD.args"
cat > "$FAKE_CODEX_RECORD.stdin"
echo '{"type":"thread.started"}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"{\"merges\":[{\"survivor_card_id\":null,\"card_ids\":[\"A\",\"B\"]}]}"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":6024,"cached_input_tokens":0,"output_tokens":115,"reasoning_output_tokens":45}}'
`

func TestOneShotRunsCodexWithNoToolsAndAnswersInTheCallersShape(t *testing.T) {
	directory := t.TempDir()
	fake := filepath.Join(directory, "codex")
	if err := os.WriteFile(fake, []byte(fakeCodex), 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(directory, "llm-bridge-codex")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	record := filepath.Join(directory, "record")
	request, _ := json.Marshal(msg.OneShotRequest{Prompt: "group these", SystemPrompt: `You "tidy" boards.`, Model: "gpt-5.6-luna",
		Schema: json.RawMessage(`{"type":"object","properties":{"merges":{"type":"array","items":{"type":"object","properties":{"survivor_card_id":{"type":"string"},"card_ids":{"type":"array","items":{"type":"string"}}},"required":["card_ids"]}}},"required":["merges"]}`)})
	command := exec.Command(binary, "-oneshot")
	command.Stdin = strings.NewReader(string(request))
	command.Env = []string{"HOME=" + directory, "PATH=" + os.Getenv("PATH"), "CODEX_PATH=" + fake, "FAKE_CODEX_RECORD=" + record}
	output, err := command.Output()
	if err != nil {
		t.Fatalf("oneshot: %v\n%s", err, output)
	}
	var response msg.OneShotResponse
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("response %s: %v", output, err)
	}
	if string(response.Parsed) != `{"merges":[{"card_ids":["A","B"]}]}` || response.Model != "gpt-5.6-luna" ||
		response.Usage.OutputTokens != 115 || response.Usage.ReasoningTokens != 45 {
		t.Fatalf("response %s", output)
	}
	argsBytes, _ := os.ReadFile(record + ".args")
	args := string(argsBytes)
	for _, want := range []string{"exec\n", "--ignore-user-config\n", "--ignore-rules\n", "--ephemeral\n", "read-only\n", "gpt-5.6-luna\n",
		"shell_tool\n", "code_mode_host\n", "web_search=\"disabled\"\n", `developer_instructions="You \"tidy\" boards."` + "\n", "--output-schema\n"} {
		if !strings.Contains(args, want) {
			t.Errorf("codex was not given %q; args:\n%s", want, args)
		}
	}
	if stdin, _ := os.ReadFile(record + ".stdin"); string(stdin) != "group these" {
		t.Errorf("prompt on stdin: %q", stdin)
	}
}

func TestOneShotRefusesAnAPIKeyAndAMissingModel(t *testing.T) {
	directory := t.TempDir()
	binary := filepath.Join(directory, "llm-bridge-codex")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	for name, check := range map[string]struct {
		env     []string
		request string
		says    string
	}{
		"api key":  {[]string{"OPENAI_API_KEY=sk-x"}, `{"prompt":"p","model":"m"}`, "OPENAI_API_KEY"},
		"no model": {nil, `{"prompt":"p"}`, "no model"},
	} {
		command := exec.Command(binary, "-oneshot")
		command.Stdin = strings.NewReader(check.request)
		command.Env = append([]string{"HOME=" + directory, "PATH=" + os.Getenv("PATH")}, check.env...)
		output, err := command.Output()
		if err == nil || !strings.Contains(string(output), check.says) {
			t.Errorf("%s: err %v output %s", name, err, output)
		}
	}
}

func TestAOneShotSendsEachImageToCodexAsAFile(t *testing.T) {
	directory := t.TempDir()
	fake := filepath.Join(directory, "codex")
	// The fake copies each -i file before the wrapper removes it.
	script := "#!/bin/sh\nn=0\nwhile [ $# -gt 0 ]; do if [ \"$1\" = -i ]; then cp \"$2\" \"$FAKE_CODEX_RECORD.image$n\"; echo \"$2\" >> \"$FAKE_CODEX_RECORD.paths\"; n=$((n+1)); fi; shift; done\ncat > /dev/null\n" +
		"echo '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"a cat\"}}'\necho '{\"type\":\"turn.completed\",\"usage\":{}}'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(directory, "llm-bridge-codex")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	record := filepath.Join(directory, "record")
	request, _ := json.Marshal(msg.OneShotRequest{Prompt: "what is this?", Model: "gpt-6-astra", Images: []msg.OneShotImage{
		{MediaType: "image/jpeg", DataBase64: "AAEC"}, {MediaType: "image/png", DataBase64: "AwQF"}}})
	command := exec.Command(binary, "-oneshot")
	command.Stdin = strings.NewReader(string(request))
	command.Env = []string{"HOME=" + directory, "PATH=" + os.Getenv("PATH"), "CODEX_PATH=" + fake, "FAKE_CODEX_RECORD=" + record}
	output, err := command.Output()
	if err != nil {
		t.Fatalf("oneshot: %v\n%s", err, output)
	}
	for index, want := range []string{"\x00\x01\x02", "\x03\x04\x05"} {
		got, err := os.ReadFile(fmt.Sprintf("%s.image%d", record, index))
		if err != nil || string(got) != want {
			t.Errorf("image %d reached codex as %q (%v), want %q", index, got, err, want)
		}
	}
	paths, _ := os.ReadFile(record + ".paths")
	lines := strings.Fields(string(paths))
	if len(lines) != 2 || !strings.HasSuffix(lines[0], ".jpg") || !strings.HasSuffix(lines[1], ".png") {
		t.Errorf("image paths %q, want a .jpg then a .png", lines)
	}
	for _, path := range lines {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("image file %s was left behind", path)
		}
	}
}

func TestAOneShotImageOfAnUnknownTypeIsRefused(t *testing.T) {
	directory := t.TempDir()
	fake := filepath.Join(directory, "codex")
	if err := os.WriteFile(fake, []byte(fakeCodex), 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(directory, "llm-bridge-codex")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	request, _ := json.Marshal(msg.OneShotRequest{Prompt: "who is this?", Model: "gpt-6-astra", Images: []msg.OneShotImage{{MediaType: "image/tiff", DataBase64: "AAEC"}}})
	command := exec.Command(binary, "-oneshot")
	command.Stdin = strings.NewReader(string(request))
	command.Env = []string{"HOME=" + directory, "PATH=" + os.Getenv("PATH"), "CODEX_PATH=" + fake, "FAKE_CODEX_RECORD=" + filepath.Join(directory, "record")}
	output, err := command.Output()
	if err == nil || !strings.Contains(string(output), "not one codex reads") {
		t.Errorf("a tiff answered %v %s, want a refusal", err, output)
	}
}
