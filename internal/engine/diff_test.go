// internal/engine/diff_test.go
package engine

import (
	"path/filepath"
	"testing"
)

func TestDiffStateMatrix(t *testing.T) {
	store := "/fu/store/skills"
	a := fakeAgent{"claude", "/home/.claude/skills"}
	link := func(s string) string { return filepath.Join(a.SkillsDir(), s) }
	target := func(s string) string { return filepath.Join(store, s) }

	cases := []struct {
		label   string
		desired map[string]bool
		entries []Entry
		want    []Action
	}{
		{"desired+absent → create",
			map[string]bool{"alpha": true}, nil,
			[]Action{{CreateLink, "claude", "alpha", link("alpha"), target("alpha")}}},
		{"desired+correct → noop",
			map[string]bool{"alpha": true},
			[]Entry{{Name: "alpha", Kind: KindFuLink, LinkTarget: target("alpha")}},
			nil},
		{"desired+broken → rebuild",
			map[string]bool{"alpha": true},
			[]Entry{{Name: "alpha", Kind: KindFuLink, LinkTarget: target("alpha"), Broken: true}},
			[]Action{{RemoveLink, "claude", "alpha", link("alpha"), ""},
				{CreateLink, "claude", "alpha", link("alpha"), target("alpha")}}},
		{"desired+foreign → conflict, never overwrite",
			map[string]bool{"alpha": true},
			[]Entry{{Name: "alpha", Kind: KindForeign}},
			[]Action{{ReportConflict, "claude", "alpha", link("alpha"), ""}}},
		{"undesired+fu link → remove",
			map[string]bool{"alpha": false},
			[]Entry{{Name: "alpha", Kind: KindFuLink, LinkTarget: target("alpha")}},
			[]Action{{RemoveLink, "claude", "alpha", link("alpha"), ""}}},
		{"unknown fu link (skill removed) → remove",
			map[string]bool{},
			[]Entry{{Name: "ghost", Kind: KindFuLink, LinkTarget: target("ghost"), Broken: true}},
			[]Action{{RemoveLink, "claude", "ghost", link("ghost"), ""}}},
		{"unknown foreign → report only",
			map[string]bool{},
			[]Entry{{Name: "manual", Kind: KindForeign}},
			[]Action{{ReportForeign, "claude", "manual", link("manual"), ""}}},
	}
	for _, c := range cases {
		got := Diff(c.desired, AgentState{Agent: a, Entries: c.entries}, store)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %d actions %+v, want %d", c.label, len(got), got, len(c.want))
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s[%d]: got %+v want %+v", c.label, i, got[i], c.want[i])
			}
		}
	}
}

// TestDiffSelfReviewCases covers state combinations the DESIGN §2 matrix
// implies but the primary table above does not spell out: a rename-style
// stale target distinct from a broken link, a disabled skill sitting
// behind foreign content, multi-skill ordering/pairing, and the empty
// input case.
func TestDiffSelfReviewCases(t *testing.T) {
	store := "/fu/store/skills"
	a := fakeAgent{"claude", "/home/.claude/skills"}
	link := func(s string) string { return filepath.Join(a.SkillsDir(), s) }
	target := func(s string) string { return filepath.Join(store, s) }

	cases := []struct {
		label   string
		desired map[string]bool
		entries []Entry
		want    []Action
	}{
		{"desired+intact fu link but stale target (rename leftover) → rebuild",
			map[string]bool{"alpha": true},
			[]Entry{{Name: "alpha", Kind: KindFuLink, LinkTarget: target("old-alpha")}},
			[]Action{{RemoveLink, "claude", "alpha", link("alpha"), ""},
				{CreateLink, "claude", "alpha", link("alpha"), target("alpha")}}},
		// Round 2 finding 2: this case used to assert "left alone, no
		// report" -- that was the exact gap the finding names, the state
		// matrix's sixth row (DESIGN §2: "不应有链接 | 未纳管条目 |
		// ReportForeign") applies here just as much as it does to a name
		// entirely absent from desired (the "unknown foreign → report
		// only" case above): "不应有链接" covers both "no entry in desired"
		// and "present but explicitly off," and the matrix does not
		// distinguish them. Before that fix this was reachable only through
		// the main loop, which had no arm for on=false+foreign at all, so
		// it fell through silently; unlike the absent-from-desired case,
		// it was never even reached by the trailing "leftover" loop, since
		// that loop skips every name present in desired regardless of
		// value.
		//
		// A later pass split this arm again: ReportForeign (still used for
		// the entirely-unknown-name case right above) and this case only
		// looked alike on disk. This one is a name fu.yaml actually tracks
		// and reports "off" for -- directly actionable, since it answers
		// "did my disable work?" (no: fu cannot touch a path it does not
		// own) -- while the unknown-name case is inventory fu never had an
		// opinion on. See TestDiffDisabledForeignDistinctFromUnknownForeign
		// below for the two (plus the sibling on+foreign conflict case)
		// pinned side by side, not just this one reproduction.
		{"desired+disabled with foreign content → reported as disabled-foreign, still left alone",
			map[string]bool{"alpha": false},
			[]Entry{{Name: "alpha", Kind: KindForeign}},
			[]Action{{ReportDisabledForeign, "claude", "alpha", link("alpha"), ""}}},
		{"empty desired and empty actual → no actions, no panic",
			map[string]bool{}, nil, nil},
	}
	for _, c := range cases {
		got := Diff(c.desired, AgentState{Agent: a, Entries: c.entries}, store)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %d actions %+v, want %d", c.label, len(got), got, len(c.want))
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s[%d]: got %+v want %+v", c.label, i, got[i], c.want[i])
			}
		}
	}
}

// TestDiffDisabledForeignDistinctFromUnknownForeign pins the split itself
// (round 2's own recurring lesson: earlier fixes each pinned only the one
// reported reproduction, leaving neighbouring inputs uncovered), not just
// the one reported reproduction: three skills, each sitting behind real
// foreign content, must land on three different outcomes depending only on
// what desired says about each name -- never confused with one another by
// Diff's single foreign-content check.
func TestDiffDisabledForeignDistinctFromUnknownForeign(t *testing.T) {
	store := "/fu/store/skills"
	a := fakeAgent{"claude", "/home/.claude/skills"}
	link := func(s string) string { return filepath.Join(a.SkillsDir(), s) }

	desired := map[string]bool{"alpha": false, "beta": true}
	entries := []Entry{
		// Known to fu.yaml, off: the actionable case this test exists to
		// pin -- the user's own disable left this content untouched, and
		// the skill may therefore still be loaded.
		{Name: "alpha", Kind: KindForeign},
		// Known to fu.yaml, on: the sibling arm (the original ReportConflict
		// case) must be unaffected by the new split -- included here, not
		// just covered elsewhere, so the two are proven not to bleed into
		// each other within the very same Diff call.
		{Name: "beta", Kind: KindForeign},
		// Absent from desired entirely: fu has no opinion on this name at
		// all, so it must stay on the informational-only channel, not the
		// actionable one -- the exact distinction this test is for.
		{Name: "mystery", Kind: KindForeign},
	}
	want := []Action{
		{ReportDisabledForeign, "claude", "alpha", link("alpha"), ""},
		{ReportConflict, "claude", "beta", link("beta"), ""},
		{ReportForeign, "claude", "mystery", link("mystery"), ""},
	}

	got := Diff(desired, AgentState{Agent: a, Entries: entries}, store)
	if len(got) != len(want) {
		t.Fatalf("got %d actions %+v, want %d: %+v", len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %+v want %+v", i, got[i], want[i])
		}
	}

	// ReportDisabledForeign must be a genuinely distinct value, not an
	// accidental alias of ReportForeign -- the struct-equality checks above
	// already pin each Action's Type, but this makes the invariant the
	// whole test depends on explicit even if the ActionType iota block is
	// ever reordered.
	if ReportDisabledForeign == ReportForeign {
		t.Fatal("ReportDisabledForeign and ReportForeign must be distinct ActionType values")
	}
}

// TestDiffMultiSkillOrderingAndPairing exercises several skills at once:
// the desired-side loop must emit actions in sorted-by-skill-name order
// (map iteration is otherwise randomized), a rebuild's RemoveLink/CreateLink
// pair must stay adjacent rather than interleaving with other skills, and
// leftover entries absent from desired must preserve the input Entries
// order (proving Diff does not itself introduce reordering there — real
// callers get sorted input from ScanAgent's os.ReadDir).
func TestDiffMultiSkillOrderingAndPairing(t *testing.T) {
	store := "/fu/store/skills"
	a := fakeAgent{"claude", "/home/.claude/skills"}
	link := func(s string) string { return filepath.Join(a.SkillsDir(), s) }
	target := func(s string) string { return filepath.Join(store, s) }

	desired := map[string]bool{"zeta": true, "alpha": true, "mango": false, "beta": true}
	// Entries deliberately not in alphabetical order, to prove Diff
	// preserves input order for the "unknown to desired" leftovers
	// rather than coincidentally sorting them.
	entries := []Entry{
		{Name: "orphan", Kind: KindFuLink, LinkTarget: target("orphan")}, // not in desired at all
		{Name: "beta", Kind: KindFuLink, LinkTarget: target("beta")},     // on, correct → noop
		{Name: "zeta", Kind: KindFuLink, LinkTarget: target("zeta"), Broken: true},
		{Name: "extra", Kind: KindForeign},                             // not in desired at all
		{Name: "mango", Kind: KindFuLink, LinkTarget: target("mango")}, // off, present → remove
	}
	want := []Action{
		{CreateLink, "claude", "alpha", link("alpha"), target("alpha")},
		{RemoveLink, "claude", "mango", link("mango"), ""},
		{RemoveLink, "claude", "zeta", link("zeta"), ""},
		{CreateLink, "claude", "zeta", link("zeta"), target("zeta")},
		{RemoveLink, "claude", "orphan", link("orphan"), ""},
		{ReportForeign, "claude", "extra", link("extra"), ""},
	}

	got := Diff(desired, AgentState{Agent: a, Entries: entries}, store)
	if len(got) != len(want) {
		t.Fatalf("got %d actions %+v, want %d: %+v", len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %+v want %+v", i, got[i], want[i])
		}
	}
}

// Round 2 finding 3: fu.yaml is hand-editable (and, from the next plan
// onward, populated by clone/pull from a network source), so its skill
// names are a trust boundary -- but Diff used to join every name in
// desired straight into both the link path and the store-side target with
// no validation at all. Reproduced against the compiled binary: a
// hand-added `fu.yaml` entry literally named "../evil" (with real content
// planted at the escaped store-side location) made `fu new beta` plant a
// symlink one level *above* the agent's skills directory --
// ~/.claude/evil, not ~/.claude/skills/evil -- while printing only
// "created beta" and exiting 0. skill.ValidateName's naming-rule regex
// already forbids '/' and any dot-based escape on its own (confirmed
// below: "../evil" is invalid by the regex alone, with or without the
// second check), so the isSinglePathComponent check is deliberately
// redundant today -- cheap, explicit insurance against a future change to
// the naming rules, not a currently-live second hole.
func TestDiffRejectsInvalidSkillNameWithoutTouchingPaths(t *testing.T) {
	store := "/fu/store/skills"
	a := fakeAgent{"claude", "/home/.claude/skills"}

	cases := []struct {
		label string
		name  string
	}{
		{"parent-escape", "../evil"},
		{"bare dot-dot", ".."},
		{"absolute-looking", "/etc/passwd"},
		{"uppercase (fails the naming-rule regex, not the path check)", "Evil"},
		{"empty", ""},
	}
	for _, c := range cases {
		desired := map[string]bool{c.name: true}
		got := Diff(desired, AgentState{Agent: a}, store)
		if len(got) != 1 || got[0].Type != ReportInvalid || got[0].Skill != c.name {
			t.Fatalf("%s: want exactly one ReportInvalid action for %q, got %+v", c.label, c.name, got)
		}
		if got[0].LinkPath != "" || got[0].Target != "" {
			t.Fatalf("%s: an invalid name must never be joined into a path, got %+v", c.label, got[0])
		}
	}
}

// The removal direction (entries already on disk, discovered via
// ScanAgent's os.ReadDir) can never carry an escaping name -- the OS
// itself guarantees a directory entry's name has no '/' -- so Diff must
// not apply the same validation there; skill.ValidateName would also
// reject perfectly legitimate foreign entries (e.g. a dotfile) that were
// never fu's to begin with and must be reported as foreign, not silently
// dropped for failing a check that was never meant for them.
func TestDiffInvalidNameCheckDoesNotApplyToActualEntries(t *testing.T) {
	store := "/fu/store/skills"
	a := fakeAgent{"claude", "/home/.claude/skills"}
	link := func(s string) string { return filepath.Join(a.SkillsDir(), s) }

	got := Diff(map[string]bool{}, AgentState{Agent: a, Entries: []Entry{
		{Name: ".hidden-real-dotfile", Kind: KindForeign},
	}}, store)
	want := []Action{{ReportForeign, "claude", ".hidden-real-dotfile", link(".hidden-real-dotfile"), ""}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
