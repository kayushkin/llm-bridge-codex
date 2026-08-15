"""Sabotage-score gateViaPrehook's request contract and its fail-closed returns.

This is a SECOND scorer next to sabotage-truncation.py rather than a widening of
it, and the split is deliberate. That one is named for the rune-boundary cut and
scores the cut; teaching it the request contract would make its name lie about
what a green run means. The two overlap in exactly one file and in no rows.

What this one measures, and why each half needed a scorer of its own:

  * The request contract. gateViaPrehook's doc comment claims the payload
    "matches ccPrehookPayload on the server side -- same JSON fields, same
    response contract". Nothing on either side held that. Before
    prehook_contract_test.go, no fixture decoded the body at all, so all five
    fields, the URL path, the method and the Content-Type could drift into a
    shape the server rejects at runtime with every suite still green.

  * Fail-closed. Five error returns, of which only the decode-adjacent one was
    ever reached. The five rows below flip each `return false` to `return true`,
    which is the exact defect the function's comment says it exists to prevent:
    "a permission gate that returns 'allow' on error would silently bypass the
    rule engine".

Three disciplines carried over from sabotage-truncation.py, each learned the
expensive way by an earlier pass:

  * CAUGHT is split into assertion-fired and guard-fired. A test that goes red
    because its own fixture blew up has detected nothing, and counting that as
    coverage inflates the score.

  * The filters that scope what this scorer can SEE are derived, never spelled.
    MUTATED_FILES comes from the case table and FAIL_LINE comes from TEST_FILES,
    because the 190th pass found the truncation scorer's fail-line regex frozen
    to one filename: a second test file's real detections would have classified
    as "no assertion text -- NOT coverage" and scored zero, which reads exactly
    like tests that did not work.

  * A -run filter is a claim about which tests are measured, and a typo in it
    fails silently -- the named test simply does not run and every row reads
    UNNOTICED. baseline_check() therefore asserts that every name in the filter
    actually ran, before any row is scored.

Run from anywhere:  python3 scripts/sabotage-prehook-contract.py
"""

import os
import pathlib
import re
import signal
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent
TARGET = "prehook_proxy.go"

# --- the payload literal, field by field -----------------------------------
# Spelled with the file's own indentation (two tabs) so each pattern can only
# match the map entry and not a mention of the same key elsewhere. The SETUP
# FAIL check asserts the count on every run rather than trusting it.
SESSION_ID = '\t\t"session_id":   bridgeID,'
TOOL_NAME = '\t\t"tool_name":    toolName,'
TOOL_INPUT = '\t\t"tool_input":   toolInput,'
TOOL_USE_ID = '\t\t"tool_use_id":  toolUseID,'
HOOK_EVENT = '\t\t"hook_event_name": "PreToolUse",'

URL_BUILD = '\turl := baseURL + "/permission/codex-prehook/" + bridgeID'
TIMEOUT = "\tclient := &http.Client{Timeout: 24 * time.Hour}"
STATUS_CHECK = "\tif resp.StatusCode/100 != 2 {"
DECISION = '\treturn decision == "allow", reason'

# The five fail-closed returns, each spelled whole so the flip is unambiguous.
FAILCLOSED = [
    ("the payload cannot be marshalled",
     '\t\treturn false, fmt.Sprintf("marshal prehook payload: %v", err)'),
    ("the request cannot be built",
     '\t\treturn false, fmt.Sprintf("build prehook request: %v", err)'),
    ("the bridge is unreachable",
     '\t\treturn false, fmt.Sprintf("prehook unreachable: %v", err)'),
    ("the response body cannot be read",
     '\t\treturn false, fmt.Sprintf("read prehook response: %v", err)'),
    ("the response cannot be decoded",
     '\t\treturn false, fmt.Sprintf("decode prehook response: %v", err)'),
]

CASES = [
    # (label, file, old, new, expect_caught)

    # ---- the five payload fields: wiring, then naming ----------------------
    # Each field gets a wiring row and, for the two most confusable, a key row.
    # A wiring row asks "is the right value here"; a key row asks "is the server
    # still going to find it". A per-field lookup in a test answers the first and
    # cannot answer the second, which is why the fixture compares the whole map.
    ("session_id carries the tool-use id instead of the bridge id",
     TARGET, SESSION_ID, '\t\t"session_id":   toolUseID,', True),
    ("session_id's JSON key drifts to camelCase, so the server reads nothing",
     TARGET, SESSION_ID, '\t\t"sessionId":   bridgeID,', True),
    ("tool_name carries the tool-use id",
     TARGET, TOOL_NAME, '\t\t"tool_name":    toolUseID,', True),
    ("tool_use_id carries the tool name",
     TARGET, TOOL_USE_ID, '\t\t"tool_use_id":  toolName,', True),
    ("tool_input is dropped from the payload",
     TARGET, TOOL_INPUT, '\t\t"tool_input":   nil,', True),
    ("hook_event_name announces the WRONG hook, PostToolUse",
     TARGET, HOOK_EVENT, '\t\t"hook_event_name": "PostToolUse",', True),
    ("hook_event_name's JSON key drifts to camelCase",
     TARGET, HOOK_EVENT, '\t\t"hookEventName": "PreToolUse",', True),

    # ---- where the request goes and how it is framed -----------------------
    # An httptest stub serves every path alike, so a fixture that only reads the
    # body cannot fail on a wrong URL -- the shape card 64766783 sweeps for
    # fleet-wide. These three rows are what turn that assertion into a score.
    ("the path drifts to the Claude Code prehook route",
     TARGET, URL_BUILD, URL_BUILD.replace("codex-prehook", "cc-prehook"), True),
    ("the path is keyed by the tool-use id instead of the bridge id",
     TARGET, URL_BUILD, URL_BUILD.replace("+ bridgeID", "+ toolUseID"), True),
    ("the prehook is PUT instead of POSTed",
     TARGET, "http.MethodPost, url", "http.MethodPut, url", True),
    ("the Content-Type stops announcing JSON",
     TARGET, '\treq.Header.Set("Content-Type", "application/json")',
     '\treq.Header.Set("Content-Type", "text/plain")', True),

    # ---- the 24h timeout ---------------------------------------------------
    # The generous timeout is what lets a prehook park for a human resolver. A
    # test cannot wait out the difference between 24h and any other long value,
    # so what IS scorable is that the timeout is longer than a round trip.
    ("the timeout shrinks below one round trip, so every call times out",
     TARGET, TIMEOUT, "\tclient := &http.Client{Timeout: 1 * time.Nanosecond}", True),
    # Known-NEGATIVE control. 24h and 24min are indistinguishable to any suite
    # that finishes, and pretending otherwise would need a test that sleeps for
    # a day. The row exists so the score says out loud which half of this
    # literal is measured: its order of magnitude, not its value.
    ("CONTROL (unmeasurable): the parked-ask window shrinks 24h -> 24min",
     TARGET, TIMEOUT, "\tclient := &http.Client{Timeout: 24 * time.Minute}", False),

    # ---- the status check, from both sides ---------------------------------
    ("only a bare 200 is accepted, so a 202 fails closed",
     TARGET, STATUS_CHECK, "\tif resp.StatusCode != 200 {", True),
    ("everything under 400 is accepted, so a redirect body is read as a decision",
     TARGET, STATUS_CHECK, "\tif resp.StatusCode >= 400 {", True),

    # ---- the five fail-closed returns --------------------------------------
    # Filled in below the table: one row per return, each flipping false to true.

    # ---- the decision, and the response shape it is read out of ------------
    ('any decision that is not "deny" approves, so every parked ask is let through',
     TARGET, DECISION, '\treturn decision != "deny", reason', True),
    ('the decision literal changes case, so a real "allow" no longer approves',
     TARGET, DECISION, '\treturn decision == "Allow", reason', True),
    ("the permissionDecision JSON tag drifts, so every decision decodes empty",
     TARGET, '`json:"permissionDecision"`', '`json:"permission_decision"`', True),
    ("the permissionDecisionReason JSON tag drifts, so codex is told nothing",
     TARGET, '`json:"permissionDecisionReason"`', '`json:"permission_decision_reason"`', True),
    ("the hookSpecificOutput envelope tag drifts, so the whole response decodes empty",
     TARGET, '`json:"hookSpecificOutput"`', '`json:"hook_specific_output"`', True),

    # ---- reason distinctness -----------------------------------------------
    # The reason is what codex shows the user, and five denials that all read
    # alike would make a permission failure undiagnosable. This row asks whether
    # anything would notice two of them merging.
    ("two fail-closed reasons merge, so an unreachable bridge reports a read error",
     TARGET, 'fmt.Sprintf("prehook unreachable: %v", err)',
     'fmt.Sprintf("read prehook response: %v", err)', True),

    # ---- known-negative controls -------------------------------------------
    # A harness with no control reports CAUGHT for everything and looks perfect
    # while measuring nothing. These two are real holes, named rather than hidden.
    ("CONTROL (out of scope): the log line's prefix changes",
     TARGET, 'log.Printf("[prehook-proxy] %s → %s (%s)"',
     'log.Printf("[contract-proxy] %s → %s (%s)"', False),
    ("CONTROL (unmeasured): the response body is never closed",
     TARGET, "\tdefer resp.Body.Close()", "\tdefer func() {}()", False),
]

# Splice the five fail-closed rows in, so the label and the pattern cannot drift
# apart the way they would if each were typed twice.
CASES[17:17] = [
    ("fails OPEN when %s" % what, TARGET, ret, ret.replace("return false,", "return true,", 1), True)
    for what, ret in FAILCLOSED
]

# Every top-level test the new coverage lives in. This is NOT a -run filter --
# see RUN_SUITE below -- it is what baseline_check asserts actually ran, so a
# renamed or deleted test is a hard failure rather than a silent slide back to
# the before-score.
TEST_NAMES = (
    "TestPrehookRequestCarriesTheServerSideContract",
    "TestPrehookApprovesOnlyAnAllowDecision",
    "TestPrehookAcceptsTheWholeTwoHundredFamily",
    "TestPrehookFailsClosedOnARedirect",
    "TestPrehookFailsClosedWhenThePayloadCannotBeMarshalled",
    "TestPrehookFailsClosedWhenTheRequestCannotBeBuilt",
    "TestPrehookFailsClosedWhenTheBridgeIsUnreachable",
    "TestPrehookFailsClosedWhenTheResponseBodyCannotBeRead",
    "TestPrehookFailsClosedOnAnUndecodableResponse",
)

# This scorer runs the WHOLE package and applies no -run filter, which is the
# one place it deliberately departs from sabotage-truncation.py.
#
# The reason is a measurement, not a preference. Scoring these rows against only
# the new test file first, and then against the whole suite, gave different
# numbers: four rows (the path, the path's key, the method, the Content-Type) and
# the timeout row were ALREADY caught by prehook_proxy_test.go's assertShippedRequest,
# which the card filing this work had listed as unmeasured. They were asserted and
# firing the whole time -- just never scored, because the truncation scorer's -run
# filter never selected a mutation that could move them.
#
# A filter would therefore have let this scorer claim credit for coverage that
# already existed. The question a sabotage score should answer is "would this
# repo notice", not "would the file I just wrote notice", and only the unfiltered
# run answers it. The 189th pass's rule -- a -run filter is a claim about which
# numbers are measured, and it is usually narrower than the file -- one layer out
# again: here the honest filter is no filter.
RUN_SUITE = ["go", "test", "-count=1", "."]

# Any _test.go file's failures count as detection, and the file is captured so
# the detail line can say WHICH suite noticed. Naming files individually is the
# defect the 190th pass found in the truncation scorer: a real failure from a
# file the pattern forgot classifies as "no assertion text -- NOT coverage" and
# scores zero, which reads exactly like a test that did not work.
FAIL_LINE = re.compile(r"^\s*(\w+_test)\.go:\d+: (.*)$", re.M)
FRAME = re.compile(r"^\s+(/\S+\.go):(\d+)", re.M)

# Messages from a fixture falling over rather than from an assertion. Because the
# whole suite runs, this is the union across every test file in the package, not
# just the new one -- and the union matters: measured on the before-run, the
# permissionDecision tag drift takes prehook_proxy_test.go's log-line lookup down
# as a GUARD, which is the suite falling over, not detecting.
GUARD_MARKERS = (
    "fixture is malformed",
    "prehook log line not found",
    "discoverSessions:",
    "mkdir sessionsDir:",
    "want 1 cold-imported session",
    "marshal:",
    "marshal user line:",
    "the cut never landed inside a rune",
    "the known-negative control never ran",
)

# Literals in prehook_proxy.go this scorer deliberately does NOT score, printed
# on every run. A scorer that measured 26 of 28 otherwise prints exactly like one
# that measured all 28.
NOT_SCORED = (
    "truncateAtRuneBoundaryWithEllipsis(string(respBody), 200) -- owned by sabotage-truncation.py",
    "truncateAtRuneBoundaryWithEllipsis(reason, 80)            -- owned by sabotage-truncation.py",
)


def classify(output):
    """Return (verdict, detail) for one sabotage run's output."""
    if "[build failed]" in output or "declared and not used" in output:
        return "COMPILE ERROR", "mutation orphaned an identifier -- rewrite it"
    if "panic:" in output:
        # Read WHERE the panic is. A mutation that crashes production code the
        # test drove it into IS detection. A panic in the fixture is the test
        # falling over before it asserted anything, which is not coverage.
        frames = [f for f, _ in FRAME.findall(output.split("panic:", 1)[1])
                  if f.startswith(str(REPO) + "/")]
        source = next((f for f in frames if not f.endswith("_test.go")), None)
        if source:
            return ("CAUGHT (panic in %s)" % pathlib.Path(source).name,
                    "test drove the mutation into a crash")
        return "CAUGHT (panic in fixture -- NOT coverage)", "the test fell over before asserting"
    if "--- FAIL" not in output:
        return "UNNOTICED", ""
    found = FAIL_LINE.findall(output)
    if not found:
        return "CAUGHT (no assertion text -- NOT coverage)", ""
    guard = [(f, m) for f, m in found if any(g in m for g in GUARD_MARKERS)]
    real = [(f, m) for f, m in found if (f, m) not in guard]
    if not real:
        return "CAUGHT (guard only -- NOT coverage)", "%s.go: %s" % (guard[0][0], guard[0][1][:74])
    # Name the file that noticed. With the whole suite running, "which suite
    # caught this" is the difference between coverage this scorer's test file
    # added and coverage that was already there.
    return "CAUGHT", "%s.go: %s" % (real[0][0], real[0][1][:74])


def self_test():
    """Drive classify() against every verdict it can return.

    A classify() whose "guard only" branch is unreachable prints the clean score
    you were hoping for. The two panic verdicts are included even though no row
    in CASES produces one, which is exactly why they cannot be measured by
    sabotaging toward them.
    """
    probes = [
        ("--- FAIL: X\n    prehook_contract_test.go:40: the prehook payload does not match\n", "CAUGHT"),
        ("--- FAIL: X\n    prehook_contract_test.go:99: fixture is malformed: encode decision\n",
         "CAUGHT (guard only -- NOT coverage)"),
        # A guard in one file and a real assertion in another must read as
        # coverage. Scoring the whole suite makes this mixture ordinary rather
        # than exotic, and a classifier that let the guard mask the assertion
        # would under-report every row that trips two suites at once.
        ("--- FAIL: X\n    prehook_proxy_test.go:99: prehook log line not found: no marker\n"
         "--- FAIL: Y\n    prehook_contract_test.go:40: approved = true, want false\n", "CAUGHT"),
        ("ok  \tgithub.com/x\n", "UNNOTICED"),
        # A red run whose only failure text is not an assertion line at all --
        # a timeout or a killed binary. It must not read as coverage.
        ("--- FAIL: X\n    the test binary was killed\n",
         "CAUGHT (no assertion text -- NOT coverage)"),
        ("panic: runtime error\n"
         "\t/usr/lib/go/src/runtime/panic.go:860 +0x13a\n"
         "\t%s/prehook_proxy.go:88\n"
         "\t%s/prehook_contract_test.go:41\n" % (REPO, REPO), "CAUGHT (panic in prehook_proxy.go)"),
        ("panic: runtime error\n"
         "\t/usr/lib/go/src/runtime/panic.go:860 +0x13a\n"
         "\t%s/prehook_contract_test.go:149\n" % REPO,
         "CAUGHT (panic in fixture -- NOT coverage)"),
        ("# github.com/x [build failed]\n", "COMPILE ERROR"),
    ]
    ok = True
    for output, want in probes:
        got, _ = classify(output)
        if got != want:
            print(f"  classifier SELF-TEST FAIL: got {got!r}, want {want!r}")
            ok = False
    print(f"  classifier self-test: {'all %d probes agree' % len(probes) if ok else 'BROKEN'}")
    return ok


def baseline_check():
    """Run the unmutated suite green, and assert every named test really ran.

    Dropping the -run filter removes the failure mode where a misspelled name
    silently selects nothing, but it introduces the opposite one: a test file
    that was deleted, renamed or excluded by a build tag takes its rows down to
    UNNOTICED and the summary reads like coverage that was never written rather
    than coverage that went missing. Naming the tests and checking they ran
    costs one run and closes that.
    """
    r = subprocess.run(RUN_SUITE + ["-v"], cwd=REPO, capture_output=True, text=True)
    output = r.stdout + r.stderr
    ran = set(re.findall(r"^=== RUN\s+(Test\w+)$", output, re.M))
    missing = [n for n in TEST_NAMES if n not in ran]
    if missing:
        print("  baseline FAIL: named test did not run: " + ", ".join(missing))
        return False
    if r.returncode != 0:
        print("  baseline FAIL: the suite is red before any mutation -- scores would be meaningless")
        return False
    print(f"  baseline: whole package green, and all {len(TEST_NAMES)} named contract tests ran")
    return True


# Every file any row mutates, derived from the table so it cannot drift from it.
MUTATED_FILES = sorted({fname for _, fname, _, _, _ in CASES})


def restore():
    subprocess.run(["git", "checkout", "--"] + MUTATED_FILES, cwd=REPO, check=True)


print("Sabotaging gateViaPrehook's request contract and fail-closed returns\n")
if not self_test():
    sys.exit(2)
if not baseline_check():
    sys.exit(2)
print("\n  NOT SCORED here (named so a partial score cannot read as a whole one):")
for line in NOT_SCORED:
    print("    " + line)
print()

score = 0
# The target holds a deliberately broken version of itself between the write
# below and the next restore. A try/finally alone does not close that window:
# Python raises KeyboardInterrupt for SIGINT, so a finally covers the signal you
# press by hand while watching and misses SIGTERM and SIGHUP -- which are exactly
# what a wall-clock cap, systemd and a process-group kill send. A killed run would
# leave a semantic edit to a tracked source file behind, looking like ordinary
# work in progress that this box's standing rule tells the next agent to keep.
#
# The handler restores, reinstates the disposition it replaced and re-raises, so
# the process dies BY the signal (rc 128+signum) rather than telling its caller a
# killed run succeeded. SIGKILL cannot be caught, and is the one gap left.
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
        # Exact replacement, asserted to occur exactly once. A stale pattern
        # silently mutates nothing and scores a bogus UNNOTICED.
        if text.count(old) != 1:
            print(f"  SETUP FAIL   {label}\n      pattern appears {text.count(old)}x in {fname}, want 1")
            continue
        p.write_text(text.replace(old, new, 1))

        r = subprocess.run(RUN_SUITE, cwd=REPO, capture_output=True, text=True)
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
