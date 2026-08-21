// internal/engine/status.go
package engine

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

// StatusReport is the read-only counterpart of Result: the same classification
// the write path computes on its way to acting, assembled without acting on it.
// Every field is filled by Status and consumed only for display.
type StatusReport struct {
	Agents   []AgentStatus
	Store    StoreStatus
	Recovery RecoveryInventory
	Staging  StagingInventory
}

// StagingInventory counts what the staging area is holding, bucketed the same
// way RecoveryInventory is -- by what the user can do about each -- but with
// three buckets where recovery has four. The missing one is Retained: staging
// holds no authority SPEC §9 promises, only work in progress and what a
// process exit left of it. Nothing here is collectable either, in recovery's
// sense of the word: `fu gc` never looks at staging at all.
//
// That every cleanup path in staging is an in-process defer is exactly why the
// second bucket exists. A source scratch is removed by its own constructor's
// failure path or by Close (source/scratch.go); a private staged root by the
// reservation's cleanup (ownedtree.go). All of them are correct while the
// process lives and none of them run again afterwards, so a process exit
// strands names that no later run enumerates the directory to collect.
type StagingInventory struct {
	// Blocked counts entries a recovery pass settles: a pending transaction's
	// published staged root and its private reservation, and a pending config
	// exchange's candidate and active swap name. RecoverPending and
	// RecoverConfigExchanges run from every write command and from
	// `fu restore`, never from `fu gc`.
	Blocked int
	// Uncollectable counts what nothing collects today. This is the gap
	// DESIGN §6 records as still open for source scratch: reporting it is the
	// half that can be delivered without ownership evidence, and offering to
	// delete it is the half that cannot.
	Uncollectable int
	// Unmatched counts public staging names no pending record claims -- the
	// content `fu new` and `fu add` refuse to run against, naming the path
	// when they do. Those refusals are a better remedy than a count, which is
	// why this is a separate bucket rather than part of Uncollectable, but
	// they only appear once the user tries the command: someone who has just
	// been refused and runs `fu status` to find out what is in the way used to
	// see nothing at all.
	Unmatched int
}

// AgentStatus carries one agent's reality as scanned, plus the drift between
// that reality and what fu.yaml asks for.
type AgentStatus struct {
	Name string
	// DirMissing means the agent's skills directory does not exist yet, so
	// nothing has been projected into it. SPEC rule 4 requires a read-only
	// command to say so rather than create it.
	DirMissing bool
	// DirIsSymlink means reconcile refuses this agent outright (SPEC rule 10):
	// fu never writes through a symlinked skills directory.
	DirIsSymlink bool
	// ScanErr is set when this agent could not be inspected at all. Isolation
	// stops at the agent, matching ScanAgent's own granularity.
	ScanErr string
	// Drift holds this agent's classified divergence from fu.yaml: Diff's
	// unexecuted actions, plus the reserved and invalid findings Desired
	// itself returns and Diff never sees -- a reserved or invalid skill name
	// never becomes a path component, so Diff cannot report it.
	Drift []Action
}

// StoreStatus carries the two store-side facts a user needs before deciding
// whether to run anything: whether the worktree holds edits no commit has
// recorded, and whether a previous command died mid-transaction.
type StoreStatus struct {
	DirtyPaths []string
	Pending    []PendingOperation
}

// PendingOperation names an unfinished transaction in the terms a user knows:
// the command that started it and what it was operating on.
type PendingOperation struct {
	Op   string
	Name string
}

// RecoveryInventory counts what the recovery directory is holding, bucketed by
// what the user can do about each: run `fu gc`, settle an unfinished write
// first, nothing at all, or judge by hand. It is a count, not a listing -- the
// point is to answer "is anything accumulating, and can I act on it", not to
// enumerate machine-local files.
type RecoveryInventory struct {
	// Collectable counts what `fu gc` would collect on its next run: the
	// journal files of every settled transaction family, the payload a
	// completed rm family's manifest describes, and the config exchange
	// bookkeeping gc's own predicates admit.
	//
	// "Would be entitled to collect" rather than "will certainly succeed" --
	// see collectableRecoveryNames for the two degraded states in which gc
	// re-reads more than this derivation does and refuses.
	Collectable int
	// Blocked counts entries in those same families that `fu gc` would leave
	// exactly where they are, because they wait on a recovery pass instead:
	// RecoverPending settles them -- from any write command, or from
	// `fu restore` -- and PruneRecovery (`fu gc`) deliberately never calls it.
	// Reporting one of these as collectable tells the user to run a command
	// and watch a count not move.
	Blocked int
	// Retained counts the authority SPEC §9 promises for restoring an adopted
	// entry in place. gc never deletes these, by design.
	Retained int
	// Uncollectable counts what nothing collects today.
	Uncollectable int
}

// Prefix tables for the inventory, plus the families the switch below decides
// outside them.
//
// Bucketing a listed name is a set membership test in every arm: prefix
// membership in one of these two tables, or exact membership in a set built
// before the walk -- pendingPayloadClaims, collectableRecoveryNames,
// store.PendingConfigExchangeRecords, store.CollectableConfigExchangeNames.
// The one exception is store.CollectableConfigArchiveNames, which lstats each
// candidate, because gc's own condition for unlinking an archive is that the
// name still resolves to the identity it states, and re-deriving that without
// asking is how the two would come to disagree.
//
// Building those sets is where the reads happen, and they are reads of fu's own
// journal rather than of the objects it describes. pendingPayloadClaims derives
// from records PendingTxns already parsed; collectableRecoveryNames scans the
// journal filenames and decodes one revision per completed rm family. Neither
// opens a payload. That is the line actually being held: the inventory agrees
// with gc by asking gc's own questions, and still never reads the content it is
// counting.
var (
	// retainedPrefixes name the authority SPEC §9 promises for restoring an
	// adopted entry in place. gc never deletes these, by design.
	retainedPrefixes = []string{"adopt-archive-", "adopt-link-"}
	// uncollectablePrefixes name objects nothing collects today. A new,
	// add, or adopt transaction's rollback payload -- quarantined under
	// rollback- while its own transaction is still open, see the switch
	// below -- ends up archived permanently under .fu-archive- once that
	// transaction resolves normally: ArchiveRecoveryPayloadOwned
	// (ownedtree.go) deliberately never unlinks it, and SPEC §5.1's gc row
	// agrees that other recovery content is left alone. A rollback- name
	// whose owning transaction's journal is gone instead never gets that
	// resolution -- addRecoveryConflictRemedy's own abandon-recovery advice
	// (txn.go) can produce exactly that state -- so the switch below counts
	// it here too, even though rollback- is not itself one of the two
	// prefixes in this table. .fu-retired-entry- has two producers, but only
	// one is ever visible here: reclaimConfigExchangeFile
	// (store/config_exchange.go) retires an unreclaimed record or marker
	// under a random name (RetireNameAt/randomRetiredName, store/retire.go)
	// directly in recovery/, so no later run can ever attribute it back to
	// fu and collect it by name. RemoveOwnedTreeAt's own .fu-retired-entry-
	// names (ownedCleanupRetiredName, store/retire.go) are deterministic
	// instead, but they land one level below recovery/, inside the owned
	// tree being removed, so this top-level listing never observes that
	// form. Whether the permanent retention above should end is a product
	// decision this report does not make; what matters here is narrower --
	// these two prefixes only grow, so acting on one is something only the
	// reader can judge and do by hand.
	uncollectablePrefixes = []string{".fu-archive-", ".fu-retired-entry-"}
	// stagingResiduePrefixes name what a process exit can leave under staging
	// that no later run collects. ".fu-src-" covers all three source scratch
	// forms at once -- the live root, the constructor's retired orphan and
	// Close's quarantine (source/scratch.go) -- because the distinction between
	// them matters to the process that made them and not to a reader who can
	// act on none of them.
	//
	// ".fu-new-" and ".fu-config-candidate-" appear here as well as in the
	// claimed set below, and the order of the switch is what separates the two
	// cases: a private staged root or an exchange candidate named by a pending
	// record is work recovery finishes, while the same shape with no record
	// behind it is what a crash before the journal write leaves, and nothing
	// can attribute that back to fu. ".fu-config-swap" is the same story at a
	// fixed name -- DESIGN §6 has fu refuse to take over an active name it
	// holds no matching record for, treating it as external occupation.
	stagingResiduePrefixes = []string{
		".fu-src-",
		".fu-new-",
		".fu-retired-staging-",
		".fu-config-candidate-",
		".fu-config-swap",
	}
)

// statusDrift turns Diff's actions into findings a reader can act on. Diff
// answers a broken or misspelled fu link with the pair that repairs it --
// RemoveLink then CreateLink for the same entry (diff.go) -- which is a work
// list for the write path; read out as a description it says one link is both
// stale and missing. The pair is collapsed here, in the report, so Diff's own
// output stays exactly what reconcile depends on.
//
// Which single fact it collapses to is the scanned entry's to decide, and
// status has that entry. A broken fu link is one whose target no longer
// exists, and a fu link's target is store content by construction (ownsLink,
// scan.go), so what is wrong is that the store no longer holds the skill:
// ReportMissing, the same finding reconcile records when its own CreateLink
// half finds nothing at the target (reconcile.go), and the one wording
// driftLabel has for this state with no way to reach it. Anything else
// reaching this arm is a link that does still resolve into the store but is
// not spelled the way fu writes it, which reconcile silently rewrites: stale,
// and only stale.
func statusDrift(actions []Action, state AgentState) []Action {
	broken := make(map[string]bool, len(state.Entries))
	for _, entry := range state.Entries {
		broken[entry.Name] = entry.Broken
	}
	drift := make([]Action, 0, len(actions))
	for index := 0; index < len(actions); index++ {
		action := actions[index]
		next := index + 1
		if action.Type != RemoveLink || next == len(actions) ||
			actions[next].Type != CreateLink || actions[next].Skill != action.Skill {
			drift = append(drift, storeSideMissing(action))
			continue
		}
		if broken[action.Skill] {
			// Carried on the CreateLink half, the shape reconcile's own
			// Missing findings have: that action names the target the store
			// was supposed to be holding.
			missing := actions[next]
			missing.Type = ReportMissing
			drift = append(drift, missing)
		} else {
			drift = append(drift, action)
		}
		index = next
	}
	return drift
}

// storeSideMissing rewrites a bare CreateLink whose store-side target is not
// there into the finding reconcile already reports for that state.
//
// Diff answers "fu.yaml wants this link and the agent does not have it" with
// CreateLink, which driftLabel renders as "missing link" -- a description that
// tells the reader `fu restore` will create it. When the store no longer holds
// the skill, restore cannot: run against exactly this state it says "alpha is
// enabled but the store no longer holds its content" (reconcile.go's own
// Missing finding), so the two commands described one state in two
// incompatible ways. This is the same defect commit 943b147 fixed for a broken
// link, on the arm that commit did not reach, and the accurate wording already
// exists -- ReportMissing, which driftLabel can already render.
//
// One Lstat per finding, read-only, and only for CreateLink. A non-directory
// at the target counts as missing for the same reason reconcile treats it that
// way: there is no skill directory to link to.
//
// It resolves a store pathname against the live filesystem, and it is the one
// read-only path in this package that does rather than reading through a pinned
// descriptor (review round 27, minor). Worth naming, because everywhere else
// the pathname/descriptor distinction is load bearing -- a write that resolves
// by name can be redirected between the check and the act. Nothing here acts.
// `fu status` takes no lock and opens no write session, so it has no pinned
// descriptor to use and no claim to make about the instant after this read: the
// whole report is a snapshot, and a target that appears or vanishes immediately
// afterwards makes some other line stale in exactly the same way. What this
// produces is a description, and the worst a race can do to it is describe the
// store as it was a moment earlier.
func storeSideMissing(action Action) Action {
	if action.Type != CreateLink || action.Target == "" {
		return action
	}
	info, err := os.Lstat(action.Target)
	if err == nil && info.IsDir() {
		return action
	}
	if err != nil && !os.IsNotExist(err) {
		// Anything other than "not there" is a question this read-only report
		// cannot answer, so the original finding stands rather than being
		// upgraded on a guess.
		return action
	}
	action.Type = ReportMissing
	return action
}

func hasAnyPrefix(name string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

type recoveryCollection struct {
	// payloads names the removed- and .fu-retired-dir- objects a completed,
	// unpruned family describes -- the ones `fu gc` reclaims on the way to
	// pruning that family.
	payloads map[string]bool
	// journal names the txn-* files `fu gc` would delete on its next run: a
	// settled family's revisions, its completion marker, and the prune record
	// an interrupted prune already wrote.
	journal map[string]bool
	// attributed names every txn-* file the scan resolved to some family,
	// collectable or not. Its complement inside the txn- prefix is the set no
	// producer in fu writes and no consumer reads -- a hand-dropped
	// "txn-leftover.txt" -- which without this set fell into no bucket at all
	// and left `fu status` claiming a directory holding it was clean.
	attributed map[string]bool
	// damaged names the txn-* files of every family the scan found invalid --
	// two prune records for one transaction, say. It is a subset of attributed
	// and exists because the two need opposite answers: an attributed name is
	// normally a pending family's, which report.Store.Pending already names by
	// op and skill, so counting it here would state one fact twice. A damaged
	// family is named by no section -- PendingTxns fails on it, so it reaches
	// neither -- and without this set its files were attributed, skipped, and
	// counted nowhere at all.
	damaged map[string]bool
	// scanned records that the derivation actually ran. Every set is empty
	// when it did not, and an empty set must not be read as "nothing here is
	// collectable": that would turn one unreadable journal into a directory
	// full of names reported as permanent litter.
	scanned bool
}

// collectableRecoveryNames derives what `fu gc` would actually be entitled to
// collect on its next run: the payloads a *completed, unpruned* family
// describes, and the journal files of every family gc has left to prune.
//
// This is the other half of pendingPayloadClaims below. That one answers "what
// must gc not collect"; this one answers "what can it". Both questions have to
// be asked, because the inventory's Collectable bucket is a promise about a
// command's behaviour and gc reclaims a removed- payload in exactly one place
// -- while pruning the family whose manifest describes it (txn_prune.go) -- and
// resumes a .fu-retired-dir- from that same manifest. Nothing else in gc ever
// looks at either prefix. With no family on disk both are inert, and counting
// them Collectable told the user to run a command and watch a number not move.
//
// The journal files are counted for the mirror-image reason. Leaving them out
// was the arrangement DESIGN §4.1 originally recorded, on the grounds that the
// unfinished-transaction section and gc's own pruning each had them covered --
// but that section shows *pending* transactions only, so a completed, unpruned
// family appeared in neither, and it is far and away the most common thing
// accumulating under recovery/. One `fu new` leaves nine entries there and
// `fu status` printed "nothing to report" over all nine, immediately before
// `fu gc` removed them. A pending family's files stay out, because
// report.Store.Pending already names that transaction by op and skill and
// reporting it here too would state one fact twice in incompatible terms --
// the same rule the removed- and rollback- arms below follow.
//
// That state is reachable by following fu's own advice:
// addRecoveryConflictRemedy (txn.go) tells a user to move a damaged
// transaction's whole journal family out of recovery/, which strands exactly
// these names. The same reasoning already put an unclaimed rollback- into
// Uncollectable; this carries it to the two prefixes it was not applied to.
//
// A family this cannot read is deliberately absent from the result. Being
// unable to prove a collector exists is the Uncollectable answer, and erring
// that way costs a user one honest "nothing collects this yet" line, where
// erring the other way costs them a remedy that does nothing.
//
// Only the newest revision of each family is read, never the whole chain.
// PendingTxns's own doc comment rejects the alternative in as many words: a
// completed family's revisions accumulate until someone runs `fu gc`, and an
// adopt records about six of them each carrying a full OwnedTree, so
// re-hashing every one of them on every `fu status` would make a read grow
// with the store's entire lifetime history. The newest revision is all this
// derivation needs -- rmPayloadName and the payload manifest both come from it
// -- and decodeTxnFile still checks the digest its own filename commits to.
//
// This does mean the Collectable count is a superset of gc's in two degraded
// states, both of which surface an error rather than a silent miscount: a
// completion marker whose bytes are damaged (gc decodes it and compares
// sequence and revision digest; this does neither), and a payload whose
// content no longer matches its manifest (gc's RemoveOwnedTreeAt refuses).
// "Collectable" therefore means "gc is entitled to collect this", not "gc will
// certainly succeed".
// It takes the journal already scanned rather than scanning one itself. Its
// only caller, Status, also needs PendingTxns over the same directory, and both
// used to read recovery/ independently -- so a damaged store paid for the walk
// twice and, when the walk failed, was told about it twice under two different
// wrappers naming the same directory (review round 27 finding 2). One scan
// feeds both; an unreadable directory is now one read and one error line.
//
// Nothing here can fail as a result, which is why there is no error return: the
// scan was the only thing that could, and the caller has already faced it.
func collectableRecoveryNamesFromJournal(st *store.Store, journal txnJournal) recoveryCollection {
	collection := recoveryCollection{
		payloads:   map[string]bool{},
		journal:    map[string]bool{},
		attributed: map[string]bool{},
		damaged:    map[string]bool{},
		scanned:    true,
	}
	keys := make(map[txnKey]bool, len(journal.revisions)+len(journal.completed)+len(journal.pruned))
	for key := range journal.revisions {
		keys[key] = true
	}
	for key := range journal.completed {
		keys[key] = true
	}
	for key := range journal.pruned {
		keys[key] = true
	}
	for key := range keys {
		files := txnFamilyFiles(journal, key)
		for _, name := range files {
			collection.attributed[name] = true
		}
		// A damaged family entitles gc to nothing: the prune loop skips it
		// with a repair remedy rather than deleting anything. Its files are
		// recorded as damaged on the way past, which is what keeps them in the
		// inventory: the paragraph above about being unable to prove a
		// collector exists names Uncollectable as the answer, and without this
		// the attributed set answered for them first with no bucket at all.
		if len(journal.invalid[key]) != 0 {
			for _, name := range files {
				collection.damaged[name] = true
			}
			continue
		}
		// Settled means gc's prune loop visits it at all: it iterates the
		// union of completed and pruned keys (txn_prune.go). A family with
		// neither is pending, waits on recovery rather than on gc, and is
		// already named by report.Store.Pending.
		if journal.completed[key] == "" && journal.pruned[key] == "" {
			continue
		}
		for _, name := range files {
			collection.journal[name] = true
		}
		// The payload half is narrower than the journal half by exactly one
		// state: an already-pruned family reclaims nothing, because the prune
		// record is written strictly after its payload was settled, so a
		// resumed prune finishes the deletion and has no manifest work left.
		if journal.pruned[key] != "" {
			continue
		}
		latest, err := newestTxnRevision(st, journal.revisions[key])
		if err != nil || latest.Op != "rm" || latest.Payload == nil {
			continue
		}
		payload := rmPayloadName(latest)
		collection.payloads[payload] = true
		// The retired root is the intermediate disposal leaves between
		// emptying the tree and unlinking it. Its name is derived from this
		// same manifest, which is exactly why RemoveOwnedTreeAt can resume
		// from it -- and exactly why it stops being collectable when the
		// manifest is gone.
		collection.payloads[store.RetiredRecoveryRootName(payload, *latest.Payload)] = true
	}
	return collection
}

// txnFamilyFiles names every journal file the scan attributed to one family.
// The three kinds are exactly what pruneCompletedTransactionsLocked deletes
// for a family it takes on: the revisions and the completion marker it lists in
// the prune record, and finally the prune record itself.
func txnFamilyFiles(journal txnJournal, key txnKey) []string {
	files := make([]string, 0, len(journal.revisions[key])+2)
	for _, revision := range journal.revisions[key] {
		files = append(files, revision.name)
	}
	if name := journal.completed[key]; name != "" {
		files = append(files, name)
	}
	if name := journal.pruned[key]; name != "" {
		files = append(files, name)
	}
	return files
}

// newestTxnRevision decodes the highest-sequence revision of one family and
// nothing else. It is collectableRecoveryNames's cheap counterpart to
// validateTxnChain, which re-reads and re-hashes every revision in the chain
// -- the cost model PendingTxns documents as unaffordable on a read path.
func newestTxnRevision(st *store.Store, revisions []txnRevision) (TxnRecord, error) {
	if len(revisions) == 0 {
		return TxnRecord{}, fmt.Errorf("transaction family has no immutable revisions")
	}
	newest := revisions[0]
	for _, revision := range revisions[1:] {
		if revision.sequence > newest.sequence {
			newest = revision
		}
	}
	return decodeTxnFile(st, newest)
}

// pendingPayloadClaims collects every recovery payload name a pending
// transaction still derives: an rm's quarantined content (rmPayloadName,
// rm.go), and an install's compensation payload in both the committed and
// not-yet-committed forms (installCompensationPayloadName,
// installUncommittedPayloadName, new_txn.go). All three are derived from every
// record rather than from the records whose op produces each form, for the
// reason pendingRecoveryPayloadClaims (txn_prune.go) states: a payload name is
// a pure function of the skill name and the start HEAD, so any pending record
// whose derivation lands on a name is one nothing can prove does not own the
// object sitting there.
//
// One derivation serves both callers, which is the point. `fu gc` asks it what
// it must not collect; status asks it what it must not tell the user to
// collect. Two derivations, each covering the half the other missed, is what
// let the inventory count a removed- payload as collectable while gc was
// leaving it alone. This form takes the pending slice its caller already read
// rather than reading the journal a second time.
// recoveryCollection is everything one journal scan tells the inventory, kept
// in three sets because the switch below has three different questions to ask
// of a recovery-directory name.
func pendingPayloadClaims(pending []TxnRecord) map[string]bool {
	claims := make(map[string]bool, len(pending)*3)
	for _, record := range pending {
		claims[rmPayloadName(record)] = true
		claims[installCompensationPayloadName(record)] = true
		claims[installUncommittedPayloadName(record)] = true
	}
	return claims
}

// Status assembles the read-only report. It never writes: agents are inspected
// through ScanAgent rather than the write path's createAndScanAgentDir
// (reconcile.go), which creates a missing skills directory before scanning it.
// It also takes no lock -- a read-only command should not be blocked behind a
// long write, and the cost is that the report is a snapshot that a concurrent
// write may already have moved past.
func Status(st *store.Store, cfg *store.Config, agents []agent.Agent) (StatusReport, error) {
	report := StatusReport{}
	for _, detected := range agents {
		status := AgentStatus{Name: detected.Name()}
		state, err := ScanAgent(detected, st.SkillsDir())
		if err != nil {
			status.ScanErr = err.Error()
			report.Agents = append(report.Agents, status)
			continue
		}
		status.DirMissing = state.ParentMissing
		status.DirIsSymlink = state.ParentIsSymlink
		// Desired's own findings come first: a reserved or invalid name never
		// becomes a path component, so Diff never sees it and cannot report it.
		desired, reserved, invalid := Desired(cfg, detected)
		status.Drift = append(status.Drift, reserved...)
		status.Drift = append(status.Drift, invalid...)
		// A symlinked skills directory is refused wholesale by reconcile, so
		// there is no per-entry drift to describe -- describing it anyway would
		// list work fu will never do.
		if !state.ParentIsSymlink {
			status.Drift = append(status.Drift, statusDrift(Diff(desired, state, st.SkillsDir()), state)...)
		}
		report.Agents = append(report.Agents, status)
	}

	// Store-side facts are read after the agents so a scan failure on one agent
	// does not cost the user the rest of the report.
	//
	// Each of the four sections below is assembled independently and its
	// failure accumulated rather than returned, which is commit 868e83d's
	// standard ("let a damaged store cost `fu status` one section, not the
	// report") applied where it had not been: these sections used to return on
	// the first error, so a damaged journal family took the store, recovery and
	// staging sections down together even though the two local inventories have
	// nothing to do with git, and a git-side failure took out the two
	// inventories even though they have nothing to do with git either.
	var problems []error
	dirty, err := st.ChangedPathsIncludingIgnored()
	if err != nil {
		problems = append(problems, fmt.Errorf("read the store worktree: %w", err))
	}
	report.Store.DirtyPaths = dirty

	// One scan of recovery/ for both derivations below. They used to take one
	// each -- PendingTxns through scanTxnJournal, the collectable derivation
	// through scanTxnJournalReport -- which read the directory twice and, when
	// that read failed, put the same root cause into problems twice under two
	// wrappers that both named the directory (review round 27 finding 2). A
	// duplicate is worst in exactly the case a user runs `fu status` to
	// diagnose, and errors.Is could not have removed it: the two failures are
	// distinct *os.PathError values, so it compares unequal pointers and says
	// they are unrelated.
	journal, scanErr := scanTxnJournalReport(st)

	var pending []TxnRecord
	if scanErr != nil {
		problems = append(problems, fmt.Errorf("read unfinished transactions: %w", scanErr))
	} else {
		var pendingErr error
		pending, pendingErr = pendingTxnsFromJournal(st, journal)
		if pendingErr != nil {
			problems = append(problems, fmt.Errorf("read unfinished transactions: %w", pendingErr))
		}
	}
	for _, record := range pending {
		report.Store.Pending = append(report.Store.Pending, PendingOperation{Op: record.Op, Name: record.Name})
	}

	// claims is computed from the pending slice just read above, not from the
	// recovery-dir listing below: it never opens or parses a payload, only
	// compares its name against a set derived from already-parsed journal
	// records.
	claims := pendingPayloadClaims(pending)
	// The mirror of claims: what a settled family entitles gc to collect. Both
	// are needed because the two buckets answer different questions -- claims
	// keeps a pending transaction's own work out of the inventory, collectable
	// keeps the Collectable count to names gc will really act on.
	//
	// Left at its zero value when the scan failed, which is what scanned is for:
	// no family state is known, so no bucket can be justified, and the empty
	// sets must not be read as "nothing here is collectable".
	var collectable recoveryCollection
	if scanErr == nil {
		collectable = collectableRecoveryNamesFromJournal(st, journal)
	}

	// Through the store's own listing rather than os.ReadDir on the pathname:
	// it reads the pinned descriptor when a session holds one and falls back
	// to the pathname otherwise, which is the TOCTOU every other reader of
	// these directories already closed. status usually lands on the fallback,
	// since it opens no write session -- but that decision belongs in one
	// place, not restated at each call site.
	names, err := st.RecoveryNames()
	if err != nil && scanErr != nil {
		// The third read of this same directory, and the third report of one
		// failure, which is what the deduplication above was actually about.
		// This listing and the journal scan differ in purpose but not in what
		// they ask the filesystem, so a directory the scan could not list is one
		// this cannot either -- and the line it would add says nothing the scan's
		// line has not already said, down to the path.
		//
		// Suppressed only when the scan has already reported it. The two do come
		// apart in one direction: a recovery directory that does not exist at all
		// is an error to the scan and an empty listing to RecoveryNames
		// (logicalRootNames absorbs IsNotExist), so that case reports once and
		// through the scan, with this branch never reached.
	} else if err != nil {
		// The inventory below cannot be taken without a listing, but the
		// staging section after it can, and so can everything already
		// assembled above.
		problems = append(problems, fmt.Errorf("read the recovery directory %s: %w", st.RecoveryDir(), err))
	}
	// A config exchange record with no completion marker beside it is finished
	// or withdrawn by RecoverConfigExchanges, which runs from RecoverPending --
	// the mandatory first step of every write command, and of `fu restore` --
	// and never from `fu gc`. Until then ReclaimCompletedConfigExchanges
	// (store/config_exchange.go) collects neither that record nor any
	// .fu-config-archive- name at all: its archive sweep is frozen while one
	// exchange is still pending. Both facts are read here through the predicate
	// gc and recovery already share, so this report cannot come to disagree
	// with gc about what gc would collect.
	pendingExchanges := map[string]bool{}
	for _, name := range store.PendingConfigExchangeRecords(names) {
		pendingExchanges[name] = true
	}
	archivesFrozen := len(pendingExchanges) != 0
	// gc's own two conditions for unlinking an archive, asked through gc's own
	// predicate so the two cannot come to disagree: the name must round-trip
	// through the archive grammar, and it must still hold the device/inode it
	// states. An archive failing either is preserved on every run forever.
	describedArchives := st.CollectableConfigArchiveNames(names)
	// The same question for the record/marker half of that sweep, asked
	// through the strict grammar gc itself applies rather than the loose one
	// the pending scan uses.
	collectableExchanges := store.CollectableConfigExchangeNames(names)
	for _, name := range names {
		switch {
		// A journal file belongs to whichever family the scan attributed it to,
		// and that family's state decides the bucket. A settled family --
		// completed, or already carrying a prune record from an interrupted
		// run -- is exactly what `fu gc` deletes next, and by far the most
		// common thing accumulating here: nine entries after a single `fu new`.
		// A pending family's files stay out, because report.Store.Pending above
		// already names that transaction by op and skill. A name carrying the
		// prefix that resolves to no family at all is fu's to explain rather
		// than to ignore, and the honest bucket for it is the last one.
		case strings.HasPrefix(name, "txn-"):
			switch {
			case !collectable.scanned:
				// The journal could not be read, so no family state is known
				// and no bucket can be justified. The failure is already among
				// the problems above.
			case collectable.journal[name]:
				report.Recovery.Collectable++
			case collectable.damaged[name]:
				// Attributed, but to a family the scan found invalid. Nothing
				// collects it -- gc's prune loop skips a damaged family with a
				// repair remedy -- and no other section names it either, since
				// PendingTxns fails on exactly this and so reports no
				// transaction at all. Tested before the arm below it, which
				// would otherwise answer for the same name with no bucket.
				report.Recovery.Uncollectable++
			case collectable.attributed[name]:
				// A pending family's own file, reported as an unfinished
				// transaction instead.
			default:
				report.Recovery.Uncollectable++
			}
		// A removed- name is an rm transaction's quarantined content
		// (rmPayloadName, rm.go). `fu gc` reclaims it while pruning the
		// completed family whose manifest describes it, but leaves it exactly
		// where it is while a pending transaction still derives the name
		// (txn_prune.go) -- ownership gc cannot disprove. That pending
		// transaction is the one report.Store.Pending above already names by op
		// and skill, so counting the payload here too would report one fact
		// twice in incompatible terms: "unfinished, the next write command
		// settles it" beside "collectable, run `fu gc`", where `fu gc` collects
		// nothing and the count never moves. Excluded on the same grounds as
		// txn- above, and as a claimed rollback- below.
		case strings.HasPrefix(name, "removed-"):
			switch {
			case claims[name]:
				// Reported already as the pending transaction it belongs to.
			case collectable.payloads[name]:
				report.Recovery.Collectable++
			default:
				// No pending transaction claims it and no completed family
				// describes it, so nothing in gc will ever name it. README's
				// own account is the accurate one here -- these are the
				// entries a reader may remove by hand -- and offering `fu gc`
				// instead contradicted it.
				report.Recovery.Uncollectable++
			}
		// A rollback- name is a new, add, or adopt transaction's own
		// compensation payload (installCompensationPayloadName /
		// installUncommittedPayloadName, new_txn.go; adopt reaches the
		// uncommitted form through rollBackUncommittedInstall,
		// adopt_txn.go): quarantined under this name while its
		// transaction is still open, and normally archived under
		// .fu-archive- before that transaction's WAL entry clears --
		// rollBackUncommittedInstall archives it inline, and the committed
		// path archives it in finish() before calling ClearTxn. So a
		// rollback- name usually means its owning transaction is still
		// pending, which report.Store.Pending above already names by op and
		// skill -- but not always: addRecoveryConflictRemedy's own
		// abandon-recovery advice (txn.go) tells a user to move every
		// journal file (revisions, completion marker, prune records) out of
		// recovery/ without mentioning this payload, so following it can
		// strand a rollback- name with no journal behind it at all.
		// claims names exactly the payloads some pending record still
		// derives; a claimed name is excluded here, the same as txn- above,
		// so the one fact is not reported twice in incompatible terms, but
		// an unclaimed name has no journal left to resolve it and nothing
		// else that will ever collect it, so it is counted Uncollectable
		// instead of silently dropped.
		case strings.HasPrefix(name, "rollback-"):
			if !claims[name] {
				report.Recovery.Uncollectable++
			}
		// A record and its marker are one family, and gc collects both in a
		// single run: the record once its marker exists, the marker once the
		// record is gone. Only a record still missing its marker waits, and it
		// waits on recovery, not on gc. A marker whose record is already gone
		// is finished by construction, so it is never pending and never
		// blocked.
		//
		// Pending and collectable are asked separately, because they are not
		// complements. The pending scan reads the loose grammar (prefix plus
		// ".json", hex unchecked); gc's collector reads the strict one
		// (exactly 16 hex digits, round-tripped). A name carrying the prefix
		// but failing the strict form -- an editor's ".json.bak", a non-hex
		// suffix -- is neither pending nor collectable, and treating "not
		// pending" as "collectable" offered every one of them to a `fu gc`
		// that walks straight past. fu does not write such names, but a user
		// rearranging this directory by hand can produce them, which is the
		// same reachability the neighbouring archive case was hardened for.
		case strings.HasPrefix(name, ".fu-config-exchange-"):
			switch {
			case pendingExchanges[name]:
				report.Recovery.Blocked++
			case collectableExchanges[name]:
				report.Recovery.Collectable++
			default:
				report.Recovery.Uncollectable++
			}
		// An archive is gc's to collect, but the sweep of them is frozen
		// wholesale while any exchange is pending -- one unfinished exchange
		// blocks every archive in the directory, not only the two its own
		// record spans.
		//
		// Unfrozen is necessary and not sufficient.
		// reclaimConfigExchangeStatedArchive (store/config_exchange.go)
		// unlinks an archive only while the name still resolves to the
		// device/inode its record states, so a name whose inode has since
		// drifted -- the case
		// TestReclaimCompletedConfigExchangesPreservesAnArchiveNameItDoesNot
		// Describe pins -- is preserved by gc every run, forever. Counting it
		// Collectable is the same false promise an unclaimed removed- payload
		// was. status cannot re-derive that identity without reading the
		// record, which this report does not do, so it reports what it can
		// prove: an archive gc will collect is one some completed exchange
		// describes.
		case strings.HasPrefix(name, ".fu-config-archive-"):
			switch {
			case archivesFrozen:
				report.Recovery.Blocked++
			case describedArchives[name]:
				report.Recovery.Collectable++
			default:
				report.Recovery.Uncollectable++
			}
		// .fu-retired-dir- is collectable rather than residue when its name is
		// derived from a manifest that is still on disk, because that is what
		// lets RemoveOwnedTreeAt resume from it on replay; reporting such a
		// name as uncollectable would push a user toward deleting a resumable
		// intermediate. Once the family is gone the derivation has nothing
		// behind it, no gc run will ever produce the name again, and the
		// honest bucket is the other one.
		case strings.HasPrefix(name, ".fu-retired-dir-"):
			if collectable.payloads[name] {
				report.Recovery.Collectable++
			} else {
				report.Recovery.Uncollectable++
			}
		case hasAnyPrefix(name, retainedPrefixes):
			report.Recovery.Retained++
		case hasAnyPrefix(name, uncollectablePrefixes):
			report.Recovery.Uncollectable++
		default:
			// Everything else, rather than nothing. Without this arm a name
			// matching no known prefix left no trace in the report at all, so
			// a recovery directory holding only such names produced an empty
			// inventory, the section was omitted, and `fu status` printed
			// "nothing to report" over it. fu's own atomic-write residue --
			// .tmp- and .tmp-retired- (fsutil.go), which DESIGN's known-gap
			// list already records as having no collector -- lands here, and
			// so does anything a future writer adds before this switch learns
			// about it. Uncollectable is the honest bucket for both: it says
			// something is accumulating and offers no remedy, which is exactly
			// the state of affairs.
			report.Recovery.Uncollectable++
		}
	}

	// The staging inventory is assembled from what has already been read: the
	// pending records above name their own staged content, and the pending
	// exchange records name theirs through the store's own derivation. Like the
	// recovery pass, this never opens or parses a staging entry -- every rule
	// is a name comparison.
	stagingClaims := make(map[string]bool, len(pending)*2+len(pendingExchanges)+1)
	for _, record := range pending {
		// A published staged root carries the skill's own name, and the private
		// reservation the name recovery reads back off the record rather than
		// deriving, since only the record knows which random name was taken.
		if record.Name != "" {
			stagingClaims[record.Name] = true
		}
		if record.StagingReservation != nil {
			stagingClaims[record.StagingReservation.Name] = true
		}
	}
	pendingExchangeNames := make([]string, 0, len(pendingExchanges))
	for name := range pendingExchanges {
		pendingExchangeNames = append(pendingExchangeNames, name)
	}
	for _, name := range store.PendingConfigExchangeStagingNames(pendingExchangeNames) {
		stagingClaims[name] = true
	}
	stagingNames, err := st.StagingNames()
	if err != nil {
		problems = append(problems, fmt.Errorf("read the staging directory %s: %w", st.StagingDir(), err))
	}
	for _, name := range stagingNames {
		switch {
		// Claimed first: a name a pending record governs is settled by the next
		// recovery pass, whatever shape it has. Testing residue first would
		// report a transaction's own work in progress as permanent litter.
		case stagingClaims[name]:
			report.Staging.Blocked++
		case hasAnyPrefix(name, stagingResiduePrefixes):
			report.Staging.Uncollectable++
		default:
			// A public name no pending transaction claims. It is not fu's
			// residue, and `fu new` and `fu add` already refuse to run against
			// it and name the path when they do (ops.go, add.go) -- a better
			// remedy than a count, which is why it gets its own bucket rather
			// than joining the residue above. Counting it at all is what
			// answers the user who has just been refused, opened `fu status`
			// to see what is in the way, and previously found nothing.
			report.Staging.Unmatched++
		}
	}
	return report, errors.Join(problems...)
}
