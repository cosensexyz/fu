// internal/engine/update.go
package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/skill"
	"github.com/cosensexyz/fu/internal/source"
	"github.com/cosensexyz/fu/internal/store"
)

// ErrLocallyModified marks update's SPEC rule 3 refusal: the published copy no
// longer matches the baseline recorded when the skill was installed, so pulling
// upstream over it would silently discard whatever was edited in place.
//
// It is a sentinel so that the one refusal the user can act on directly --
// `--force` overwrites -- can be recognised as itself rather than matched by
// message. Today's CLI does not: the `--force` hint is written into both
// refusal strings and nothing in internal/cli inspects this value. The
// consumers are this package's tests and, per SPEC §5.2, a second front end
// that will want to offer the flag without parsing prose.
var ErrLocallyModified = errors.New("skill has local modifications")

// shortDigest shortens a "sha256:"+64-hex content digest (skill.DigestManifest)
// for display, keeping the algorithm prefix so a reader can still tell what
// they are looking at. It is the digest counterpart of the commit shortening
// `fu show` and `fu outdated` already apply (shortCommit, internal/cli).
//
// A digest is shown at all only because SPEC rule 3 requires the refusal to
// show the difference and a digest pair is all the recorded baseline can
// support -- so it is shown at the length that distinguishes two values, not
// at the length that lets one be retyped.
func shortDigest(digest string) string {
	const prefix = "sha256:"
	if hex, ok := strings.CutPrefix(digest, prefix); ok && len(hex) > 12 {
		return prefix + hex[:12]
	}
	return digest
}

// describeLocalModification is the shared body of update's two SPEC rule 3
// refusals: the one selectUpdateTargets raises for a named skill before any
// clone (application.go), and checkUpdateAvailable's own, raised from the
// content shape below. Both owe the user 提示差异 (SPEC §5.1, rule 3), and
// the two halves of that are here so they cannot drift apart.
//
// What it can show is the digest pair and where the content is. fu.yaml
// records the install baseline as a digest, not a manifest, so nothing at
// this layer can name which files changed; the store's git history can, and
// it always holds the edit -- either still uncommitted in the worktree, or
// swept into a commit of its own by whichever fu write command ran next
// (SPEC §5.3).
//
// `git log -p` leads, and that order is load-bearing rather than stylistic.
// Both of this refusal's call sites run *after* a sweep on every path a user
// can reach -- selectUpdateTargets after the prologue's sweep, and
// checkUpdateAvailable inside run, after run's own -- so by the time either
// message is printed the edit is a commit and `git diff` prints nothing at
// all. Offering it first made the first thing SPEC rule 3's 提示差异 obligation
// hands the user a guaranteed dead end. It is still named second, because the
// pre-sweep state is reachable in principle (the window between a sweep and
// the preflight that follows it, and a direct call from a test), and the
// clause that names it says which state it is for.
// An absent baseline is one of the two it can be handed. judgeLocalModification
// sets Baseline straight from cfg.Digest and calls anything but the empty string
// a modification (outdated.go), so a fu.yaml whose `digest:` key was removed by
// hand reaches this refusal with nothing on the recorded side. Rendered as the
// pair it is not, that reads "(recorded , now sha256:...)" -- a blank where the
// value belongs, which looks like a fault in the message rather than the missing
// record it reports.
func describeLocalModification(st *store.Store, name, baseline, current string) string {
	tracked := path.Join("skills", name)
	comparison := fmt.Sprintf("recorded %s, now %s", shortDigest(baseline), shortDigest(current))
	if baseline == "" {
		comparison = fmt.Sprintf("nothing recorded, now %s", shortDigest(current))
	}
	return fmt.Sprintf(
		"%s no longer matches the content recorded when %q was installed (%s); "+
			"inspect the difference with `git -C %s log -p -- %s`, "+
			"or `git -C %s diff -- %s` if fu has not recorded the edit yet",
		filepath.Join(st.SkillsDir(), name), name, comparison,
		st.Dir(), tracked, st.Dir(), tracked)
}

// UpdateOutcome reports how updateSkill resolved one skill.
type UpdateOutcome struct {
	Name string
	// LockOnly is true when the upstream content's digest matched the
	// store's recorded install baseline, so nothing needed to move on disk:
	// only the source lock in fu.yaml advanced. False means the full staged
	// exchange ran instead (the content shape).
	LockOnly  bool
	Operation OperationOutcome
}

// updateSkill pulls one skill's already-prepared source ahead and records
// the result. p must already be prepared (source.Source.Prepare /
// PrepareChecked) by the caller before the write lock is taken -- exactly as
// `fu add` does it (add_command.go's prepareAddSource) -- because network I/O
// must never happen while the write lock (taken inside run, pipeline.go) is
// held. The trailing hooks are the pipeline's test-only seam (see run), so a
// test can terminate the operation at each durable boundary and assert what
// recovery makes of the state left behind -- the same seam newSkill and
// removeSkill expose for their own crash tests.
//
// It is deliberately unexported, unlike NewSkill / AddSkill / RemoveSkill. It
// had an exported wrapper with no production caller: `fu update` reaches it
// through Application.UpdateSkills, which is the boundary a second front end
// would call too, and which ErrUpdateForceNeedsName's own doc already names as
// the place a refusal has to hold. An exported entry point one layer below
// that boundary would have to restate every guard the boundary applies, and
// the lock-only shape below applies fewer than the content shape does.
//
// It has two shapes, chosen by comparing the digest of the tree the prepared
// source actually holds against the skill's recorded baseline
// (store.Config.Digest). That digest is recomputed below from p.Root() and
// cand.Subdir rather than read from cand.Digest: cand.Digest is what a scan
// reported when the candidate was inspected, while the shape decision has to
// rest on what is there now. cand.Digest is still checked by the content shape,
// in its Mutate, but as the "did the source change since inspection" guard it
// already is in add -- not as the input to this choice. The lock-only shape
// never reads it: it has already established that the source tree hashes to the
// recorded baseline, which answers the same question more strongly than the
// scan's own record of it could.
//
//   - lock-only: the digest matches. Nothing on disk needs to change, so only
//     fu.yaml's source record advances.
//   - content: the digest differs. A full transaction stages the new tree and
//     exchanges it in atomically (store.ExchangeStagedWithSkillOwned).
//
// The lock-only shape exists because `fu outdated` is read-only (SPEC §9)
// and must never write anything, so it can never clone to compare content --
// it can only compare the recorded commit against whatever the remote's ref
// currently resolves to. When an upstream commit advances but the skill's
// own subdir is untouched, `outdated` therefore reports the skill as
// updatable even though the content turns out identical. If update recorded
// nothing in that case, that skill would be reported updatable forever:
// every run would prompt, and every update would do nothing. Advancing the
// lock is what closes that loop.
//
// Only the content shape runs checkUpdateAvailable, and only the content shape
// needs to. SPEC rule 3 protects the store copy a user has edited in place, and
// the lock-only shape writes nothing but fu.yaml's source record: it does not
// touch skills/<name>, and it leaves cfg.Digest alone, so the install baseline
// still records the pre-edit content and the skill still reads as locally
// modified afterwards. There is nothing here for rule 3 to protect. The
// refusal a locally modified skill meets on this path comes from
// selectUpdateTargets (application.go), which cannot yet know which shape will
// apply -- it has not cloned -- and is a product decision about not advancing
// a lock behind the user's back, not a safety guard. Its message is worded
// accordingly.
//
// The two preconditions that are not rule 3 do apply to both shapes, and the
// lock-only Mutate below states each where it runs it: the recorded source
// must still be the one the source was prepared from, and skills/<name> must
// still hold content for the lock to be about. Neither is a comparison against
// the published tree's contents, which is what rule 3 is and what only the
// content shape owes.
//
// --force is the one thing that reaches past that reasoning, and the shape
// decision has to account for it (round 3): the flag asks to overwrite the
// local modifications, which no amount of lock advancing does. When the store
// copy has drifted from the baseline, --force therefore takes the content
// shape even though upstream has not moved -- see the force arm below.
//
// recorded is the source record p was prepared from, and fields is the record
// this update will write back; both shapes re-check the first against the
// config they actually mutate (checkRecordedSourceUnchanged) before writing
// the second.
func updateSkill(st *store.Store, agents []agent.Agent, p *source.Prepared, name string, cand Candidate, recorded, fields map[string]string, force bool, h hooks) (UpdateOutcome, error) {
	if err := skill.ValidateName(name); err != nil {
		return UpdateOutcome{Name: name}, fmt.Errorf("refuse to update %q, which is not a public skill name: %w", name, err)
	}
	outcome := UpdateOutcome{Name: name}

	srcRoot, err := p.Root()
	if err != nil {
		return outcome, fmt.Errorf("use prepared source %s: %w", p.Dir(), err)
	}
	proj, err := skill.ProjectDir(srcRoot.FS(), cand.Subdir)
	if err != nil {
		return outcome, fmt.Errorf("project source %s: %w", cand.Subdir, err)
	}
	observedDigest, err := skill.DigestManifest(proj)
	if err != nil {
		return outcome, fmt.Errorf("digest source %s: %w", cand.Subdir, err)
	}

	// The shape decision has to be made before `run` is entered: `run` only
	// ever builds the one Op shape it is given, and cfg -- what the shape
	// decision must compare against -- is only available inside its Mutate.
	// Reading the config once more here, outside any lock, decides which Op
	// shape to build and nothing else.
	//
	// Exactly one of the two shapes then repeats this comparison. The lock-only
	// Mutate below re-tests observedDigest against the config it actually
	// received and refuses if the baseline moved in between -- the same
	// double-check checkAddAvailable performs once in Preflight and once in
	// Mutate (add.go). The content shape's Mutate does not: it runs
	// checkUpdateAvailable, whose digest comparison asks a different question
	// (the published tree against the baseline, SPEC rule 3).
	//
	// So what makes a stale read safe on that side is not a re-check but where
	// the content shape takes its inputs from: it re-snapshots the published
	// tree under the lock and derives PreviousPayload from that snapshot, never
	// from anything read here. A shape chosen against a baseline that has since
	// moved therefore either refuses under rule 3 or performs an exchange whose
	// result is byte-identical to the tree it replaced -- redundant, and
	// invisible in the store's git history.
	cfg, err := store.LoadConfig(st.ConfigPath())
	if err != nil {
		return outcome, fmt.Errorf("load config %s: %w", st.ConfigPath(), err)
	}

	if observedDigest != cfg.Digest(name) {
		return updateSkillContent(st, agents, p, name, cand, recorded, fields, force, h)
	}
	// Upstream matches the baseline, so there is nothing new to pull -- but
	// --force is not a request to pull, it is a request to overwrite the local
	// modifications, and only the content shape can do that. Round 3: without
	// this arm `fu update <name> --force` over a hand-edited skill whose
	// upstream had not moved took the lock-only branch, which never consults
	// force, never touches skills/<name> and never moves the baseline. The
	// command exited 0 saying the content was unchanged, the hand edit
	// survived, and `fu outdated` went on advertising a refusal forever --
	// while --force's own help promised exactly this overwrite. No other
	// command could resolve it.
	//
	// The exchange this routes into restores the upstream tree, which here is
	// byte-identical to the recorded baseline: that is what "overwrite the
	// local changes" means when upstream has not moved. The overwritten content
	// stays in git history, since the prologue sweep commits it first (SPEC
	// rule 3).

	if force {
		matches, err := storeCopyMatchesBaseline(st, name, cfg.Digest(name))
		if err != nil {
			return outcome, err
		}
		if !matches {
			return updateSkillContent(st, agents, p, name, cand, recorded, fields, force, h)
		}
	}
	outcome.LockOnly = true
	operation := OperationOutcome{Name: name}
	// No Txn: a lock-only update has no filesystem step to recover, so it
	// takes the same shape as setGlobalTracked (ops.go) -- an Op with just
	// Message, outcome, AllowedChanges and Mutate. Giving a config-only
	// change a transaction record would put an entry in the recovery
	// journal that recovery has nothing to do with.
	_, runErr := run(st, agents, Op{
		Message:        "update: " + name + " (lock only)",
		outcome:        &operation,
		AllowedChanges: []string{"fu.yaml"},
		Mutate: func(st *store.Store, cfg *store.Config) error {
			if !cfg.HasSkill(name) {
				return fmt.Errorf("unknown skill %q", name)
			}
			if err := checkRecordedSourceUnchanged(cfg, name, recorded); err != nil {
				return err
			}
			// The published tree is proof this skill still has content to
			// advance a lock over, and it is the one precondition this shape
			// does not otherwise reach: observedDigest is the digest of the
			// *upstream* tree, not of the store copy, so the comparison below
			// passes cleanly when skills/<name> is simply gone -- and
			// judgeLocalModification degrades a missing store copy to
			// LocallyModified=false (outdated.go), so nothing upstream of here
			// refuses it either. The content shape gets this for free, inside
			// checkUpdateAvailable; without it the two shapes disagree about
			// whether a skill whose content is absent may still record a lock.
			// The snapshot itself is discarded: existence is the whole question.
			//
			// So this hashes a whole tree to answer a stat question, and that
			// cost is deliberate. Lstat would answer "is something there"; only
			// the snapshot answers it the same way the content shape does,
			// including which entry types count as content at all
			// (snapshotOwnedTree refuses the ones neither shape supports). A
			// reader reaching for Lstat here would make the two shapes disagree
			// about a store copy that exists but is not a tree fu can own.
			if _, err := st.SnapshotSkillPayload(name); err != nil {
				return fmt.Errorf("snapshot published skill %s: %w", filepath.Join(st.SkillsDir(), name), err)
			}
			// Repeat the digest comparison against the config this Mutate
			// actually received: it can differ from the snapshot read above
			// to choose this shape, if a hand edit to fu.yaml or another fu
			// process moved the baseline in between. (Not Sweep: it commits
			// worktree content and never writes fu.yaml -- cfg.Digest only
			// reads, and the field it reads is written in exactly three
			// places: AddSkill at install, and cfg.SetDigest's two callers,
			// updateSkillContent's Mutate and expectedUpdatedConfig, which is
			// recovery reconstructing what that Mutate wrote. This shape
			// deliberately writes no digest at all -- so an external edit to
			// fu.yaml moves the baseline and Sweep merely commits it
			// afterwards.)
			// When it no longer matches, the shape choice made outside the
			// lock is stale, and this operation must refuse rather than
			// silently advance the lock over content it never actually
			// verified against the config it is about to save.
			if observedDigest != cfg.Digest(name) {
				return fmt.Errorf("update: %q changed since it was inspected; retry", name)
			}
			cfg.SetSourceFields(name, fields)
			return nil
		},
	}, h)
	outcome.Operation = operation
	return outcome, runErr
}

// storeCopyMatchesBaseline reports whether the store's own copy of a skill
// still hashes to the digest fu.yaml recorded at install time -- SPEC rule 9's
// "本地修改" question, asked here only to decide which shape --force needs.
//
// It opens its own write session for the pinned descriptors SnapshotSkillPayload
// requires, exactly as Outdated does and for the same reason: BeginWrite opens
// read-only descriptors and validates identity, and the write lock is a separate
// step this never takes.
//
// Like the config read above it, this decides a shape rather than authorising a
// write -- and unlike that read, nothing downstream repeats it: the only call
// site is inside `if force`, and checkUpdateAvailable short-circuits on force
// before it computes a current digest at all (below). What makes a stale answer
// harmless is that both shapes are safe under --force. Lock-only writes nothing
// but the source record, and the content shape re-snapshots the live published
// tree inside its own Mutate, so PreviousPayload is never taken from what is
// read here. The whole cost of a stale answer is a --force that finds nothing
// to overwrite, and only when the store copy is edited between this read and
// the write lock.
func storeCopyMatchesBaseline(st *store.Store, name, baseline string) (matches bool, err error) {
	session, beginErr := st.BeginWrite()
	if beginErr != nil {
		return false, fmt.Errorf("open store to compare %q against its install baseline: %w", name, beginErr)
	}
	defer func() {
		if closeErr := session.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	payload, err := session.Store.SnapshotSkillPayload(name)
	if err != nil {
		return false, fmt.Errorf("snapshot published skill %s: %w", filepath.Join(st.SkillsDir(), name), err)
	}
	digest, err := digestOwnedPayload(payload)
	if err != nil {
		return false, fmt.Errorf("digest published skill %q: %w", name, err)
	}
	return digest == baseline, nil
}

// checkRecordedSourceUnchanged refuses an update whose input record moved
// while the source was being prepared. Both shapes run it against the config
// their own Mutate received, which is the only config a write can be
// authorised against.
//
// The window it closes is the whole of update's network I/O: UpdateSkills
// reads fu.yaml, clones from what it found (up to two minutes for a large
// repository), and only then takes the write lock. A hand edit landing inside
// that window is swept into a commit by `run`'s own sweep -- not the
// prologue's, which ran and released the lock before the clone started -- and
// then overwritten by this transaction's own SetSourceFields -- so the record the user just wrote
// is silently reverted, and, for the content shape, the content published
// comes from the repository they just stopped tracking. This is the class of
// race the in-Mutate repeat exists to stop: the write lock excludes other fu
// processes, never an external writer.
//
// Only the fields that decided what was prepared are compared. type, url, ref
// and path select the source and ref_kind decides which form of it is cloned
// (updateSourceKey, application.go, whose own doc names refKind part of the
// identity for exactly that reason); subdir selects the skill inside it
// (candidateAt). commit alone is left out, and not for the reason this
// paragraph used to give: both sides of the comparison are read from the config
// -- recorded before the clone, current inside Mutate -- and nothing in a single
// update moves the config's commit between those two reads (the new value lives
// in a separate fields map this function never sees), so comparing it would
// refuse nothing. It is excluded because it is not part of the identity the
// source was prepared from: it is the resolved output the previous update
// recorded, and this guard asks only whether the record still names the same
// source. ref_kind reads like a second such output and is not one, because it
// selects the source rather than reporting on it: judgeGitUpdate refuses any
// row whose ref_kind is not "branch" (outdated.go) and cloneSource returns the
// kind it was given (git.go), so in equals out on every reachable path -- while
// leaving it out let a hand edit pinning a tag pass this guard and be written
// straight back to branch by this transaction's own SetSourceFields, which is a
// replacement and not a merge. A record with no fields at all is compared like
// any other: it too is a record this update was not prepared from.
//
// The wording is the lock-only shape's own, which had exactly this guard for
// its own digest input from the start; sharing the sentence is what keeps the
// two halves of "changed since it was inspected" telling one story.
func checkRecordedSourceUnchanged(cfg *store.Config, name string, recorded map[string]string) error {
	current := cfg.SourceFields(name)
	for _, field := range []string{"type", "url", "ref", "ref_kind", "path", "subdir"} {
		if current[field] != recorded[field] {
			return fmt.Errorf("update: %q changed since it was inspected; retry", name)
		}
	}
	return nil
}

// checkUpdateAvailable is update's precondition set for the content shape, and
// returns the manifest of the published tree it validated so the caller can
// record that tree as the one the exchange will replace. Every check here can
// reject an untouched starting state, which is why it runs in Preflight --
// before any transaction record exists -- as well as again in Mutate
// (Op.Preflight, pipeline.go).
func checkUpdateAvailable(st *store.Store, cfg *store.Config, name string, cand Candidate, recorded map[string]string, force bool) (store.OwnedTree, error) {
	if !cfg.HasSkill(name) {
		return store.OwnedTree{}, fmt.Errorf("unknown skill %q", name)
	}
	// Before anything else this function checks: every remaining check is
	// about the source that was prepared, and none of them mean anything if
	// the record no longer names it.
	if err := checkRecordedSourceUnchanged(cfg, name, recorded); err != nil {
		return store.OwnedTree{}, err
	}
	// SPEC rule 1: a name is a skill's identity, in fu.yaml and in every
	// agent's links alike, so a renamed upstream skill is a different skill
	// rather than a new version of this one. Re-registering it under the new
	// name would leave the old name linked everywhere and owned by nothing.
	if cand.Name != name {
		return store.OwnedTree{}, fmt.Errorf(
			"upstream renamed %q to %q, and fu cannot rename an installed skill; run `fu rm %s` and add the new name instead",
			name, cand.Name, name)
	}
	// The same staging guard checkAddAvailable applies (add.go), and for a
	// sharper reason here: a crash inside this operation's own reclaim leaves
	// the tree it replaced orphaned at staging/<name>, and nothing clears it --
	// reclamation runs strictly after the terminal marker, so no recovery pass
	// treats it as work. Without this check the next update of that skill would
	// only meet the orphan inside Mutate, at createTxnStagedRoot, with the WAL
	// already open: a condition knowable while the store was still untouched
	// would instead manufacture a pending transaction for recovery to resolve.
	// Refusing here, before any record exists, is what Preflight is for.
	//
	// Checked before the --force shortcut below, because --force waives the
	// local-modification refusal and nothing else; an occupied staging name
	// blocks the exchange either way.
	//
	// The way out named first is `fu gc`, not the "move it aside or remove it"
	// that add advises: this orphan is a tree fu itself left behind, possibly
	// the only remaining copy of content the user wants, and gc verifies the
	// recorded manifest before reclaiming anything where a hand `rm -rf` does
	// not.
	//
	// The fallback is named too (fix round 2, Important #2), because gc is a
	// remedy for exactly one of the shapes that reach here. `fu gc` touches
	// staging/<name> only when a completed, unpruned `update` family names it;
	// a directory the user or an editor created there, and the orphan left by
	// the double fault txn_prune.go documents (an interrupted inline reclaim
	// plus an interrupted same-name `fu rm`), both leave gc silent and exiting
	// 0. This check cannot cheaply tell those apart -- the manifest that would
	// prove which is which lives in a journal family this code does not read --
	// so the message states the order to try them in rather than guessing.
	stagingRoot, err := st.StagingRoot()
	if err != nil {
		return store.OwnedTree{}, fmt.Errorf("use checked staging root: %w", err)
	}
	if _, err := stagingRoot.Lstat(name); err == nil {
		return store.OwnedTree{}, fmt.Errorf(
			"staging already holds unmatched content at %s; run `fu gc` to reclaim it before updating %q again, "+
				"and if `fu gc` reports nothing to collect, move that entry out of %s",
			filepath.Join(st.StagingDir(), name), name, st.StagingDir())
	} else if !errors.Is(err, fs.ErrNotExist) {
		return store.OwnedTree{}, fmt.Errorf("check existing staging content at %s: %w",
			filepath.Join(st.StagingDir(), name), err)
	}
	// Taken even under --force: the exchange needs this manifest either way,
	// and it is the only thing that proves which tree is being replaced.
	published, err := st.SnapshotSkillPayload(name)
	if err != nil {
		return store.OwnedTree{}, fmt.Errorf("snapshot published skill %s: %w", filepath.Join(st.SkillsDir(), name), err)
	}
	if force {
		return published, nil
	}
	current, err := digestOwnedPayload(published)
	if err != nil {
		return store.OwnedTree{}, fmt.Errorf("digest published skill %s: %w", name, err)
	}
	// SPEC rule 3: the store copy is the user's to edit, and an edit is only
	// visible here as a digest that no longer matches the install baseline.
	// Pulling upstream over it would discard that edit with nothing to say so.
	if current != cfg.Digest(name) {
		return store.OwnedTree{}, fmt.Errorf("%w: %s; re-run with --force to overwrite the local changes",
			ErrLocallyModified, describeLocalModification(st, name, cfg.Digest(name), current))
	}
	return published, nil
}

// updateSkillContent is update's content shape: the upstream tree differs from
// the published one, so a full transaction stages the new tree and swaps it
// into place with one exchange (store.ExchangeStagedWithSkillOwned).
//
// The exchange, rather than a retirement followed by a publish, is what update
// needs and no other operation does: add and new publish into a free name, rm
// empties one, adopt moves one in, while update replaces a name every agent's
// symlink is already pointing at. Two steps would leave a window in which the
// skill is simply absent -- a window an agent starting a session in it observes
// as a broken link. ExchangeStagedWithSkillOwned carries the rest of that
// argument, including what its own validation can and cannot promise.
func updateSkillContent(st *store.Store, agents []agent.Agent, p *source.Prepared, name string, cand Candidate, recorded, fields map[string]string, force bool, h hooks) (UpdateOutcome, error) {
	outcome := UpdateOutcome{Name: name}
	operation := OperationOutcome{Name: name}
	txn := &TxnRecord{
		Op:   "update",
		Name: name,
		// Same [staging, store] order as new/add/adopt/rm, so a validator
		// written against the install pattern accepts update's records too.
		Targets: []string{
			filepath.Join("staging", name),
			filepath.Join("store", "skills", name),
		},
		SourceFields: fields,
	}
	_, runErr := run(st, agents, Op{
		Message:        "update: " + name,
		Txn:            txn,
		outcome:        &operation,
		AllowedChanges: []string{"fu.yaml", filepath.ToSlash(filepath.Join("skills", name))},
		ValidatePrepared: func(st *store.Store, prepared store.PreparedCommit) error {
			if txn.Payload == nil {
				return errors.New("update transaction has no staged manifest at commit preparation")
			}
			// By this point the exchange has already run, so the staged
			// manifest is what skills/<name> must hold.
			if err := st.ValidateSkillOwned(name, *txn.Payload); err != nil {
				return fmt.Errorf("validate exchanged skill before commit: %w", err)
			}
			return st.ValidatePreparedOwnedTree(prepared, filepath.ToSlash(filepath.Join("skills", name)), *txn.Payload)
		},
		Preflight: func(st *store.Store, cfg *store.Config) error {
			_, err := checkUpdateAvailable(st, cfg, name, cand, recorded, force)
			return err
		},
		Mutate: func(st *store.Store, cfg *store.Config) error {
			// Repeated against the config this Mutate actually received. The
			// write lock excludes other fu processes; this second check is
			// what protects against an external writer racing the read-only
			// preflight, the same double-check add performs (add.go).
			published, err := checkUpdateAvailable(st, cfg, name, cand, recorded, force)
			if err != nil {
				return err
			}
			// Recorded before anything moves. The exchange is this
			// transaction's only pivot, and matching one manifest or the other
			// against skills/<name> is how recovery learns which side of it a
			// crash landed on -- so both must be journalled before the tree
			// they describe can change.
			txn.PreviousPayload = &published
			txn.Stage = updateTxnSnapshotted
			if err := WriteTxn(st, txn); err != nil {
				return fmt.Errorf("record the tree being replaced: %w", err)
			}
			srcRoot, err := p.Root()
			if err != nil {
				return fmt.Errorf("use prepared source %s: %w", p.Dir(), err)
			}
			proj, err := skill.ProjectDir(srcRoot.FS(), cand.Subdir)
			if err != nil {
				return fmt.Errorf("project source %s: %w", cand.Subdir, err)
			}
			observedDigest, err := skill.DigestManifest(proj)
			if err != nil {
				return fmt.Errorf("digest source %s: %w", cand.Subdir, err)
			}
			if cand.Digest == "" || observedDigest != cand.Digest {
				return fmt.Errorf("prepared source candidate %s changed since inspection", cand.Subdir)
			}
			declared := declaredFromProjection(proj)
			txn.Declared = declared
			rootPayload, err := createTxnStagedRoot(st, txn, name, 0o755, h)
			if err != nil {
				return err
			}
			payload, err := st.CopyStagedTreeOwned(name, rootPayload, srcRoot, cand.Subdir, declared)
			if err != nil {
				return fmt.Errorf("copy %s into staging: %w", cand.Subdir, err)
			}
			// SPEC rule 7's path-safety half, run against the staged tree
			// before it can be exchanged in. Its structural half
			// (ValidateSkillDir) runs below, once the manifest describing the
			// tree it inspects has been journalled.
			if err := skill.ValidateLinks(manifestEntries(payload)); err != nil {
				return fmt.Errorf("path-safety check: %w", err)
			}
			txn.Payload = &payload
			txn.Declared = nil
			txn.Stage = updateTxnStaged
			if err := WriteTxn(st, txn); err != nil {
				return fmt.Errorf("record the staged replacement: %w", err)
			}
			if err := st.ValidateStagedOwned(name, payload); err != nil {
				return fmt.Errorf("validate exact staged skill: %w", err)
			}
			stagedRoot, err := st.StagingRoot()
			if err != nil {
				return err
			}
			if err := skill.ValidateSkillDir(stagedRoot.FS(), name); err != nil {
				return fmt.Errorf("validate staged skill: %w", err)
			}
			d, err := digestOwnedPayload(payload)
			if err != nil {
				return fmt.Errorf("digest staged skill ownership: %w", err)
			}
			if d != observedDigest {
				return fmt.Errorf("copied skill %s does not match the source digest verified before staging", cand.Subdir)
			}
			txn.Digest = d
			cfg.SetDigest(name, d)
			cfg.SetSourceFields(name, fields)
			return nil
		},
		Publish: func(st *store.Store) error {
			if txn.Payload == nil || txn.PreviousPayload == nil {
				return errors.New("update transaction has no staged and replaced ownership manifests")
			}
			// Publish runs after the config is saved and strictly before the
			// commit (Op.Publish, pipeline.go). That ordering is what makes
			// "committed implies exchanged" true, and it is the whole reason
			// recovery can decide this transaction by looking at HEAD and the
			// two manifests alone.
			if err := st.ExchangeStagedWithSkillOwned(name, *txn.Payload, *txn.PreviousPayload); err != nil {
				return err
			}
			txn.Stage = updateTxnExchanged
			if err := WriteTxn(st, txn); err != nil {
				return fmt.Errorf("record the completed exchange: %w", err)
			}
			return nil
		},
		// The exchange left the replaced tree at staging/<name>. Disposing of
		// it runs here, strictly after the transaction's terminal marker, and
		// never before it.
		//
		// What that copy is owed to is the operation commit, not the marker:
		// until the commit is durable, staging/<name> holds the only copy an
		// uncommitted rollback can exchange back, so reclaiming any earlier
		// would destroy exactly what the rollback needs. Once the commit is
		// written there is no rollback branch left to serve -- run returns
		// without rolling back for every post-commit failure (pipeline.go),
		// and a later recoverUpdate takes the committed branch because
		// currentHead != startHash (update_txn.go). Waiting for the marker as
		// well is what keeps this reclaim from ever becoming a precondition of
		// a recovery: past it, a crash or a failure here leaves an orphan
		// nothing is waiting on. That is the same ordering, and the same
		// reason, as rm's own reclaim (rm.go).
		//
		// It goes to disposal rather than to recovery/ because SPEC rule 3
		// promises the overwritten content stays recoverable from the store's
		// git history. Git is the backstop, so the staging copy is the second
		// copy, not the first, and quarantining it would retain a duplicate
		// nothing reads.
		afterTxnCleared: func(st *store.Store) {
			// h.beforeUpdateReclaim is a test-only crash seam, the counterpart
			// of rm's beforeReclaim and placed at the same point of the same
			// ordering (rm.go): production always passes the zero hooks value,
			// so this is a no-op there. Without it nothing outside this file
			// could stop the process between the terminal marker and the
			// inline reclaim, and design §7's acceptance for that window --
			// "崩溃在 afterTxnCleared 后，fu gc 能把 staging 孤立载荷收干净" --
			// could only be asserted against a hand-built record, which proves
			// gc's arm handles that shape without proving a real crash here
			// produces it.
			if err := h.fire(h.beforeUpdateReclaim); err != nil {
				return
			}
			reclaimExchangedUpdatePayload(st, *txn)
		},
	}, h)
	outcome.Operation = operation
	return outcome, runErr
}

// reclaimExchangedUpdatePayload removes the replaced tree the exchange left in
// staging, once the update is durably committed and its WAL is cleared.
//
// reclaimUpdateStagingPayload's error is deliberately dropped, for the reason
// reclaimCommittedRemovePayload drops its own: the update has already
// succeeded durably, so a failed reclamation must not turn it into a reported
// failure. It leaves behind the same orphan a crash at this point would -- one
// nothing is waiting on, since reclamation runs strictly after the terminal
// marker and is therefore never a recovery precondition. Both causes leave
// identical state, which is why `fu gc` has to be able to collect it.
func reclaimExchangedUpdatePayload(st *store.Store, record TxnRecord) {
	if record.PreviousPayload == nil {
		return
	}
	_ = reclaimUpdateStagingPayload(st, record.Name, *record.PreviousPayload)
}

// reclaimUpdateStagingPayload removes one tree an update replaced from its
// staging name. It is the one primitive both of update's residue collectors
// share: this operation's own inline reclaim above, whose caller must never
// see a failure because the update it is cleaning up after has already
// committed durably; and `fu gc`'s prune loop (txn_prune.go), which must
// report a failure to its caller.
//
// It is called unconditionally rather than only when something is there.
// store.RemoveOwnedTreeAt (retire.go) returns nil, not an error, when the name
// is already clear at both the live path and the deterministic retired sibling
// its own protocol can leave it parked at mid-removal -- the rule
// ReclaimRecoveryPayloadOwned states for the recovery side as "An already
// absent payload is not an error" (ownedtree.go), and that is precisely the ordinary,
// uncrashed state by the time gc's caller reaches this function: this same
// operation's own afterTxnCleared already reclaimed the tree synchronously, in
// the same process, so nothing is left to find. gc has no reason to tell that
// no-op apart from a real reclaim, because it does not count either one --
// PruneOutcome says why.
//
// Anchored to the staging root pinned at BeginWrite: the removal must land
// in the directory whose identity was validated at session start, not
// wherever the pathname happens to point by the time it runs.
func reclaimUpdateStagingPayload(st *store.Store, name string, expected store.OwnedTree) error {
	// The same guard store.ReclaimRecoveryPayloadOwned and
	// RecoveryPayloadSettled apply to their own name, and for the same reason
	// (round 2, Minor #3): `fu gc` reaches this with a Name taken straight off
	// a completed family's record, and nothing on the prune path validates that
	// field. store.RemoveOwnedTreeAt applies only the weaker validLogicalEntry,
	// which permits a .fu- name. The recovery path is already covered --
	// recoverUpdate runs skill.ValidateName before anything else -- so this
	// closes the gc path and keeps the pair symmetric about a discipline
	// RecoveryPayloadSettled's own comment says the whole file rests on.
	if err := skill.ValidateName(name); err != nil {
		return fmt.Errorf("refuse to reclaim staging content under %q, which is not a public skill name: %w", name, err)
	}
	stagingRoot, err := st.StagingRoot()
	if err != nil {
		return err
	}
	dir, err := stagingRoot.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return store.RemoveOwnedTreeAt(dir, name, expected)
}

// updateStagingPayloadSettled is store.RecoveryPayloadSettled's staging-side
// counterpart, and answers the same question for the same caller: `fu gc` has
// to decide whether a completed update family still has anything on disk that
// its manifest is the only proof of, in the one run where it cannot read the
// pending set and so cannot show any name is unclaimed (txn_prune.go).
//
// "Settled" has to mean settled at both names disposal uses, for the reason
// RecoveryPayloadSettled's own doc gives: RemoveOwnedTreeAt empties the tree,
// renames the root to a sibling derived from the manifest, and only then
// unlinks it, so a crash between the last two frees the live name while the
// emptied root remains -- and pruning on the strength of that free name
// destroys the only manifest the root could ever be resumed by.
func updateStagingPayloadSettled(st *store.Store, name string, expected store.OwnedTree) (bool, error) {
	// The third thing its counterpart does, and the one this was missing: gc
	// reaches here with a Name taken straight off a completed family's record,
	// which nothing on the prune path validates, and answering "settled" for a
	// name outside the public namespace would let the prune delete the manifest
	// on the strength of two Lstats fu should never have made. Both this and
	// RecoveryPayloadSettled are stats rather than deletions and the threat
	// model is single-user, so the impact is negligible -- but the pair is not
	// allowed to be asymmetric about a discipline the reclaim beside it applies
	// (reclaimUpdateStagingPayload above, store.RecoveryPayloadSettled).
	if err := skill.ValidateName(name); err != nil {
		return false, fmt.Errorf("refuse to inspect staging content under %q, which is not a public skill name: %w", name, err)
	}
	stagingRoot, err := st.StagingRoot()
	if err != nil {
		return false, err
	}
	present, err := updateStagingTreePresent(stagingRoot, name, expected)
	if err != nil {
		return false, err
	}
	return !present, nil
}

// updateStagingTreePresent reports whether an owned-tree removal at name has
// anything left to do: either the live name itself, or the deterministic
// retired sibling RemoveOwnedTreeAt's own protocol can leave it parked at
// between emptying it and unlinking it. Both must be checked, not just the
// live one -- a crash inside gc's own previous reclaim attempt can leave the
// live name already clear with the retired sibling still holding the tree,
// and that state still has work for RemoveOwnedTreeAt to finish, the same
// resumability RecoveryPayloadSettled exists to recognise on the recovery
// side (ownedtree.go).
//
// store.RetiredRecoveryRootName's name is recovery-flavoured, but its
// derivation is not: RemoveOwnedTreeAt applies the identical
// ownedCleanupRetiredName computation regardless of which directory it is
// asked to remove from, so the name it would derive for a staging root is
// exactly the one this function must check.
func updateStagingTreePresent(stagingRoot *os.Root, name string, expected store.OwnedTree) (bool, error) {
	for _, candidate := range []string{name, store.RetiredRecoveryRootName(name, expected)} {
		if _, err := stagingRoot.Lstat(candidate); err == nil {
			return true, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}
