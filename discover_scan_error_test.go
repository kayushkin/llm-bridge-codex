package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexUserLineOfExactly builds a well-formed Codex user response_item of
// exactly n bytes.
//
// The envelope is spelled out rather than marshalled so the length arithmetic
// is visible: a fixture that is approximately the ceiling is worth nothing,
// because the point is to straddle it by one byte.
func codexUserLineOfExactly(t *testing.T, n int) string {
	t.Helper()
	const prefix = `{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"`
	const suffix = `"}]}}`
	pad := n - len(prefix) - len(suffix)
	if pad < 1 {
		t.Fatalf("cannot build a %d-byte line: the envelope alone is %d bytes", n, len(prefix)+len(suffix))
	}
	line := prefix + strings.Repeat("a", pad) + suffix
	if len(line) != n {
		t.Fatalf("built a %d-byte line, want %d", len(line), n)
	}
	return line
}

const (
	codexSessionMetaLine = `{"type":"session_meta","payload":{"id":"sess-194","timestamp":"2026-04-30T00:00:00Z","cwd":"/tmp/project-194"}}`
	codexShortUserLine   = `{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"hello"}]}}`
)

// TestParseCodexSessionReadsTheLongestLineItsCeilingAllows straddles the
// scanner's line ceiling by one byte on each side.
//
// codex had no test on this boundary before, which is how a 1 MB cap can be
// moved to 300 KB with a green suite. Both literals are written out rather than
// derived from the production expression, so a mutation that moves the ceiling
// cannot move the fixture with it.
//
// The numbers are measured against bufio with codex's own buffer shape
// (make([]byte, 0, 256*1024), 1024*1024), not carried over from another repo.
// The zero-length starting slice does not move the boundary — it only sets how
// much is allocated before the buffer grows.
func TestParseCodexSessionReadsTheLongestLineItsCeilingAllows(t *testing.T) {
	const (
		longestAccepted = 1048575 // one below the shipped 1 MB ceiling
		firstRejected   = 1048576 // exactly the ceiling, which bufio refuses
	)

	for _, tc := range []struct {
		name      string
		lineBytes int
		wantTurns int
	}{
		{"one byte under the ceiling is read", longestAccepted, 1},
		{"a line at the ceiling is dropped", firstRejected, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollout.jsonl")
			body := codexSessionMetaLine + "\n" + codexUserLineOfExactly(t, tc.lineBytes) + "\n"
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatalf("write fixture: %v", err)
			}

			var id, prompt, cwd string
			var turns int
			// The over-ceiling case reports; keep it off the test output.
			captureLog(t, func() {
				id, prompt, _, cwd, turns = parseCodexSession(path)
			})

			// session_meta is the first line in both cases, so it is read either
			// way. This is what makes the turn count below the only difference.
			if id != "sess-194" || cwd != "/tmp/project-194" {
				t.Fatalf("id = %q, cwd = %q: session_meta is the first line and under the ceiling in both cases", id, cwd)
			}
			if turns != tc.wantTurns {
				t.Fatalf("turns = %d, want %d for a %d-byte line", turns, tc.wantTurns, tc.lineBytes)
			}
			if tc.wantTurns == 0 {
				if prompt != "" {
					t.Errorf("prompt = %q, want empty: the line was over the ceiling and never parsed", prompt)
				}
				return
			}
			if prompt == "" {
				t.Errorf("prompt is empty for a %d-byte line, which is within the ceiling", tc.lineBytes)
			}
		})
	}
}

// TestParseCodexSessionReportsAnOverLongLineInsteadOfDroppingItInSilence pins
// whether anything says a scan was cut short, as distinct from what was cut.
//
// codex loses more than claudecode and jig do at this ceiling. Those two return
// (prompt, ts, turns); this also returns id and cwd off the session_meta line,
// so WHERE the over-long line falls changes what is lost:
//
//	over-long line after session_meta    id and cwd survive, turn count is short
//	over-long line before session_meta   id and cwd are lost as well
//
// id has a fallback in coldImportRollouts (extractIDFromFilename). cwd has
// none — out.Project is simply left empty, which is why the report names both.
func TestParseCodexSessionReportsAnOverLongLineInsteadOfDroppingItInSilence(t *testing.T) {
	const (
		overCeiling = 1048576 // exactly the shipped cap, which bufio refuses
		turnsAfter  = 3
	)

	write := func(t *testing.T, lines ...string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		return path
	}

	shortTurns := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = codexShortUserLine
		}
		return out
	}

	// The control comes first and carries most of the weight. Without it, a
	// mutation that logs on every call — or one that reports a truncation that
	// never happened — passes the cases below and is never seen.
	t.Run("a file with no over-long line reads everything and says nothing", func(t *testing.T) {
		lines := append([]string{codexSessionMetaLine}, shortTurns(1+turnsAfter)...)
		path := write(t, lines...)

		var id, cwd string
		var turns int
		out := captureLog(t, func() {
			id, _, _, cwd, turns = parseCodexSession(path)
		})

		if turns != 1+turnsAfter {
			t.Errorf("turns = %d, want %d: every line in this fixture is under the ceiling", turns, 1+turnsAfter)
		}
		if id != "sess-194" || cwd != "/tmp/project-194" {
			t.Errorf("id = %q, cwd = %q: both are on the first line and under the ceiling", id, cwd)
		}
		if out != "" {
			t.Errorf("nothing was truncated, so nothing should have been logged; got:\n%s", out)
		}
	})

	t.Run("after session_meta: id and cwd survive, the turn count does not", func(t *testing.T) {
		lines := append([]string{codexSessionMetaLine, codexShortUserLine, codexUserLineOfExactly(t, overCeiling)},
			shortTurns(turnsAfter)...)
		path := write(t, lines...)

		var id, cwd string
		var turns int
		out := captureLog(t, func() {
			id, _, _, cwd, turns = parseCodexSession(path)
		})

		if id != "sess-194" {
			t.Errorf("id = %q, want sess-194: session_meta is before the over-long line and was read", id)
		}
		if cwd != "/tmp/project-194" {
			t.Errorf("cwd = %q, want /tmp/project-194: session_meta is before the over-long line and was read", cwd)
		}
		if turns != 1 {
			t.Errorf("turns = %d, want 1: the scan stops at the over-long line, so the %d turns after it are unread",
				turns, turnsAfter)
		}
		assertReportNames(t, out, path, "1")
	})

	t.Run("before session_meta: the session loses its id and its project too", func(t *testing.T) {
		lines := append([]string{codexUserLineOfExactly(t, overCeiling), codexSessionMetaLine},
			shortTurns(turnsAfter)...)
		path := write(t, lines...)

		var id, cwd string
		var turns int
		out := captureLog(t, func() {
			id, _, _, cwd, turns = parseCodexSession(path)
		})

		// This is the codex-only half. The scan dies before session_meta, so the
		// two fields that only session_meta carries come back empty.
		if id != "" {
			t.Errorf("id = %q, want empty: the scan stopped before session_meta was reached", id)
		}
		if cwd != "" {
			t.Errorf("cwd = %q, want empty: the scan stopped before session_meta was reached", cwd)
		}
		if turns != 0 {
			t.Errorf("turns = %d, want 0: the over-long line is first, so nothing after it is read", turns)
		}
		assertReportNames(t, out, path, "0")
		// The blank Project is the consequence with no fallback anywhere, so the
		// report has to be explicit about it rather than leaving the reader to
		// infer it from an absent field.
		if !strings.Contains(out, "cwd is empty") {
			t.Errorf("the report does not say cwd was lost, so a blank Project has nothing to be traced to.\ngot:\n%s", out)
		}
		if !strings.Contains(out, "session_meta was never reached") {
			t.Errorf("the report does not say session_meta was missed, so the id fallback looks like a real id.\ngot:\n%s", out)
		}
	})
}

// assertReportNames checks the parts of the report every placement shares: the
// file it came from, why the scan stopped, and the partial turn count.
func assertReportNames(t *testing.T, out, path, wantTurns string) {
	t.Helper()
	if out == "" {
		t.Fatalf("the scan stopped early and nothing was logged: the drop is silent again, " +
			"which is the whole defect this test pins")
	}
	// Name the file: a report that cannot be traced to a session is not
	// actionable, and a bare "scan error" line is indistinguishable between the
	// hundreds of rollouts a cold import walks.
	if !strings.Contains(out, path) {
		t.Errorf("the report does not name the rollout file, so it cannot be traced back to one.\nwant substring: %s\ngot:\n%s", path, out)
	}
	if !strings.Contains(out, "too long") {
		t.Errorf("the report does not say why the scan stopped.\ngot:\n%s", out)
	}
	// The partial count has to appear, otherwise the reader is told a scan broke
	// but not that the number alongside it is short.
	if !strings.Contains(out, "first "+wantTurns+" turn") {
		t.Errorf("the report does not carry the partial turn count (%s), so nothing connects it to the wrong number.\ngot:\n%s", wantTurns, out)
	}
}
