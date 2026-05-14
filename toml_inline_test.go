package main

import (
	"encoding/json"
	"testing"
)

// TestJSONValueToInlineTOML covers the cases buildAppServerExtraArgs
// actually emits: a hooks tree with nested arrays + tables, strings
// containing quotes and special chars (the curl command), and integer
// timeout fields that mustn't get serialized as floats.
func TestJSONValueToInlineTOML(t *testing.T) {
	cases := []struct {
		name string
		in   string // JSON literal
		want string // expected inline TOML
	}{
		{"bool true", `true`, `true`},
		{"bool false", `false`, `false`},
		{"int", `42`, `42`},
		{"int from float-ish", `86400`, `86400`},
		{"plain string", `"hello"`, `"hello"`},
		{"string with quote", `"say \"hi\""`, `"say \"hi\""`},
		{"string with backslash", `"a\\b"`, `"a\\b"`},
		{"string with newline", `"line1\nline2"`, `"line1\nline2"`},
		{"empty array", `[]`, `[]`},
		{"int array", `[1, 2, 3]`, `[1, 2, 3]`},
		{"string array", `["a", "b"]`, `["a", "b"]`},
		{"empty object", `{}`, `{}`},
		{
			"object keys alphabetized",
			`{"b":1,"a":2}`,
			`{a = 2, b = 1}`,
		},
		{
			// 'P' (0x50) sorts before 'f' (0x66) — alphabetic in lexical
			// byte order, not case-insensitive. Bare keys allow [-A-Za-z0-9_];
			// the dot in "foo.bar" forces quoting.
			"quoted key when non-bare",
			`{"Pre-Tool":1,"foo.bar":2}`,
			`{Pre-Tool = 1, "foo.bar" = 2}`,
		},
		{
			"hook entry (mirrors what buildCodexHookConfig emits)",
			`{"matcher": ".*", "hooks": [{"type":"command","command":"curl http://x","timeout":86400}]}`,
			`{hooks = [{command = "curl http://x", timeout = 86400, type = "command"}], matcher = ".*"}`,
		},
		{
			"full PreToolUse array (the actual outer value)",
			`[{"matcher": ".*", "hooks": [{"type":"command","command":"echo","timeout":30}]}]`,
			`[{hooks = [{command = "echo", timeout = 30, type = "command"}], matcher = ".*"}]`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var v any
			if err := json.Unmarshal([]byte(tc.in), &v); err != nil {
				t.Fatalf("invalid test JSON %q: %v", tc.in, err)
			}
			got, err := jsonValueToInlineTOML(v)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("\nin:   %s\ngot:  %s\nwant: %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestJSONValueToInlineTOMLNullErrors locks in the "null isn't representable"
// contract so callers know to drop the key.
func TestJSONValueToInlineTOMLNullErrors(t *testing.T) {
	var v any
	_ = json.Unmarshal([]byte(`null`), &v)
	if _, err := jsonValueToInlineTOML(v); err == nil {
		t.Fatal("expected error for null, got nil")
	}
}
