package engine

import (
	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

// AgentOverview is one row of `fu agent`: what fu knows about an adapter and,
// for the ones this machine actually has, how much of the desired projection
// is in place.
//
// It is deliberately a set of counts rather than a second drift list. `fu
// status` already names every diverging entry; this command answers the
// question that comes before that one -- which agents fu manages, and whether
// each is up to date -- and a user who wants the names runs status.
type AgentOverview struct {
	Name string
	// Detected is what makes an agent managed (SPEC rule 4). An adapter fu
	// supports but that is not installed here is listed by name alone: every
	// field below would describe a directory belonging to no one.
	Detected bool
	// SkillsDir is empty for an undetected agent. Reporting the path fu would
	// use invites the reader to create it by hand, which is not how an agent
	// becomes managed.
	SkillsDir string
	// DirMissing means nothing has been projected yet. SPEC rule 4 requires a
	// read-only command to say so rather than create the directory; the next
	// write command or `fu restore` does that.
	DirMissing bool
	// DirIsSymlink means reconcile refuses this agent wholesale (SPEC rule
	// 10), so the counts below stay zero: describing entries fu will never
	// touch would read as work it intends to do.
	DirIsSymlink bool
	// ScanErr is set when the directory could not be inspected at all.
	// Isolation stops at the agent, matching ScanAgent's own granularity.
	ScanErr string
	// Delivered counts live fu links; Broken counts fu links whose store-side
	// content is gone. A broken link projects a name with nothing behind it,
	// so it is not delivered.
	Delivered int
	Broken    int
	// Pending counts the link changes the next write command would make --
	// creations and removals alike, one per entry. A link fu.yaml no longer
	// wants is as much outstanding work as one it wants and does not have,
	// and leaving removals out answered "is this agent up to date?" with yes
	// for an agent still holding a link to a disabled or unregistered skill.
	//
	// The one thing excluded is a link the store cannot supply and that is
	// not there to remove either: fu will report that name, not act on it,
	// and it is counted in Unavailable instead. A broken link is not that
	// case and does count -- reconcile retires it first and only then reports
	// the missing content, so the name does change. Counting a removal here
	// while the same entry is also counted in Delivered is deliberate: the
	// link does deliver content today, and it is still going away.
	Pending int
	// Unmanaged counts entries fu did not create and never touches (SPEC
	// rule 2). Uninspectable counts entries the scan could not classify at
	// all; they are their own column because fu does not know what they are.
	Unmanaged     int
	Uninspectable int
	// Blocked counts skills fu.yaml wants delivered whose path is already
	// held by content fu did not create, and Unavailable counts ones the
	// store no longer holds. Neither is Pending -- fu will make no change for
	// either -- and neither is visible in the columns: a blocked name is one
	// more Unmanaged entry, indistinguishable from a harmless foreign
	// directory, and an unavailable one appears nowhere at all. Both need the
	// user to act, so both get a note under the table.
	//
	// Blocked counts the disabled form too (ReportDisabledForeign), which is
	// the same state one switch over: fu.yaml says off, and something fu did
	// not create sits at the name, so the skill may well still be loaded
	// every session. Diff calls that kind actionable and printResult gives it
	// its own line; leaving it out here would have `fu agent` report ok for
	// an agent that is still loading a skill the user turned off.
	Blocked     int
	Unavailable int
}

// AgentOverviews describes every adapter fu knows about, detected or not. It
// is read-only in the strict sense SPEC §9 requires: no lock, no directory
// creation, no write of any kind.
func AgentOverviews(st *store.Store, cfg *store.Config, known []agent.Agent) []AgentOverview {
	overviews := make([]AgentOverview, 0, len(known))
	for _, a := range known {
		overviews = append(overviews, agentOverview(st, cfg, a))
	}
	return overviews
}

func agentOverview(st *store.Store, cfg *store.Config, a agent.Agent) AgentOverview {
	overview := AgentOverview{Name: a.Name()}
	if !a.Detect() {
		return overview
	}
	overview.Detected = true
	overview.SkillsDir = a.SkillsDir()
	state, err := ScanAgent(a, st.SkillsDir())
	if err != nil {
		overview.ScanErr = err.Error()
		return overview
	}
	overview.DirMissing = state.ParentMissing
	overview.DirIsSymlink = state.ParentIsSymlink
	if state.ParentIsSymlink {
		return overview
	}
	for _, entry := range state.Entries {
		switch {
		case entry.Kind == KindUnknown:
			overview.Uninspectable++
		case entry.Kind == KindFuLink && entry.Broken:
			overview.Broken++
		case entry.Kind == KindFuLink:
			overview.Delivered++
		default:
			overview.Unmanaged++
		}
	}
	// Desired's reserved and invalid findings are dropped here on purpose:
	// both are properties of fu.yaml that `fu status` reports by name, and
	// neither is a count of anything in this directory.
	desired, _, _ := Desired(cfg, a)
	// Counted from the raw Diff rather than statusDrift's rewrite, sharing
	// only its rebuild-pair predicate. The two readers want different things
	// from the same actions: statusDrift describes each entry's state, and so
	// rewrites a broken link's whole pair to one ReportMissing; this counts
	// changes fu will make, and reconcile does retire that link before
	// reporting the content gone. A bare CreateLink the store cannot satisfy
	// is the case where nothing changes, and storeSideMissing is the same
	// question statusDrift asks to detect it.
	actions := Diff(desired, state, st.SkillsDir())
	for index := 0; index < len(actions); index++ {
		if isRebuildPair(actions, index) {
			// One link changes, whether or not the store can supply the
			// replacement half: the removal happens either way.
			overview.Pending++
			index++
			continue
		}
		action := actions[index]
		switch action.Type {
		case RemoveLink:
			overview.Pending++
		case CreateLink:
			if storeSideMissing(action).Type == CreateLink {
				overview.Pending++
			} else {
				overview.Unavailable++
			}
		case ReportConflict, ReportDisabledForeign:
			overview.Blocked++
		}
	}
	return overview
}
