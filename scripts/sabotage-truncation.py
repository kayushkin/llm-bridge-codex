"""Sabotage-score the rune-boundary truncation tests.

A suite that passes tells you nothing on its own. This breaks the fix in six
ways and checks that the tests notice five of them and, just as importantly, do
NOT notice the sixth. A scorer with no known-negative control reports CAUGHT for
everything and looks perfect while measuring nothing.

Three things this harness is careful about, all learned the expensive way by
earlier passes of this sweep:

  * Mutations are written as drifted comparisons or substitutions that keep every
    identifier live. Deleting the walk-back orphans the `utf8` import, and
    `go test` runs vet, so the case would report a compile error instead of a
    score -- and a compile error hides whether any test would have caught the
    behaviour.

  * CAUGHT is split into assertion-fired and guard-fired. A test can go red
    because its own fixture blew up (t.Fatalf on a setup step) rather than
    because it detected the defect. That is not coverage, and counting it as
    coverage inflates the score.

  * classify() is exercised against every verdict it can return, including the
    two panic verdicts no row in this table produces. A table cannot score its
    own scorer: the forty-eighth pass shipped a scorer whose panic branch was
    unreachable and read 5/5 with it dead, because nothing it mutated panicked.

Run from anywhere:  python3 scripts/sabotage-truncation.py
"""

import os
import pathlib
import re
import signal
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent

WALKBACK = "for cut > 0 && !utf8.RuneStart(s[cut]) {"
# Six tabs: the call site sits inside for/if/for/if in parseCodexSession.
CALLSITE = "\t\t\t\t\t\tprompt = truncateAtRuneBoundary(c.Text, 200)"
BYTECUT = ("\t\t\t\t\t\tif len(c.Text) > 200 {\n"
           "\t\t\t\t\t\t\tprompt = c.Text[:200]\n"
           "\t\t\t\t\t\t} else {\n"
           "\t\t\t\t\t\t\tprompt = c.Text\n"
           "\t\t\t\t\t\t}")

# The ellipsis helper's two call sites, both prehook-proxy log lines. Written
# without surrounding indentation because each is an argument mid-line, not a
# statement, and each appears exactly once in the file -- which the SETUP FAIL
# check below asserts on every run rather than trusting.
BODYCALL = "truncateAtRuneBoundaryWithEllipsis(string(respBody), 200)"
REASONCALL = "truncateAtRuneBoundaryWithEllipsis(reason, 80)"

CASES = [
    # (label, file, old, new, expect_caught)
    ("the walk-back never runs, so the cut splits a rune again",
     "discover.go", WALKBACK, "for cut > len(s) && !utf8.RuneStart(s[cut]) {", True),
    ("the helper trims to nothing instead of to the boundary",
     "discover.go", "\treturn s[:cut]", "\treturn s[:cut*0]", True),
    ("the walk-back stops one byte short, leaving the rune split",
     "discover.go", "\treturn s[:cut]", "\treturn s[:cut+1]", True),
    ("the empty-budget guard stops rejecting a budget of zero",
     "discover.go", "if maxBytes <= 0 {", "if maxBytes < -1 {", True),
    # The call site, not the helper. The helper being correct does not prove the
    # caller uses it, and this is the mutation that says so. It is also the only
    # row TestDiscoveredPromptStaysValidUTF8 can catch alone.
    ("only the call site reverts to a plain byte cut",
     "discover.go", CALLSITE, BYTECUT, True),
    # Known-NEGATIVE control. maxBytes==0 already returns "" via the s[:cut]
    # path with cut==0, so narrowing this guard is a behavioural no-op. A harness
    # that reports CAUGHT here is reporting CAUGHT for everything.
    ("CONTROL (no-op): the maxBytes guard narrows from <=0 to <0",
     "discover.go", "if maxBytes <= 0 {", "if maxBytes < 0 {", False),

    # ---- boundary VALUES, ported from llm-bridge-claudecode by the 187th ----
    # Everything above moves a MECHANISM: the walk-back, the slice, the call
    # site. None of it moves a number by one. A suite can hold every mechanism
    # in this file and still let each boundary drift a byte, because a rune
    # sliding across a fixed cut never asks what the cut is. The three repos
    # share this helper byte-for-byte; they do NOT share the fixtures, so the
    # rows are re-measured here rather than assumed to score alike.
    ("the budget-of-one boundary: the empty-budget guard swallows maxBytes==1",
     "discover.go", "if maxBytes <= 0 {", "if maxBytes <= 1 {", True),
    ("the exactly-at-budget boundary: a string the length of the budget is cut",
     "discover.go", "if len(s) <= maxBytes {", "if len(s) < maxBytes {", True),
    ("the over-budget boundary: one byte more than the budget is returned whole",
     "discover.go", "if len(s) <= maxBytes {", "if len(s) <= maxBytes+1 {", True),
    ("the cut starts one byte below the budget",
     "discover.go", "\tcut := maxBytes", "\tcut := maxBytes - 1", True),
    ("the walk-back's own loop bound stops at 1, keeping a lone lead byte",
     "discover.go", WALKBACK, "for cut > 1 && !utf8.RuneStart(s[cut]) {", True),
    # The call site's 200 is the value that SHIPS. Every helper test supplies
    # its own budget, so none of them is a claim about this number.
    ("the shipped label budget shrinks by one byte, 200 -> 199",
     "discover.go", CALLSITE, CALLSITE.replace("200", "199"), True),
    ("the shipped label budget grows by one byte, 200 -> 201",
     "discover.go", CALLSITE, CALLSITE.replace("200", "201"), True),

    # ---- translate.go's ellipsis cut, scored for the first time -------------
    # This repo carries a SECOND truncation helper, tested since the day it
    # landed and never mutated by anything. Two tests passing is not a score,
    # and a target absent from the case list is indistinguishable from a target
    # with no tests at all — the aggregate line reads the same either way.
    ("ellipsis cut: a string exactly the length of the budget gains an ellipsis",
     "translate.go", "if len(s) <= maxBytes {", "if len(s) < maxBytes {", True),
    ("ellipsis cut: one byte over the budget is returned whole and unmarked",
     "translate.go", "if len(s) <= maxBytes {", "if len(s) <= maxBytes+1 {", True),

    # ---- the ellipsis cut's CALL SITES, scored for the first time -----------
    # The two rows above score the helper. The helper scoring 15/15 is not a
    # claim about the numbers its callers hand it: every test above supplies its
    # own budget, so none of them is evidence for the 200 and the 80 that ship.
    # Before prehook_proxy_test.go existed, gateViaPrehook had no test of any
    # kind -- not a suite that failed to separate the budgets, no suite at all --
    # and all four drift rows below read UNNOTICED.
    ("the shipped HTTP-error-body budget shrinks by one byte, 200 -> 199",
     "prehook_proxy.go", BODYCALL, BODYCALL.replace("200", "199"), True),
    ("the shipped HTTP-error-body budget grows by one byte, 200 -> 201",
     "prehook_proxy.go", BODYCALL, BODYCALL.replace("200", "201"), True),
    ("the shipped decision-reason budget shrinks by one byte, 80 -> 79",
     "prehook_proxy.go", REASONCALL, REASONCALL.replace("80", "79"), True),
    ("the shipped decision-reason budget grows by one byte, 80 -> 81",
     "prehook_proxy.go", REASONCALL, REASONCALL.replace("80", "81"), True),
    # A budget that drifts is one failure; a call site that stops cutting at all
    # is the other, and a drift row cannot see it -- there is no number left to
    # move. Same reason discover.go carries "only the call site reverts to a
    # plain byte cut" alongside its two budget rows.
    ("the HTTP-error-body call site stops truncating and logs the whole body",
     "prehook_proxy.go", BODYCALL, "string(respBody)", True),
    ("the decision-reason call site stops truncating and logs the whole reason",
     "prehook_proxy.go", REASONCALL, "reason", True),
]

TESTS = ("TestTruncateAtRuneBoundarySlidesTheCutAcrossEveryOffset|"
         "TestTruncateAtRuneBoundaryMixedWidths|"
         "TestTruncateAtRuneBoundaryEdgeCases|"
         "TestDiscoveredPromptStaysValidUTF8|"
         "TestDiscoveredLabelKeepsExactlyTheShippedBudget|"
         "TestTruncateWithEllipsisSlidesTheCutAcrossEveryOffset|"
         "TestTruncateWithEllipsisEdgeCases|"
         "TestPrehookErrorBodyLogKeepsExactlyTheShippedBudget|"
         "TestPrehookDecisionReasonLogKeepsExactlyTheShippedBudget")

# Messages from fixture guards rather than from an assertion about truncation.
# A red run that shows only these is the test falling over, not detecting.
GUARD_MARKERS = (
    "discoverSessions:",
    "mkdir sessionsDir:",
    "want 1 cold-imported session",
    "marshal:",
    "marshal user line:",
    "the cut never landed inside a rune",
    "the known-negative control never ran",
    # prehook_proxy_test.go's two fixture guards: the markers not landing on the
    # boundary, and the log line the payload is lifted out of not being found.
    # Neither is a statement about truncation.
    "fixture is malformed",
    "prehook log line not found",
)

# Every test file whose failures count as assertions. This was the bare literal
# "truncate_test.go" while the case list mutated one file; a second test file's
# failures matched nothing, so classify() would have read every real detection
# from it as "CAUGHT (no assertion text -- NOT coverage)" -- a verdict that does
# NOT count toward the score. The four prehook drift rows would then have gone
# from UNNOTICED to still-failing, and the tests written to close them would have
# looked like they had not worked.
FAIL_LINE = re.compile(r"^\s*(?:truncate_test|prehook_proxy_test)\.go:\d+: (.*)$", re.M)
# A stack frame naming a .go file, e.g. "\t/home/u/repo/discover.go:388". The
# full path is captured because the Go runtime's own frames (runtime/panic.go)
# would otherwise pass a bare-filename filter and be mistaken for our source.
FRAME = re.compile(r"^\s+(/\S+\.go):(\d+)", re.M)


def classify(output):
    """Return (verdict, detail) for one sabotage run's output."""
    if "[build failed]" in output or "declared and not used" in output:
        return "COMPILE ERROR", "mutation orphaned an identifier -- rewrite it"
    if "panic:" in output:
        # Read WHERE the panic is, not just that there is one. A mutation that
        # crashes production code the test drove it into IS detection -- the
        # program died instead of returning a wrong answer, and the test names
        # the input that did it. A panic in the fixture is the test falling
        # over before it asserted anything, which is not coverage.
        #
        # Pick the first non-test frame rather than requiring that no test frame
        # is present: a panic inside production code always has the calling test
        # frame below it, so an "and no _test.go frame" clause is unreachable and
        # would file every real crash as fixture damage.
        frames = [f for f, _ in FRAME.findall(output.split("panic:", 1)[1])
                  if f.startswith(str(REPO) + "/")]
        source = next((f for f in frames if not f.endswith("_test.go")), None)
        if source:
            return ("CAUGHT (panic in %s)" % pathlib.Path(source).name,
                    "test drove the mutation into a crash")
        return "CAUGHT (panic in fixture -- NOT coverage)", "the test fell over before asserting"
    if "--- FAIL" not in output:
        return "UNNOTICED", ""
    messages = FAIL_LINE.findall(output)
    if not messages:
        return "CAUGHT (no assertion text -- NOT coverage)", ""
    guard = [m for m in messages if any(g in m for g in GUARD_MARKERS)]
    real = [m for m in messages if m not in guard]
    if not real:
        return "CAUGHT (guard only -- NOT coverage)", guard[0][:90]
    return "CAUGHT", real[0][:90]


def self_test():
    """Exercise classify() directly against every verdict it can return.

    The forty-third pass's rule: when you automate a check, the check is the next
    unmeasured claim. A classify() that can never return "guard only" prints the
    clean score you were hoping for. Rather than trust that these branches are
    reachable, drive all six from synthetic output -- including the two panic
    verdicts that no row in CASES produces, which is exactly why sabotaging
    toward them is not available here.
    """
    probes = [
        ("--- FAIL: X\n    truncate_test.go:40: result is not valid UTF-8\n", "CAUGHT"),
        ("--- FAIL: X\n    truncate_test.go:99: want 1 cold-imported session, got 0\n",
         "CAUGHT (guard only -- NOT coverage)"),
        ("ok  \tgithub.com/x\n", "UNNOTICED"),
        # Both panic branches, distinguished only by which file the top repo
        # frame names. The runtime's own panic.go frame is present in each and
        # must not be mistaken for our source. The production-code probe carries
        # a test frame too, because a real one always does.
        ("panic: slice bounds out of range\n"
         "\t/usr/lib/go/src/runtime/panic.go:860 +0x13a\n"
         "\t%s/discover.go:388\n"
         "\t%s/truncate_test.go:41\n" % (REPO, REPO), "CAUGHT (panic in discover.go)"),
        ("panic: slice bounds out of range\n"
         "\t/usr/lib/go/src/runtime/panic.go:860 +0x13a\n"
         "\t%s/truncate_test.go:149\n" % REPO,
         "CAUGHT (panic in fixture -- NOT coverage)"),
        ("# github.com/x [build failed]\n", "COMPILE ERROR"),
    ]
    ok = True
    for output, want in probes:
        got, _ = classify(output)
        if got != want:
            print(f"  classifier SELF-TEST FAIL: got {got!r}, want {want!r}")
            ok = False
    print(f"  classifier self-test: {'all 6 verdicts reachable' if ok else 'BROKEN'}")
    return ok


# Every file any row in CASES mutates. This used to be the literal
# "discover.go", while CASES has carried a per-row file since it was written --
# so the file field was a promise the restore did not keep. A row naming a
# second file would have left that file mutated for every case after it AND
# after the run, which is the exact failure the signal handlers below exist to
# prevent, reachable without any signal at all. Derived from the table so it
# cannot drift from it again.
MUTATED_FILES = sorted({fname for _, fname, _, _, _ in CASES})


def restore():
    subprocess.run(["git", "checkout", "--"] + MUTATED_FILES, cwd=REPO, check=True)


print("Sabotaging the rune-boundary truncation fix in llm-bridge-codex\n")
if not self_test():
    sys.exit(2)
print()

score = 0
# The file under test holds a deliberately broken version of itself from the write
# in the loop below until the next restore, and this script used to have no way out
# of that window except the ones it chooses to take. A killed run left the mutated
# file behind as ordinary-looking uncommitted work — a semantic edit to a tracked
# source file, which `git status` reports the same way it reports real work in
# progress, and which this box's standing rule tells the next agent not to throw
# away.
#
# A try/finally alone does NOT close this, and measuring it is how you find that
# out. Python raises KeyboardInterrupt for SIGINT, so a finally is on the way out
# for that one and for nothing else. SIGTERM and SIGHUP kill the process between
# the write and the restore — and those are exactly what a wall-clock cap, systemd
# and a process-group kill send. So the one signal a finally covers is the one you
# press by hand while watching, and the ones it misses are the ones an unattended
# run actually receives. Measured by kill on this scorer before these handlers
# existed: SIGTERM and SIGHUP each left the mutated file behind.
#
# The handler restores, reinstates the disposition it replaced and re-raises, so
# the process dies BY the signal (rc 128+signum). A handler that restores and
# exits 0 tells every caller a killed run succeeded.
#
# SIGKILL cannot be caught by the process that receives it. It is the one gap left
# here, and it is named rather than papered over.
_previous_handlers = {}


def _restore_and_reraise(signum, frame):
    restore()
    signal.signal(signum, _previous_handlers[signum])
    os.kill(os.getpid(), signum)


for _sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
    _previous_handlers[_sig] = signal.signal(_sig, _restore_and_reraise)

try:
    for label, fname, old, new, expect in CASES:
        restore()
        p = REPO / fname
        text = p.read_text()
        # Exact string replacement, asserted to occur exactly once. A stale pattern
        # silently mutates nothing and scores a bogus UNNOTICED.
        if text.count(old) != 1:
            print(f"  SETUP FAIL   {label}\n      pattern appears {text.count(old)}x in {fname}, want 1")
            continue
        p.write_text(text.replace(old, new, 1))

        r = subprocess.run(["go", "test", "-count=1", "-run", TESTS, "."],
                           cwd=REPO, capture_output=True, text=True)
        verdict, detail = classify(r.stdout + r.stderr)

        caught = verdict.startswith("CAUGHT") and "NOT coverage" not in verdict
        ok = caught == expect
        score += ok
        want = "CAUGHT" if expect else "UNNOTICED"
        print(f"  {'ok  ' if ok else 'BAD '} {verdict:<34} (want {want:<9}) {label}")
        if detail:
            print(f"         -> {detail}")
finally:
    restore()
    for _sig, _handler in _previous_handlers.items():
        signal.signal(_sig, _handler)
print(f"\nscore {score}/{len(CASES)}")
sys.exit(0 if score == len(CASES) else 1)
