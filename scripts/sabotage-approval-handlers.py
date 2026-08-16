"""Sabotage-score gateApproval, its auto-approve fallback, and the nine handlers.

This is a THIRD scorer next to sabotage-truncation.py and sabotage-prehook-contract.py,
and the split follows the same rule those two established: a scorer is named for
the thing it scores, and widening one to cover a neighbouring frame makes its name
lie about what a green run means.

    sabotage-truncation.py          the rune-boundary cut and its two budgets
    sabotage-prehook-contract.py    gateViaPrehook: the request contract, five fail-closed returns
    this one                        gateApproval and RegisterApprovalHandlers, one frame up

The three overlap in no rows. This one is the only one that mutates translate.go's
approval half or codex.go at all.

What it measures, and why the frame needed its own scorer:

  * The branch that fails OPEN. gateViaPrehook is fail-closed on all five of its
    error returns and scores 28/28. Its caller then auto-approves EVERY tool call
    when bridgeServerURL is empty, by design and with a loud log. That inversion
    lives entirely in gateApproval, so no row in the scorer below it can reach it,
    and before approval_handlers_test.go nothing asserted it approved, nothing
    asserted it logged, and nothing asserted it was confined to the empty-URL
    case. A condition drifting to something a CONFIGURED session can reach is a
    silent fleet-wide permission bypass, and the rows in section A are what would
    now notice it.

  * The five tool_input shapes permission-store matches on. Each handler builds
    its own map. sabotage-prehook-contract.py pins the ENVELOPE those maps travel
    in, for one synthetic tool_input; it cannot tell whether the five that ship
    carry the keys the rule engine looks for. A key renamed in one handler
    changes which rules match, silently.

  * The four methods that are not approvals. RegisterApprovalHandlers registers
    NINE methods, not five. The card that filed this work named five and warned
    to read its list as candidates; the other four were untested for exactly the
    same reason and are scored here.

Three disciplines carried over, each learned expensively by an earlier pass:

  * CAUGHT is split into assertion-fired and guard-fired. A test that goes red
    because its own fixture blew up has detected nothing.

  * The filters that scope what this scorer can SEE are derived, never spelled.
    MUTATED_FILES comes from the case table; FAIL_LINE matches any *_test.go,
    because the 190th pass found a fail-line regex frozen to one filename and a
    second test file's real detections scoring zero.

  * No -run filter. The 191st pass measured that filtering to the file you just
    wrote lets a scorer claim credit for coverage that already existed. The
    question is "would this repo notice", not "would my new file notice", and
    only an unfiltered run answers it. baseline_check therefore asserts every
    named test actually ran, which is the failure mode dropping the filter
    reintroduces.

Run from anywhere:  python3 scripts/sabotage-approval-handlers.py
"""

import os
import pathlib
import re
import signal
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent
TARGET = "translate.go"
TYPES = "codex.go"

# --- the gate itself --------------------------------------------------------
SNAPSHOT_BRIDGE_ID = "\tbridgeID := t.bridgeID"
FALLBACK_COND = '\tif baseURL == "" {'
FALLBACK_LOG = ('\t\tlog.Printf("[approval] bridgeServerURL unset — falling back to '
                'auto-approve %s", toolName)')
FALLBACK_APPROVED = "\t\tapproved = true"
FALLBACK_REASON = '\t\treason = "auto-approved (no bridge URL configured)"'

# --- the canonical event ----------------------------------------------------
EVENT_TYPE = "\te := t.event(msg.EventApproval)"
EVENT_STATUS = '\tstatus := "approved"'
EVENT_ACTION = '\taction := "approve"'
EVENT_FLIP = "\tif !approved {"
EVENT_TOOL = "\t\tToolName: toolName,"
EVENT_DETAIL = "\t\tDetail:   reason,"
EVENT_RAW = "\te.Raw = rawParams"
# t.emit(e) alone appears 35 times in this file — every event the translator
# sends. Anchoring it to the line above is what scopes the row to the approval
# path; measured, not assumed, by the SETUP FAIL guard rejecting the bare form.
EVENT_EMIT = "\te.Raw = rawParams\n\tt.emit(e)"

# --- the five gated handlers ------------------------------------------------
# Each spelled as the whole multi-line call, because the two unified_exec
# handlers differ only in their last line: a single-line pattern would match
# both and the SETUP FAIL guard would reject it rather than scoring the wrong
# one, which is the guard doing its job but not the row getting measured.
CMD_GATE = ('\t\tapproved, reason := t.gateApproval("unified_exec", map[string]any{\n'
            '\t\t\t"command": req.Command,\n'
            "\t\t}, req.ItemID, params)")
EXEC_GATE = ('\t\tapproved, reason := t.gateApproval("unified_exec", map[string]any{\n'
             '\t\t\t"command": req.Command,\n'
             '\t\t}, "", params)')
FILE_GATE = ('\t\tapproved, reason := t.gateApproval("apply_patch", map[string]any{\n'
             '\t\t\t"path":  req.Path,\n'
             '\t\t\t"patch": req.Patch,\n'
             "\t\t}, req.ItemID, params)")
PERM_GATE = ('\t\tapproved, reason := t.gateApproval("request_permissions", map[string]any{\n'
             '\t\t\t"permissions": req.Permissions,\n'
             "\t\t}, req.ItemID, params)")
PATCH_GATE = ('\t\tapproved, reason := t.gateApproval("apply_patch", '
              'json.RawMessage(params), "", params)')

# --- the four handlers that are not approvals -------------------------------
USER_INPUT = ('\t\treturn nil, &RPCError{Code: -32000, Message: '
              '"headless mode: user input not available"}')
TOOL_CALL = ('\t\treturn nil, &RPCError{Code: -32000, Message: '
             '"headless mode: dynamic tool calls not supported"}')
ELICITATION = '\t\treturn json.Marshal(map[string]string{"action": "cancel"})'
AUTH_REFRESH = ('\t\treturn nil, &RPCError{Code: -32000, Message: '
                '"bridge does not manage auth tokens"}')

CASES = [
    # (label, file, old, new, expect_caught)

    # ---- A. the branch that fails OPEN -------------------------------------
    # The headline of this scorer. Every row here is a way the auto-approve
    # fallback stops being confined to an unconfigured session, or stops
    # announcing itself when it fires.
    ("the auto-approve fallback fires for CONFIGURED sessions instead of unconfigured ones",
     TARGET, FALLBACK_COND, '\tif baseURL != "" {', True),
    ("the unconfigured fallback denies instead of approving, breaking legacy sessions",
     TARGET, FALLBACK_APPROVED, "\t\tapproved = false", True),
    # Measured, not assumed: deleting this call outright is a COMPILE ERROR, not
    # an UNNOTICED. It is the only use of the log package in translate.go, so
    # full silence orphans the import and the compiler is the test — which is
    # worth knowing, because it means the branch cannot be quietly silenced by a
    # one-line deletion. What CAN happen without the compiler noticing is the
    # message being tidied down to something that no longer announces itself, so
    # that is the row.
    ("the fallback log stops announcing itself and logs a bare tool name",
     TARGET, FALLBACK_LOG, '\t\tlog.Printf("%s", toolName)', True),
    ("the fallback log stops naming the tool it let through",
     TARGET, FALLBACK_LOG,
     '\t\tlog.Printf("[approval] bridgeServerURL unset — falling back to auto-approve")', True),
    ("the fallback log's prefix drifts, so an [approval] grep finds nothing",
     TARGET, FALLBACK_LOG,
     FALLBACK_LOG.replace("[approval]", "[gate]"), True),
    ("the fallback reason stops saying WHY it approved, so codex shows the user nothing useful",
     TARGET, FALLBACK_REASON, '\t\treason = "auto-approved"', True),

    # ---- B. the snapshot ---------------------------------------------------
    # NewTranslator sets sessionID and bridgeID from one argument, so this row is
    # only scorable because the fixture calls SetSessionID and drives them apart.
    # Without that it reads UNNOTICED and looks like a missing test rather than a
    # fixture that cannot distinguish two fields.
    ("the gate asks about the codex thread id instead of the bridge session id",
     TARGET, SNAPSHOT_BRIDGE_ID, "\tbridgeID := t.sessionID", True),

    # ---- C. the tool name each handler gates under -------------------------
    # permission-store rules are keyed on tool_name. Two handlers legitimately
    # share apply_patch and two share unified_exec, which is why a row that
    # swapped one for the other would still produce a name the rule engine
    # accepts — and match the wrong rules.
    ("commandExecution gates under apply_patch, so exec rules stop matching",
     TARGET, CMD_GATE, CMD_GATE.replace('"unified_exec"', '"apply_patch"'), True),
    ("fileChange gates under unified_exec, so patch rules stop matching",
     TARGET, FILE_GATE, FILE_GATE.replace('"apply_patch"', '"unified_exec"'), True),
    ("permissions gates under unified_exec",
     TARGET, PERM_GATE, PERM_GATE.replace('"request_permissions"', '"unified_exec"'), True),
    ("applyPatchApproval gates under unified_exec",
     TARGET, PATCH_GATE, PATCH_GATE.replace('"apply_patch"', '"unified_exec"'), True),
    ("execCommandApproval gates under apply_patch",
     TARGET, EXEC_GATE, EXEC_GATE.replace('"unified_exec"', '"apply_patch"'), True),

    # ---- D. the tool_input keys the rules match on -------------------------
    # A renamed key here keeps this side compiling and every envelope test green;
    # the rule engine simply stops finding the field and matches nothing.
    ("commandExecution's command key is renamed, so command rules see no command",
     TARGET, CMD_GATE, CMD_GATE.replace('"command":', '"cmd":'), True),
    ("fileChange's path key is renamed, so path rules see no path",
     TARGET, FILE_GATE, FILE_GATE.replace('"path":  req.Path', '"file":  req.Path'), True),
    ("fileChange stops sending the patch body at all",
     TARGET, FILE_GATE, FILE_GATE.replace('\t\t\t"patch": req.Patch,\n', ""), True),
    ("permissions' key is renamed",
     TARGET, PERM_GATE, PERM_GATE.replace('"permissions":', '"perms":'), True),
    ("execCommandApproval's command key is renamed",
     TARGET, EXEC_GATE, EXEC_GATE.replace('"command":', '"cmd":'), True),
    # applyPatchApproval is the odd handler out: it forwards the RAW params, so
    # every field codex sent reaches the rules. A rewrite to the tidy map shape
    # the other handlers use is the likeliest edit anyone would make here, and it
    # silently drops fields.
    ("applyPatchApproval is tidied into a built map, dropping the fields codex sent",
     TARGET, PATCH_GATE,
     '\t\tapproved, reason := t.gateApproval("apply_patch", map[string]any{"path": "x"}, "", params)',
     True),

    # ---- E. tool_use_id ----------------------------------------------------
    # The doc comment says this is what correlates an approval with its parked-ask
    # banner in the UI. Two handlers ship an empty one and three ship the item id;
    # nothing said so before, so either could have become the other.
    ("commandExecution stops correlating its approval with the parked-ask banner",
     TARGET, CMD_GATE, CMD_GATE.replace(", req.ItemID, params)", ', "", params)'), True),
    ("fileChange correlates on the thread id instead of the item id",
     TARGET, FILE_GATE, FILE_GATE.replace(", req.ItemID, params)", ", req.ThreadID, params)"), True),
    ("execCommandApproval leaks the command itself into tool_use_id",
     TARGET, EXEC_GATE, EXEC_GATE.replace(', "", params)', ", req.Command, params)"), True),
    ("applyPatchApproval invents a tool_use_id no banner will match",
     TARGET, PATCH_GATE, PATCH_GATE.replace(', "", params)', ', "legacy", params)'), True),

    # ---- F. the method names codex sends -----------------------------------
    # A handler registered under a name codex never sends never runs, and looks
    # identical to a working one from inside the closure. Nothing but a test that
    # reads the registered SET can see it.
    ("fileChange is registered under a misspelled method, so codex hangs waiting",
     TARGET, '\tsrv.OnRequest("item/fileChange/requestApproval"',
     '\tsrv.OnRequest("item/filechange/requestApproval"', True),
    ("execCommandApproval is registered under the wrong name",
     TARGET, '\tsrv.OnRequest("execCommandApproval"',
     '\tsrv.OnRequest("execCommandApprove"', True),

    # ---- G. the envelope codex reads the decision out of -------------------
    ("the approved field's JSON tag drifts, so codex reads no decision at all",
     TYPES, '\tApproved bool   `json:"approved"`', '\tApproved bool   `json:"is_approved"`', True),
    ("the reason field's JSON tag drifts, so codex shows the user nothing",
     TYPES, '\tReason   string `json:"reason,omitempty"`',
     '\tReason   string `json:"detail,omitempty"`', True),

    # ---- H. the canonical event -------------------------------------------
    # A separate surface from the response codex gets, and it can drift alone:
    # the event carries the decision as two strings where the wire carries a
    # bool, so a denial answered correctly can still be logged as approved.
    ("the decision is logged under the wrong event type, vanishing from the approval stream",
     TARGET, EVENT_TYPE, "\te := t.event(msg.EventError)", True),
    ("an allowed call is logged with a status no consumer expects",
     TARGET, EVENT_STATUS, '\tstatus := "pending"', True),
    ("an allowed call is logged with the wrong action",
     TARGET, EVENT_ACTION, '\taction := "run"', True),
    ("the status/action flip inverts, so denials are logged as approvals",
     TARGET, EVENT_FLIP, "\tif approved {", True),
    ("the event stops naming the tool the decision was about",
     TARGET, EVENT_TOOL, '\t\tToolName: "",', True),
    ("the event stops carrying the gate's reason",
     TARGET, EVENT_DETAIL, '\t\tDetail:   "",', True),
    ("the event drops codex's original payload, so nobody can see what was asked",
     TARGET, EVENT_RAW, "\te.Raw = nil", True),
    ("the event is built and never emitted, so gated calls leave no record",
     TARGET, EVENT_EMIT, "\te.Raw = rawParams\n\t_ = e", True),

    # ---- I. the four handlers that are not approvals -----------------------
    ("the headless refusal stops saying WHAT was refused",
     TARGET, USER_INPUT,
     '\t\treturn nil, &RPCError{Code: -32000, Message: "headless mode: unavailable"}', True),
    ("the dynamic-tool-call refusal changes its RPC code",
     TARGET, TOOL_CALL, TOOL_CALL.replace("-32000", "-32001"), True),
    ("an MCP elicitation is answered with an action no MCP server acts on",
     TARGET, ELICITATION,
     '\t\treturn json.Marshal(map[string]string{"action": "decline"})', True),
    ("the elicitation answer grows a field the protocol does not define",
     TARGET, ELICITATION,
     '\t\treturn json.Marshal(map[string]string{"action": "cancel", "reason": "headless"})', True),
    ("the auth-token refusal stops naming auth tokens",
     TARGET, AUTH_REFRESH,
     '\t\treturn nil, &RPCError{Code: -32000, Message: "bridge does not manage tokens"}', True),

    # ---- J. known-negative controls ----------------------------------------
    # A harness with no control reports CAUGHT for everything and looks perfect
    # while measuring nothing. Both of these are real holes, named rather than
    # hidden — and the second is a genuine wire-shape change nothing here can see.
    ("CONTROL (inert): omitempty on a decode-only field, which does nothing on the way in",
     TYPES, '\tPatch    string `json:"patch,omitempty"`', '\tPatch    string `json:"patch"`', False),
    ("CONTROL (unmeasured): an empty reason starts shipping as \"reason\":\"\" instead of being omitted",
     TYPES, '\tReason   string `json:"reason,omitempty"`', '\tReason   string `json:"reason"`', False),
]

# Every top-level test the new coverage lives in. NOT a -run filter — see
# RUN_SUITE — this is what baseline_check asserts actually ran, so a renamed or
# deleted test is a hard failure rather than a silent slide back to the
# before-score.
TEST_NAMES = (
    "TestRegisterApprovalHandlersRegistersEveryMethodCodexSends",
    "TestApprovalHandlersSendTheShapePermissionStoreMatchesOn",
    "TestApprovalHandlersReturnCodexTheDecision",
    "TestUnconfiguredBridgeURLAutoApprovesWithoutConsultingAnyRule",
    "TestTheAutoApproveFallbackIsConfinedToAnEmptyURL",
    "TestAFailClosedGateStaysClosedThroughTheHandler",
    "TestEveryGatedCallEmitsTheCanonicalApprovalEvent",
    "TestTheUnconfiguredFallbackIsAlsoRecordedAsAnApproval",
    "TestTypedApprovalHandlersRefuseAMalformedPayload",
    "TestExecCommandApprovalGatesOnAnEmptyCommandWhenItsPayloadIsMalformed",
    "TestHeadlessHandlersRefuseWhatNoOneCanAnswer",
    "TestElicitationIsCancelledRatherThanRefused",
)

# The whole package, no -run filter. See the module docstring.
RUN_SUITE = ["go", "test", "-count=1", "."]

# Any _test.go file's failures count as detection, and the file is captured so
# the detail line can say WHICH suite noticed.
FAIL_LINE = re.compile(r"^\s*(\w+_test)\.go:\d+: (.*)$", re.M)
FRAME = re.compile(r"^\s+(/\S+\.go):(\d+)", re.M)

# Messages from a fixture falling over rather than from an assertion. This is the
# union across every test file in the package, because the whole suite runs.
#
# Note what is NOT here: "no handler registered for", "no request reached the
# prehook at all" and "the approval response is not valid JSON" all read like
# fixture problems and are real detections — a renamed method, a gate that was
# never consulted, and a handler answering codex with something it cannot parse
# are the defects this file exists to catch.
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

# Literals in the mutated files this scorer deliberately does NOT score, printed
# on every run. A scorer that measured 38 of 40 otherwise prints exactly like one
# that measured all 40.
NOT_SCORED = (
    "gateViaPrehook's request contract and five fail-closed returns -- owned by sabotage-prehook-contract.py",
    "truncateAtRuneBoundaryWithEllipsis's <= boundary                -- owned by sabotage-truncation.py",
    "the t.mu snapshot around baseURL/bridgeID: a lock this scorer cannot move without deadlocking",
    "msg.ApprovalEvent's Command/Path/Patch/Permissions fields, which this bridge never populates",
    "the 24h parked-ask window reached through gateApproval: unmeasurable by any suite that finishes",
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
    return "CAUGHT", "%s.go: %s" % (real[0][0], real[0][1][:74])


def self_test():
    """Drive classify() against every verdict it can return.

    A classify() whose "guard only" branch is unreachable prints the clean score
    you were hoping for. The two panic verdicts are included even though no row
    in CASES produces one, which is exactly why they cannot be measured by
    sabotaging toward them.
    """
    probes = [
        ("--- FAIL: X\n    approval_handlers_test.go:40: gated as tool_name \"apply_patch\"\n", "CAUGHT"),
        ("--- FAIL: X\n    approval_handlers_test.go:99: fixture is malformed: encode decision\n",
         "CAUGHT (guard only -- NOT coverage)"),
        # A guard in one file and a real assertion in another must read as
        # coverage: scoring the whole suite makes that mixture ordinary.
        ("--- FAIL: X\n    prehook_proxy_test.go:99: prehook log line not found: no marker\n"
         "--- FAIL: Y\n    approval_handlers_test.go:40: approved = true, want false\n", "CAUGHT"),
        ("ok  \tgithub.com/x\n", "UNNOTICED"),
        ("--- FAIL: X\n    the test binary was killed\n",
         "CAUGHT (no assertion text -- NOT coverage)"),
        ("panic: runtime error\n"
         "\t/usr/lib/go/src/runtime/panic.go:860 +0x13a\n"
         "\t%s/translate.go:800\n"
         "\t%s/approval_handlers_test.go:41\n" % (REPO, REPO), "CAUGHT (panic in translate.go)"),
        ("panic: runtime error\n"
         "\t/usr/lib/go/src/runtime/panic.go:860 +0x13a\n"
         "\t%s/approval_handlers_test.go:149\n" % REPO,
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
    """Run the unmutated suite green, and assert every named test really ran."""
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
    print(f"  baseline: whole package green, and all {len(TEST_NAMES)} named approval tests ran")
    return True


# Every file any row mutates, derived from the table so it cannot drift from it.
MUTATED_FILES = sorted({fname for _, fname, _, _, _ in CASES})


# `restore()` below is `git checkout --`, so it puts these files back to HEAD. It
# cannot tell a mutation this scorer wrote from work somebody has not committed
# yet, and it runs at the TOP of every case, before anything is read. So scoring a
# fix that is written but not yet committed deletes the fix and scores HEAD.
#
# The symptom accuses the wrong file. Every case then prints
# `SETUP FAIL: pattern not found`, which reads as a stale case list — so the
# obvious next move is to edit the case list, against a source file the scorer has
# already reverted. Nothing in that output mentions the checkout. Measured in
# memory-store by the 239th nightly pass: eight rows read SETUP FAIL and the ninth
# read ok, and the loss was found by being bitten rather than by reading.
#
# The shared engine (tool-store scripts/sabotage.py) has refused this for a long
# time. This scorer does not import the engine, so it never inherited the refusal.
_dirty = subprocess.run(["git", "status", "--porcelain", "--"] + MUTATED_FILES,
                        cwd=REPO, capture_output=True, text=True,
                        check=True).stdout.strip()
if _dirty:
    sys.exit("REFUSING: these have uncommitted changes; this harness restores "
             "from git and would delete them:\n%s" % _dirty)


def restore():
    subprocess.run(["git", "checkout", "--"] + MUTATED_FILES, cwd=REPO, check=True)


print("Sabotaging gateApproval, its auto-approve fallback, and the nine registered handlers\n")
if not self_test():
    sys.exit(2)
if not baseline_check():
    sys.exit(2)
print("\n  NOT SCORED here (named so a partial score cannot read as a whole one):")
for line in NOT_SCORED:
    print("    " + line)
print()

score = 0
# The targets hold a deliberately broken version of themselves between the write
# below and the next restore. A try/finally alone does not close that window:
# Python raises KeyboardInterrupt for SIGINT, so a finally covers the signal you
# press by hand and misses SIGTERM and SIGHUP -- exactly what a wall-clock cap,
# systemd and a process-group kill send. A killed run would leave a semantic edit
# to a tracked source file behind, looking like ordinary work in progress that
# this box's standing rule tells the next agent to keep.
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
