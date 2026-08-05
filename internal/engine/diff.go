// internal/engine/diff.go
package engine

import (
	"path/filepath"
	"sort"

	"github.com/cosensexyz/fu/internal/skill"
)

type ActionType int

const (
	CreateLink ActionType = iota
	RemoveLink
	ReportConflict        // desired path occupied by foreign content — never overwrite
	ReportForeign         // name fu.yaml has no opinion on at all — informational only, reserved for a future `fu status`
	ReportDisabledForeign // name fu.yaml tracks and wants off, but its path is occupied by foreign content — actionable, unlike ReportForeign
	ReportReserved        // desired skill name collides with an agent's reserved entry — never linked (SPEC rule 11)
	ReportInvalid         // desired skill name fails validation — never turned into a path component (round 2 finding 3)
)

type Action struct {
	Type      ActionType
	AgentName string
	Skill     string
	LinkPath  string
	Target    string // link target, set for CreateLink only
}

// Diff computes actions turning actual into desired for one agent
// (DESIGN §2 state matrix). Pure function: no filesystem access, sorted
// iteration for determinism. Callers must handle ParentIsSymlink before
// calling.
func Diff(desired map[string]bool, state AgentState, storeSkillsDir string) []Action {
	var acts []Action
	agentDir := state.Agent.SkillsDir()
	name := state.Agent.Name()
	byName := map[string]Entry{}
	for _, e := range state.Entries {
		byName[e.Name] = e
	}
	skills := make([]string, 0, len(desired))
	for s := range desired {
		skills = append(skills, s)
	}
	sort.Strings(skills)
	for _, skillName := range skills {
		// fu.yaml is hand-editable today, and the next plan's clone/pull
		// will populate it from a network source -- making every name in
		// desired a trust boundary. A name that fails validation must
		// never be turned into a path component below: skipping straight
		// to the report, before link/want are ever computed, means an
		// invalid name is never even joined into a path, not merely
		// "joined but then discarded" (round 2 finding 3). This does not
		// apply to the removal direction (the loop below, over
		// state.Entries): those names come from ScanAgent's os.ReadDir, so
		// the OS itself already guarantees each one is a single existing
		// path component.
		//
		// This is now a backstop, not the only guard (round 3 finding 2):
		// Desired already runs this identical check and excludes a failing
		// name from desired entirely before Diff is ever called in
		// production, specifically so the trailing loop below -- which
		// skips every name *present* in desired, valid or not -- still gets
		// a chance to see a matching disk entry and remove it if it is a
		// genuine fu link. Reaching this arm at all with a name that also
		// has a disk entry (only possible if some future caller hands Diff
		// a desired map that skipped Desired's filtering) reproduces that
		// same trap on its own: continuing here still leaves the name
		// "known" to desired, so the trailing loop still skips it. Kept
		// here anyway as cheap, explicit insurance for Diff called in
		// isolation, the same reasoning isSinglePathComponent's own comment
		// below already documents for itself.
		if err := skill.ValidateName(skillName); err != nil || !isSinglePathComponent(skillName) {
			acts = append(acts, Action{ReportInvalid, name, skillName, "", ""})
			continue
		}
		on := desired[skillName]
		e, present := byName[skillName]
		link := filepath.Join(agentDir, skillName)
		want := filepath.Join(storeSkillsDir, skillName)
		switch {
		case on && !present:
			acts = append(acts, Action{CreateLink, name, skillName, link, want})
		// The `present` guards below are not redundant with the case above:
		// KindFuLink is EntryKind's zero value, so an absent entry would
		// otherwise match the rebuild case if these arms were ever reordered.
		case on && present && e.Kind == KindFuLink && (e.Broken || e.LinkTarget != want):
			acts = append(acts,
				Action{RemoveLink, name, skillName, link, ""},
				Action{CreateLink, name, skillName, link, want})
		case on && present && e.Kind == KindForeign:
			acts = append(acts, Action{ReportConflict, name, skillName, link, ""})
		case !on && present && e.Kind == KindFuLink:
			acts = append(acts, Action{RemoveLink, name, skillName, link, ""})
		// DESIGN §2's state matrix row six ("不应有链接 | 未纳管条目 |
		// ReportForeign") does not distinguish "no entry in desired" from
		// "present but explicitly off" -- both are "不应有链接". The
		// trailing loop below only ever sees the former (it skips every
		// name known to desired, value notwithstanding), so this arm is
		// the only place the latter is ever reported (round 2 finding 2).
		// Left unhandled, a disabled skill sitting behind real foreign
		// content produced zero actions and an entirely empty Result --
		// which is also what let a misclassified fu link (round 2 finding
		// 1) disappear completely instead of at least surfacing as
		// unexpectedly foreign.

		// This arm used to emit plain ReportForeign, landing in the same
		// Result.Foreign bucket the trailing loop below feeds. That conflated
		// two situations that only look alike on disk: a name here is one
		// fu.yaml actually tracks and reports "off" for -- the user (directly,
		// or a previous disable) asked for this skill to be off for this
		// agent, so foreign content still sitting at its path directly answers
		// "did my disable work?" (no: fu cannot touch a path it does not own,
		// so the skill may still be loaded). A name the trailing loop finds,
		// by contrast, is one fu.yaml never mentions at all -- nothing the
		// user did caused it, and it would be noise on every write command.
		// Reporting both the same way kept this actionable half exactly as
		// silent as the merely informational half: Result.Foreign is
		// deliberately never printed by printResult (reserved for a future
		// `fu status`), so a disabled skill behind foreign content produced a
		// confirmation and nothing else -- even though it is exactly the case
		// the confirmation's "takes effect" claim cannot make good on.
		// ReportDisabledForeign gives this half its own channel, so it can be
		// surfaced without also dumping the trailing loop's inventory on every
		// command.
		case !on && present && e.Kind == KindForeign:
			acts = append(acts, Action{ReportDisabledForeign, name, skillName, link, ""})
		}
	}
	// Every name here is, by construction, absent from desired entirely --
	// the loop above already handled every name desired has an opinion on,
	// on or off. fu has no record of it at all, so a foreign entry found
	// here stays on the informational-only ReportForeign/Result.Foreign
	// channel (reserved for a future `fu status`), never the actionable
	// ReportDisabledForeign above.
	for _, e := range state.Entries {
		if _, known := desired[e.Name]; known {
			continue
		}
		link := filepath.Join(agentDir, e.Name)
		if e.Kind == KindFuLink {
			acts = append(acts, Action{RemoveLink, name, e.Name, link, ""})
		} else {
			acts = append(acts, Action{ReportForeign, name, e.Name, link, ""})
		}
	}
	return acts
}

// isSinglePathComponent reports whether name is safe to use as exactly one
// path component: no separator anywhere in it, and not a "." or ".."
// navigation entry. skill.ValidateName's naming-rule regex already forbids
// every character this would ever catch (it only allows lowercase
// alphanumerics and single internal hyphens), so this check is redundant
// against ValidateName as it stands today -- it is here as cheap, explicit
// insurance at the one place names become path components, in case the
// naming rules are ever relaxed without this call site being revisited
// (round 2 finding 3).
func isSinglePathComponent(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name
}
