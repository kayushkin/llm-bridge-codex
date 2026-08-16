package main

import (
	"testing"
	"unicode/utf8"
)

// The real thing, as Codex writes it. Every case below is measured against
// this so a stricter parse cannot quietly stop recognising a live filename.
const realRolloutName = "rollout-2026-04-13T02-00-13-019d8491-627f-79e1-89a7-d32ed21ee93e.jsonl"
const realRolloutID = "019d8491-627f-79e1-89a7-d32ed21ee93e"

// splitRuneRolloutName is the case the four-dash guard waves through: a
// filename whose last 36 bytes begin in the MIDDLE of a multi-byte rune and
// still carry '-' at 8, 13, 18 and 23.
//
// "é" is 0xC3 0xA9. The 36-byte window opens on the 0xA9, so the candidate
// starts with a continuation byte — not valid UTF-8, and not a UUID — while
// every position the old guard inspects holds exactly what it wants.
//
//	byte 0   0xA9      continuation byte, never inspected
//	1..7     "bcdef01" 7 more, filling the first group
//	8        '-'
//	9..12    "2345"
//	13       '-'
//	14..17   "6789"
//	18       '-'
//	19..22   "abcd"
//	23       '-'
//	24..35   "ef0123456789"
const splitRuneRolloutName = "rollout-2026-04-13T02-00-13-ébcdef01-2345-6789-abcd-ef0123456789.jsonl"

// splitRuneRolloutTrimmed is what the function must return for that name once
// it refuses the candidate: the whole name with only ".jsonl" removed.
const splitRuneRolloutTrimmed = "rollout-2026-04-13T02-00-13-ébcdef01-2345-6789-abcd-ef0123456789"

// TestExtractIDFromFilename_RefusesACandidateThatIsNotAUUID pins the property
// that failed: the returned id is a session KEY — it is matched against Codex
// rollout files — so a candidate that is not a UUID must be refused outright.
// Returning a politely-shortened 36 bytes would match nothing while looking
// stable, which is worse than reporting the name unchanged.
func TestExtractIDFromFilename_RefusesACandidateThatIsNotAUUID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			// The reason this card exists. The old guard reads bytes 8, 13,
			// 18 and 23 and never looks at byte 0, so it returns a string
			// that is not valid UTF-8.
			name: "window opens mid-rune, dashes in every inspected position",
			in:   splitRuneRolloutName,
			want: splitRuneRolloutTrimmed,
		},
		{
			// Same hole, plain ASCII: 'z' is not a hex digit and sits in a
			// position the old guard never reads.
			name: "non-hex byte in an uninspected position",
			in:   "rollout-2026-04-13T02-00-13-z19d8491-627f-79e1-89a7-d32ed21ee93e.jsonl",
			want: "rollout-2026-04-13T02-00-13-z19d8491-627f-79e1-89a7-d32ed21ee93e",
		},
		{
			// A dash where a hex digit belongs. The count of dashes is right
			// and their positions are right; the group lengths are not.
			name: "extra dash inside a group",
			in:   "rollout-2026-04-13T02-00-13-019d8491-627f-79e1-89a7-d32ed21ee-3e.jsonl",
			want: "rollout-2026-04-13T02-00-13-019d8491-627f-79e1-89a7-d32ed21ee-3e",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractIDFromFilename(tc.in)
			if got != tc.want {
				t.Fatalf("extractIDFromFilename(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("extractIDFromFilename(%q) returned a string that is not valid UTF-8: %q", tc.in, got)
			}
		})
	}
}

// TestExtractIDFromFilename_AcceptsARealRolloutName is the known-negative
// control. A stricter parse that also stops recognising live Codex filenames
// would pass the test above and break discovery, so this must be seen to pass
// both before and after the repair.
func TestExtractIDFromFilename_AcceptsARealRolloutName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"the filename Codex writes", realRolloutName, realRolloutID},
		{"already bare, no .jsonl suffix", realRolloutID, realRolloutID},
		{"uppercase hex is still a UUID", "rollout-2026-04-13T02-00-13-019D8491-627F-79E1-89A7-D32ED21EE93E.jsonl", "019D8491-627F-79E1-89A7-D32ED21EE93E"},
		{"a nil UUID", "rollout-2026-04-13T02-00-13-00000000-0000-0000-0000-000000000000.jsonl", "00000000-0000-0000-0000-000000000000"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractIDFromFilename(tc.in); got != tc.want {
				t.Fatalf("extractIDFromFilename(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestIsUUID_LengthGate pins isUUID's own contract rather than the one the
// caller happens to give it. extractIDFromFilename always hands over exactly
// 36 bytes, so removing this gate changes nothing today and no test through
// that path can see it move — the second caller is the one that would pay.
func TestIsUUID_LengthGate(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{realRolloutID, true},
		{"", false},
		{realRolloutID[:35], false},                          // one short
		{realRolloutID + "0", false},                         // one long
		{realRolloutID[:8] + realRolloutID[9:], false},       // 35: a dash removed, groups slide left
		{realRolloutID[:8] + "-" + realRolloutID[8:], false}, // 37: a dash doubled
	}

	for _, tc := range cases {
		if got := isUUID(tc.in); got != tc.want {
			t.Fatalf("isUUID(%q) = %v, want %v (len %d)", tc.in, got, tc.want, len(tc.in))
		}
	}
}

// TestExtractIDFromFilename_ShortNamesAreReturnedWhole pins the untouched
// branch: a name with fewer than 36 bytes left after the suffix cut has no
// candidate to inspect and comes back as it arrived.
func TestExtractIDFromFilename_ShortNamesAreReturnedWhole(t *testing.T) {
	for _, in := range []string{"", "rollout.jsonl", "019d8491-627f-79e1-89a7-d32ed21ee93.jsonl"} {
		want := in
		if in == "rollout.jsonl" {
			want = "rollout"
		}
		if in == "019d8491-627f-79e1-89a7-d32ed21ee93.jsonl" {
			want = "019d8491-627f-79e1-89a7-d32ed21ee93"
		}
		if got := extractIDFromFilename(in); got != want {
			t.Fatalf("extractIDFromFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
