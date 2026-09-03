package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

// ExternalCommitMessage re-exports store.ExternalCommitMessage for the layers
// above engine. internal/cli must not import internal/store (the dependency
// rule its architecture_test.go enforces), and it needs this exact string to
// tell the user which commit it just recorded on their behalf -- so without a
// re-export the CLI repeats the literal, and the day the store's constant
// changes nothing catches the drift. Re-exported rather than duplicated so
// the compiler does.
const ExternalCommitMessage = store.ExternalCommitMessage

// commitSubjectMaxSkills is how many skill names a derived subject spells
// out before collapsing them to a count.
const commitSubjectMaxSkills = 5

// commitSubject builds the subject line of a `fu commit`. The verb before the
// colon is what operationVerbs (store/git.go) counts, so the subject is
// always generated and the user's -m text goes to the body. A named commit
// is "commit: <name>"; a store-wide one names what the candidate touched:
// the skills in sorted order (collapsed to "N skills" past
// commitSubjectMaxSkills), then "fu.yaml", then "store" for any path that
// is neither -- a file directly under skills/, say, or a README at the root.
func commitSubject(name string, changed []string) string {
	if name != "" {
		return "commit: " + name
	}
	skills := map[string]bool{}
	var config, other bool
	for _, p := range changed {
		switch {
		case p == "fu.yaml":
			config = true
		case strings.HasPrefix(p, store.SkillsPrefix):
			skill, _, found := strings.Cut(strings.TrimPrefix(p, store.SkillsPrefix), "/")
			if found && skill != "" {
				skills[skill] = true
			} else {
				other = true
			}
		default:
			other = true
		}
	}
	var parts []string
	if len(skills) > commitSubjectMaxSkills {
		parts = append(parts, fmt.Sprintf("%d skills", len(skills)))
	} else if len(skills) > 0 {
		names := make([]string, 0, len(skills))
		for s := range skills {
			names = append(names, s)
		}
		sort.Strings(names)
		parts = append(parts, names...)
	}
	if config {
		parts = append(parts, "fu.yaml")
	}
	if other || len(parts) == 0 {
		parts = append(parts, "store")
	}
	return "commit: " + strings.Join(parts, ", ")
}

// CommitScope says what one `fu commit` records: Name limits it to that
// skill's own directory, empty means the whole store; Message is the -m text
// and becomes the commit body.
type CommitScope struct {
	Name    string
	Message string
}

// CommitOutcome reports one `fu commit`. Written is false when the candidate
// held no change, in which case Subject is empty and no commit exists.
// Changed is the candidate's HEAD-to-index path set, empty or not.
//
// ExternalWritten is set only on the store-wide shape (scope.Name == ""),
// and reports whether a snapshot the user had staged directly with git was
// recorded under store.ExternalCommitMessage before the store-wide candidate
// was even prepared. It is not implied by Written and must be read alongside
// it: once that external commit lands on HEAD, the store-wide candidate
// PrepareCommit takes right afterward can turn out to equal that same HEAD
// tree byte for byte -- there is nothing left on top of it to record -- so
// Written stays false and Subject stays empty even though a durable commit
// was in fact written (review round 1, finding 2: `git add -A` followed by
// `fu commit -m reason` is exactly this case). "nothing to commit" is the
// wrong thing to tell a user in that state, so the CLI must check
// ExternalWritten before saying it.
type CommitOutcome struct {
	Result          Result
	Written         bool
	ExternalWritten bool
	Subject         string
	Changed         []string
}

// CommitOperations records pending hand edits as one operation (SPEC §5.1
// `fu commit`, §5.3). It is a write command that deliberately does not take
// run's route (pipeline.go): run sweeps the whole worktree before anything
// else, which is incompatible with recording one skill and leaving the rest
// pending. What it keeps from that route is everything else a write command
// owes -- the lock, pending-transaction recovery before any of its own work,
// the writability and canonical-path checks, and a closing reconcile -- in
// the same shape RevertOperations (restore.go) uses.
//
// Named, the candidate is PrepareCommitUnder(skills/<name>) and the public
// index is never consulted for that subtree. Store-wide, it is Sweep's own
// two layers: a snapshot the user staged with direct git is committed first
// under ExternalCommitMessage, then the worktree under the derived subject.
func CommitOperations(st *store.Store, agents []agent.Agent, scope CommitScope) (outcome CommitOutcome, retErr error) {
	var res Result
	defer func() { outcome.Result = res }()
	session, err := st.BeginWrite()
	if err != nil {
		return outcome, err
	}
	defer func() { retErr = errors.Join(retErr, session.Close()) }()
	checked := session.Store
	homeRoot, err := checked.Root()
	if err != nil {
		return outcome, fmt.Errorf("use checked commit root: %w", err)
	}
	storeRoot, err := checked.StoreRoot()
	if err != nil {
		return outcome, fmt.Errorf("use checked store root for commit: %w", err)
	}
	retErr = withLock(homeRoot, "fu.lock", st.LockPath(), func() error {
		recoveryResult, err := RecoverPendingReporting(checked)
		mergeResult(&res, recoveryResult)
		if err != nil {
			return fmt.Errorf("recover pending transactions before commit: %w", err)
		}
		cfg, err := store.LoadConfigRoot(storeRoot, "fu.yaml", st.ConfigPath())
		if err != nil {
			return fmt.Errorf("load config %s for commit: %w", st.ConfigPath(), err)
		}
		if err := cfg.CheckWritable(); err != nil {
			return fmt.Errorf("check config writable before commit: %w", err)
		}
		if err := session.CheckCanonicalPath(); err != nil {
			return err
		}

		var prepared store.PreparedCommit
		if scope.Name != "" {
			if !cfg.HasSkill(scope.Name) {
				// The shared answer `fu show`, `fu rm` and `fu update` give,
				// rather than a bare "unknown skill": a name fu.yaml holds but
				// LoadConfig excluded for failing validation is visible in
				// `fu list` and is not unknown, so the reply has to name the
				// file and say what is wrong with it (review 2026-09-02,
				// Minor).
				return unknownSkillError(checked, cfg, scope.Name)
			}
			prepared, err = checked.PrepareCommitUnder([]string{store.SkillsPrefix + scope.Name})
			if err != nil {
				return explainCommitScopeViolation(scope.Name, checked.Dir(), err)
			}
		} else {
			// The first of the two layers store.Sweep records, through the
			// same primitive it uses: whatever the user staged with direct
			// git is committed under its own message before the worktree
			// state that came after it, so a staged-only version stays
			// recoverable in history. Written is read out whether or not the
			// error is nil, for the reason CommitStagedSnapshot's own doc
			// gives (review round 1, finding 3).
			externalCommitted, err := checked.CommitStagedSnapshot()
			outcome.ExternalWritten = externalCommitted.Written
			if err != nil {
				return fmt.Errorf("record the staged snapshot: %w", err)
			}
			if prepared, err = checked.PrepareCommit(); err != nil {
				return fmt.Errorf("prepare commit: %w", err)
			}
		}
		outcome.Changed = prepared.ChangedPaths()
		// An empty candidate still falls through to the reconcile below
		// (review round 1, finding 1): a hand-deleted agent symlink with
		// nothing pending to commit must be repaired exactly like every other
		// write command repairs it, not left for the next command that
		// happens to have something to record. Written and Subject stay at
		// their zero values, since there is nothing here to commit.
		if len(outcome.Changed) == 0 {
			// Published anyway, though there is nothing to record. A
			// candidate with no HEAD-to-index change can still supersede
			// public index entries -- stage a version, then edit the worktree
			// back to the committed bytes -- and CommitPrepared's own
			// no-change return is where that index sync happens. Skipping the
			// call left those entries stale for good: `fu status` reported the
			// skill pending while this command answered "nothing to commit"
			// forever (review 2026-09-02 round 2, Important; round 1's fix
			// reached the store layer but this caller short-circuited past
			// it).
			//
			// The subject is the one this command would have used, not "".
			// It is unused by construction -- an empty changed set means the
			// candidate tree equals HEAD's, which is exactly the branch that
			// writes no commit, and the assertion below holds that. But
			// "unused by construction" rests on a tree comparison made after
			// this call is prepared, and DESIGN books a prepare-to-publish
			// race; should one ever land, the difference is between a durable
			// commit that reads as an operation in `fu log` and one with no
			// message at all (review 2026-09-03, Minor). The honest subject
			// costs nothing.
			settled, err := checked.CommitPrepared(commitSubject(scope.Name, nil), prepared)
			if err != nil {
				return err
			}
			if settled.Written {
				return fmt.Errorf("internal: a candidate with no changes published commit %s", settled.Hash)
			}
		} else {
			outcome.Subject = commitSubject(scope.Name, outcome.Changed)
			message := outcome.Subject
			if scope.Message != "" {
				message += "\n\n" + scope.Message
			}
			// Written is captured before the error check, exactly as the
			// external layer above does it and for the reason
			// CommitStagedSnapshot's doc states once for both. The same rule
			// governs RevertOperations (restore.go), which sets
			// outcome.Changed before checking Revert's error.
			committed, err := checked.CommitPrepared(message, prepared)
			outcome.Written = committed.Written
			if err != nil {
				return err
			}
		}

		// Content changed or not, topology did not; still reconciled, so this
		// write command settles link drift like every other one does. This
		// runs even when nothing was committed above -- see the comment on
		// the empty-candidate branch.
		reconcileResult, err := reconcileChecked(checked, cfg, agents, nil)
		mergeResult(&res, reconcileResult)
		return err
	})
	return outcome, retErr
}

// explainCommitScopeViolation turns PrepareCommitUnder's containment refusal
// into something a user can act on.
//
// The refusal is genuinely reachable, not merely defensive: PrepareCommitUnder
// fills every path outside the requested prefix from the *public* index, and
// its changed-path set is a HEAD-to-index projection. So `git add fu.yaml`
// followed by `fu commit alpha` lands fu.yaml in the candidate's changed set
// on account of being staged, not on account of anything alpha did, and the
// containment check fires. Refusing is correct -- committing would silently
// fold the user's staged fu.yaml into a commit whose subject claims to be
// about alpha -- but PrepareCommitUnder's own message ("candidate
// unexpectedly changes fu.yaml") names the paths and stops there, which
// reads as an internal invariant tripping rather than something the user
// did. This wraps it with the paths, why they are blocking, and both ways
// forward, while keeping PrepareCommitUnder's own error in the chain through
// commitScopeRefusal.Unwrap, so errors.Is/errors.As still see it. Not %w:
// see the comment on the return below for why that verb could not be used
// here (review 2026-09-03 round 5, Minor).
//
// The offending paths come from store.CommitScopeViolationError via
// errors.As, not from scanning the rendered message for a marker substring
// as this once did (final review, finding 2): scraping failed *wrong* rather
// than safe if the message grew a suffix, mangled any path containing the
// marker text, and could only ever surface the first path -- so a user with
// three staged paths was refused three times, once per path. All of them are
// now named at once.
//
// storeDir is where the suggested command must be run: `git restore --staged`
// only works inside the store under $FU_HOME, which is rarely where the user
// was standing when they typed `fu commit <name>`.
//
// The claim that the paths are "staged in git's index" is exact, and depends
// on final review finding 1: before that fix an untracked path elsewhere in
// the store also reached this refusal, and calling it staged was false.
func explainCommitScopeViolation(name, storeDir string, err error) error {
	var violation *store.CommitScopeViolationError
	if !errors.As(err, &violation) || len(violation.Paths) == 0 {
		return fmt.Errorf("prepare commit for %s: %w", name, err)
	}
	// The paths are named once, in the command that acts on them, rather than
	// listed as a subject and then repeated as arguments: one of three staged
	// paths appearing twice in a single sentence reads as two findings
	// (review 2026-09-02 round 1, Minor).
	//
	// Getting to *once* takes a type rather than another fmt.Errorf. The %w
	// that keeps PrepareCommitUnder's error reachable also splices its text
	// -- which ends in the same path list -- onto the end of this message, so
	// an earlier attempt at this fix removed one of three renderings and left
	// two (review 2026-09-03 round 4, Minor). commitScopeRefusal separates
	// the two jobs: Error returns only the sentence, Unwrap keeps the chain.
	subject, pronoun := "a path is", "it"
	if len(violation.Paths) > 1 {
		subject, pronoun = fmt.Sprintf("%d paths are", len(violation.Paths)), "them"
	}
	return &commitScopeRefusal{
		message: fmt.Sprintf(
			"%s staged in git's index outside skill %q being recorded; unstage %s with "+
				"`git restore --staged %s` in the store at %s, or run `fu commit` with no name "+
				"to record everything",
			subject, name, pronoun, strings.Join(violation.Paths, " "), storeDir,
		),
		err: err,
	}
}

// commitScopeRefusal carries explainCommitScopeViolation's sentence without
// letting the wrapped error render itself into it.
//
// fmt.Errorf's %w has no way to keep an error reachable without also printing
// it, and the store error's text ends in the very path list this message
// already names. Error and Unwrap split what %w conflates, so errors.Is and
// errors.As behave exactly as they did before while each path is named once.
type commitScopeRefusal struct {
	message string
	err     error
}

func (e *commitScopeRefusal) Error() string { return e.message }

func (e *commitScopeRefusal) Unwrap() error { return e.err }
