package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// jsonValueToInlineTOML converts a JSON value to a single-line inline-TOML
// representation suitable for codex's `-c key=value` config overrides
// (`codex app-server -c hooks.PreToolUse='[...]'`).
//
// Codex's `-c` flag parses the value as TOML; if parsing fails, it falls
// back to a literal string. We want clean parses, so the output here
// strictly conforms to TOML 1.0 inline syntax:
//
//   - strings → "…" with backslash-escape for ", \, control chars
//   - numbers → decimal (int) or float
//   - bools   → true / false
//   - arrays  → [ a, b, c ]
//   - objects → { k = v, k = v }  (inline table)
//
// Keys are emitted in alphabetic order so the output is deterministic —
// important for test fixtures and equality-comparing CLI args.
//
// nil / null is not representable in TOML and returns an error; callers
// should drop the key entirely instead.
func jsonValueToInlineTOML(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", fmt.Errorf("TOML has no null; drop the key instead")
	case bool:
		if x {
			return "true", nil
		}
		return "false", nil
	case string:
		return tomlString(x), nil
	case float64:
		// json.Unmarshal puts every number into float64. Emit as int
		// when it has no fractional component so the TOML side gets the
		// expected type (timeout integers shouldn't become floats).
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x)), nil
		}
		return fmt.Sprintf("%g", x), nil
	case int:
		return fmt.Sprintf("%d", x), nil
	case int64:
		return fmt.Sprintf("%d", x), nil
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			s, err := jsonValueToInlineTOML(item)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			s, err := jsonValueToInlineTOML(x[k])
			if err != nil {
				return "", err
			}
			parts = append(parts, tomlKey(k)+" = "+s)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	case json.Number:
		return string(x), nil
	}
	return "", fmt.Errorf("unsupported JSON type %T for inline TOML", v)
}

// tomlString emits a TOML basic string with the minimum required escapes.
// Per TOML 1.0: backslash and double-quote must escape; control chars use
// \uXXXX. Everything else passes through, including UTF-8.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 || r == 0x7f {
				b.WriteString(fmt.Sprintf(`\u%04x`, r))
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// tomlKey emits a TOML key — bare if it matches [A-Za-z0-9_-]+, otherwise
// quoted. Codex's `-c` dotted-path is parsed before the value, so the
// outermost key on the command line is always bare (validated upstream).
// This helper handles inner table keys, which CAN contain mixed case
// (like "PreToolUse").
func tomlKey(k string) string {
	if k == "" {
		return `""`
	}
	for _, r := range k {
		if !(r >= 'A' && r <= 'Z') &&
			!(r >= 'a' && r <= 'z') &&
			!(r >= '0' && r <= '9') &&
			r != '_' && r != '-' {
			return tomlString(k)
		}
	}
	return k
}
