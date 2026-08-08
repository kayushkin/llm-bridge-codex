package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// The defect these tests pin: cutting a Go string at a fixed byte offset splits
// whatever rune straddles that offset, and the result is not valid UTF-8.
// encoding/json substitutes U+FFFD rather than failing, so nothing along the
// path reports it.
//
// A single hand-picked input is not enough. Whether a cut splits a rune depends
// on where that rune happens to sit relative to the budget, so one string lands
// off the boundary and passes against the unfixed code — which is how this
// survived across the fleet. Every test here slides the cut across a range and
// states how much of that range actually exercised the case.

// musicalNote is four bytes (U+1D11E). A two-byte rune is a weaker probe: only
// one of its two interior offsets is wrong, so a test that picks the other one
// is green against the bug.
const musicalNote = "\U0001D11E"

// TestTruncateAtRuneBoundarySlidesTheCutAcrossEveryOffset walks maxBytes across
// the whole length of an all-four-byte string. Three offsets in every four
// straddle a rune; the fourth is boundary-aligned and is the known-negative
// control, which must come back untouched against fixed and unfixed code alike.
func TestTruncateAtRuneBoundarySlidesTheCutAcrossEveryOffset(t *testing.T) {
	s := strings.Repeat(musicalNote, 50) // 200 bytes, the caller's real budget
	straddled, aligned := 0, 0

	for maxBytes := 1; maxBytes <= len(s); maxBytes++ {
		got := truncateAtRuneBoundary(s, maxBytes)

		if !utf8.ValidString(got) {
			t.Fatalf("maxBytes=%d: result is not valid UTF-8: %q", maxBytes, got)
		}
		if len(got) > maxBytes {
			t.Fatalf("maxBytes=%d: result is %d bytes, over budget", maxBytes, len(got))
		}
		// Validity alone is not falsifiable — a helper that returns "" for
		// everything is always valid and always within budget. Pin maximality
		// too: whatever was dropped must genuinely not have fitted.
		if len(got) < len(s) {
			_, width := utf8.DecodeRuneInString(s[len(got):])
			if len(got)+width <= maxBytes {
				t.Fatalf("maxBytes=%d: stopped at %d bytes but the next rune (%d bytes) still fits",
					maxBytes, len(got), width)
			}
		}

		if maxBytes%4 == 0 {
			aligned++
			if got != s[:maxBytes] {
				t.Fatalf("maxBytes=%d is rune-aligned, so the cut is a no-op; result changed", maxBytes)
			}
		} else {
			straddled++
		}
	}

	// The slide is a claim about coverage, so assert it. An earlier version of
	// this loop in the reference repo moved the input rather than the cut,
	// never straddled once, and was green against the unfixed helper.
	if straddled == 0 {
		t.Fatal("the cut never landed inside a rune: this test proves nothing")
	}
	if aligned == 0 {
		t.Fatal("the known-negative control never ran: cannot tell a real catch from a false one")
	}
	t.Logf("cut straddled a rune at %d of %d offsets (%d aligned controls)",
		straddled, straddled+aligned, aligned)
}

// TestTruncateAtRuneBoundaryMixedWidths repeats the slide over a string mixing
// one-, two-, three- and four-byte runes, so the result does not depend on every
// rune being the same width.
func TestTruncateAtRuneBoundaryMixedWidths(t *testing.T) {
	s := strings.Repeat("aé€"+musicalNote, 25) // 1+2+3+4 bytes per group
	straddled := 0

	for maxBytes := 1; maxBytes <= len(s); maxBytes++ {
		got := truncateAtRuneBoundary(s, maxBytes)

		if !utf8.ValidString(got) {
			t.Fatalf("maxBytes=%d: result is not valid UTF-8: %q", maxBytes, got)
		}
		if len(got) > maxBytes {
			t.Fatalf("maxBytes=%d: result is %d bytes, over budget", maxBytes, len(got))
		}
		if len(got) < len(s) {
			_, width := utf8.DecodeRuneInString(s[len(got):])
			if len(got)+width <= maxBytes {
				t.Fatalf("maxBytes=%d: stopped at %d bytes but the next rune (%d bytes) still fits",
					maxBytes, len(got), width)
			}
		}
		if len(got) < maxBytes {
			straddled++
		}
	}

	if straddled == 0 {
		t.Fatal("the cut never landed inside a rune: this test proves nothing")
	}
	t.Logf("cut straddled a rune at %d of %d offsets", straddled, len(s))
}

func TestTruncateAtRuneBoundaryEdgeCases(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxBytes int
		want     string
	}{
		// ASCII is the other known-negative control: the whole class is a no-op
		// on it, so these must hold against the unfixed helper too.
		{"ascii under budget", "hello", 200, "hello"},
		{"ascii exactly at budget", "hello", 5, "hello"},
		{"ascii over budget cuts plainly", "hello", 3, "hel"},
		{"empty input", "", 200, ""},
		{"zero budget", "hello", 0, ""},
		{"negative budget", "hello", -1, ""},
		// One four-byte rune against a budget too small to hold it: the only
		// rune-safe answer is the empty string, not a lone lead byte.
		{"budget smaller than the first rune", musicalNote, 3, ""},
		{"budget exactly the first rune", musicalNote, 4, musicalNote},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := truncateAtRuneBoundary(c.in, c.maxBytes); got != c.want {
				t.Fatalf("truncateAtRuneBoundary(%q, %d) = %q, want %q",
					c.in, c.maxBytes, got, c.want)
			}
		})
	}
}

// TestDiscoveredPromptStaysValidUTF8 is the call-site test. The helper being
// correct does not prove the caller uses it, and the caller is what makes this
// tier 1 rather than cosmetic: parseCodexSession's prompt becomes
// msg.StoredSession.Prompt (discover.go, buildStoredSession), which main.go
// encodes to stdout as JSON for bridge-server, which serves it to the session
// list. A split rune survives the request and a reload does not fix it.
//
// The prompt slides so that a four-byte rune lands on each of the four byte
// offsets around the 200-byte budget, and the assertion is made on the JSON
// bridge-server actually receives.
func TestDiscoveredPromptStaysValidUTF8(t *testing.T) {
	for pad := 197; pad <= 200; pad++ {
		t.Run(fmt.Sprintf("pad=%d", pad), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)

			sessionsDir := filepath.Join(home, ".codex", "sessions", "2026", "04", "30")
			if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
				t.Fatalf("mkdir sessionsDir: %v", err)
			}

			prompt := strings.Repeat("a", pad) + musicalNote + strings.Repeat("b", 50)
			writeRolloutFileWithPrompt(t, sessionsDir, "hsid-utf8", prompt)

			got, err := discoverSessions()
			if err != nil {
				t.Fatalf("discoverSessions: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("want 1 cold-imported session, got %d", len(got))
			}

			if !utf8.ValidString(got[0].Prompt) {
				t.Fatalf("discovered prompt is not valid UTF-8: %q", got[0].Prompt)
			}
			if len(got[0].Prompt) > 200 {
				t.Fatalf("prompt is %d bytes, over the 200-byte budget", len(got[0].Prompt))
			}
			// The byte cut does not fail here, it corrupts silently: json.Marshal
			// swaps the split rune for U+FFFD and reports no error. Assert on the
			// encoded form so the test sees what bridge-server sees.
			encoded, err := json.Marshal(got[0].Prompt)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(encoded), "�") {
				t.Fatalf("prompt reached JSON carrying U+FFFD: %s", encoded)
			}
		})
	}
}

// writeRolloutFileWithPrompt is writeRolloutFile with the user text under test
// instead of a fixed "hello". Kept separate from discover_test.go's helper so
// this file stands on its own.
func writeRolloutFileWithPrompt(t *testing.T, dir, harnessID, prompt string) string {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("rollout-%s.jsonl", harnessID))
	userLine, err := json.Marshal(map[string]any{
		"type": "response_item",
		"payload": map[string]any{
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": prompt}},
		},
	})
	if err != nil {
		t.Fatalf("marshal user line: %v", err)
	}
	body := strings.Join([]string{
		fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"timestamp":"2026-04-30T00:00:00Z","cwd":"/tmp/test"}}`, harnessID),
		string(userLine),
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestTruncateWithEllipsisSlidesTheCutAcrossEveryOffset covers the second cut in
// this repo: translate.go's helper, which prehook_proxy.go uses for its two log
// lines. It is the same walk-back with "..." appended, so the ellipsis has to be
// stripped before the result can be judged — and the assertion that it is there
// at all is what keeps a helper that silently returns the input from passing.
//
// Three offsets in every four straddle a rune; the fourth is rune-aligned and is
// the known-negative control, which must be a plain byte cut against fixed and
// unfixed code alike.
func TestTruncateWithEllipsisSlidesTheCutAcrossEveryOffset(t *testing.T) {
	s := strings.Repeat(musicalNote, 60) // 240 bytes, past the 200-byte budget
	straddled, aligned := 0, 0

	for maxBytes := 1; maxBytes < len(s); maxBytes++ {
		got := truncateAtRuneBoundaryWithEllipsis(s, maxBytes)

		body := strings.TrimSuffix(got, "...")
		if body == got {
			t.Fatalf("maxBytes=%d: input is over budget, so the result must carry the ellipsis; got %q", maxBytes, got)
		}
		if !utf8.ValidString(body) {
			t.Fatalf("maxBytes=%d: kept text is not valid UTF-8: %q", maxBytes, body)
		}
		if len(body) > maxBytes {
			t.Fatalf("maxBytes=%d: kept %d bytes, over budget", maxBytes, len(body))
		}
		// Maximality, without which a helper that keeps nothing passes: valid
		// UTF-8, inside budget, and useless.
		_, width := utf8.DecodeRuneInString(s[len(body):])
		if len(body)+width <= maxBytes {
			t.Fatalf("maxBytes=%d: stopped at %d bytes but the next rune (%d bytes) still fits",
				maxBytes, len(body), width)
		}

		if maxBytes%4 == 0 {
			aligned++
			if body != s[:maxBytes] {
				t.Fatalf("maxBytes=%d is rune-aligned, so the walk-back must be a no-op; kept %d bytes", maxBytes, len(body))
			}
			continue
		}
		straddled++
		if body == s[:maxBytes] {
			t.Fatalf("maxBytes=%d straddles a rune, so the cut must move; it did not", maxBytes)
		}
	}

	if straddled == 0 || aligned == 0 {
		t.Fatalf("fixture covered %d straddling and %d aligned offsets; both must be non-zero", straddled, aligned)
	}
}

// TestTruncateWithEllipsisEdgeCases pins the branches the sliding loop cannot
// reach: input within budget must come back untouched and WITHOUT an ellipsis,
// and a budget of zero or less keeps no text. The byte cut this replaced
// panicked on a negative budget — in source that is the same expression as the
// split rune, so no scan for one can see the other.
func TestTruncateWithEllipsisEdgeCases(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxBytes int
		want     string
	}{
		{"ascii under budget keeps no ellipsis", "hello", 200, "hello"},
		{"ascii exactly at budget keeps no ellipsis", "hello", 5, "hello"},
		{"ascii over budget cuts plainly", "hello", 3, "hel..."},
		{"empty input", "", 200, ""},
		{"zero budget", "hello", 0, "..."},
		{"negative budget", "hello", -1, "..."},
		{"budget smaller than the first rune", musicalNote, 3, "..."},
		{"budget exactly the first rune", musicalNote, 4, musicalNote},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := truncateAtRuneBoundaryWithEllipsis(c.in, c.maxBytes); got != c.want {
				t.Fatalf("truncateAtRuneBoundaryWithEllipsis(%q, %d) = %q, want %q",
					c.in, c.maxBytes, got, c.want)
			}
		})
	}
}
