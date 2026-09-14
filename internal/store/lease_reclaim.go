package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// LeaseState is what a collector concluded about one lease. `fu status` and
// `fu gc` reach it through the same function, so the two commands cannot come
// to disagree about a directory the user is looking at.
type LeaseState int

const (
	// LeaseStateUnset is the zero value, and it is deliberately not a verdict.
	// A LeaseState is read out of maps keyed by name, where a miss yields the
	// zero value silently; with a real verdict at iota 0, every name no lease
	// covers would have come back carrying that verdict. It did: `fu status`
	// briefly reported every unleased staging entry as in use by a live
	// process, because the map said so by saying nothing.
	LeaseStateUnset LeaseState = iota
	// LeaseInUse: the lease is held, so its object is in use by a live
	// process. The only evidence fu accepts for this is the flock, and the
	// only thing to do about it is nothing.
	LeaseInUse
	// LeaseCollectable: the holder is gone and the evidence accounts for the
	// object, so `fu gc` removes both.
	LeaseCollectable
	// LeaseUnaccountable: the holder is gone and the evidence does not account
	// for what is there -- the object was replaced, or its identity no longer
	// matches what the lease recorded. Deleting it would be a guess, so both
	// the object and the lease are kept: the evidence has to stay beside the
	// object, or it degrades into residue nobody can ever explain.
	LeaseUnaccountable
	// LeaseVanished: the lease was gone by the time it was opened, which is
	// what another fu process releasing one looks like from here. Nothing to
	// count and nothing to say about it.
	LeaseVanished
)

func (s LeaseState) String() string {
	switch s {
	case LeaseStateUnset:
		return "unset"
	case LeaseInUse:
		return "in use"
	case LeaseCollectable:
		return "collectable"
	case LeaseUnaccountable:
		return "unaccountable"
	case LeaseVanished:
		return "vanished"
	}
	return fmt.Sprintf("LeaseState(%d)", int(s))
}

// LeaseFinding is one lease and what a collector may do about it.
type LeaseFinding struct {
	LeasePath string
	Name      string
	Kind      LeaseKind
	State     LeaseState
	// Reason explains an Unaccountable finding in the user's terms. Empty
	// otherwise.
	Reason string
	// identity is what the lease recorded, kept unexported: it decides which
	// removal authority applies and is not something a reporting layer should
	// be able to act on.
	identity *FileIdentity
}

// ScanLeases classifies every lease under stagingDir without changing
// anything.
//
// It is read-only in the sense SPEC §9 requires: the only thing it does beyond
// reading is take and immediately drop each lease's flock, which is how
// liveness is asked and leaves nothing behind -- no bytes change, no name
// changes, and nothing is created, since a lease file is only ever created by
// AcquireLease.
func ScanLeases(stagingDir string) ([]LeaseFinding, error) {
	return classifyLeases(stagingDir, nil)
}

// LeaseReclaimOutcome counts what a reclamation run did.
type LeaseReclaimOutcome struct {
	// Objects and Leases are removed counts. They differ whenever a lease
	// outlived its object, which every convergent crash boundary produces.
	Objects int
	Leases  int
	// InUse and Unaccountable are what the run deliberately left alone, and
	// Claimed is a third: an object a pending transaction's journal governs,
	// which recovery settles and gc must not touch.
	InUse         int
	Unaccountable int
	Claimed       int
	// Refusals explains each Unaccountable entry in the user's terms. The
	// classifier computes a reason for every refusal, and a count on its own
	// is the least useful thing to do with it -- this is the bucket with no
	// remedy, so the explanation is all fu has to offer about it. Carried out
	// with the counts because `fu gc` may be running where `fu status` cannot:
	// a home with no store yet is the one case the storeless sweep exists for,
	// and there status exits 1.
	Refusals []LeaseRefusal
}

// LeaseRefusal is one entry reclamation declined, and why.
type LeaseRefusal struct {
	Name   string
	Reason string
}

// Each object is removed while its lease is still held by this process, so a
// new holder cannot appear between the decision and the act: a would-be holder
// must create its lease with O_EXCL under a fresh token, and could not have
// this one.
//
// ReclaimLeases removes what the evidence accounts for and leaves everything
// else exactly as it found it.
//
// claimed names entries a pending transaction's journal still governs. A
// leased object can be journalled before its lease is dropped -- that handover
// is two steps, and this is the window between them -- and deleting one then
// leaves the journal naming a root that is gone, which no recovery pass can
// repair and which wedges every later command. Every other sweep in `fu gc`
// excludes claimed names for the same reason; this one was the exception.
// A caller with no store open passes nil, which is right: there is no journal
// to contradict.
func ReclaimLeases(stagingDir string, claimed map[string]bool) (LeaseReclaimOutcome, error) {
	var outcome LeaseReclaimOutcome
	_, err := classifyLeases(stagingDir, func(finding LeaseFinding, held, parent *os.File) error {
		// Written so that only one state proceeds to a deletion, rather than
		// listing the states that must not. The inverse shape shipped first,
		// and a state added later -- LeaseVanished, from a round of fixes that
		// had nothing to do with deleting -- fell through it into the removal
		// path, with no lock held and a name read before the lock was tried.
		// In a stress loop that deleted a live writer's private staged root on
		// the first iteration.
		//
		// A default of "do nothing" cannot make that mistake. The cost of
		// forgetting to list a new state here is that it is never collected,
		// which someone notices; the cost the other way round is a deletion
		// nobody asked for.
		switch finding.State {
		case LeaseCollectable:
			// fall through to the removal below
		case LeaseInUse:
			outcome.InUse++
			return nil
		case LeaseUnaccountable:
			outcome.Unaccountable++
			outcome.refuseEntries(finding)
			return nil
		case LeaseVanished:
			// Another process released it between the listing and the open.
			// Nothing to remove and nothing to report.
			return nil
		default:
			return nil
		}
		if held == nil {
			// Belt and braces for the same defect: every removal below acts
			// under the lock this classification was made under, and a state
			// that arrives without one has not been through that.
			return nil
		}
		if claimed[finding.Name] {
			// The journal has taken over and the lease is merely the older of
			// two claims. Recovery settles it; gc must not.
			outcome.Claimed++
			return nil
		}
		result, err := removeLeasedObject(parent, finding)
		if err != nil {
			return err
		}
		if result == payloadRemoved {
			outcome.Objects++
		}
		// The lease is dropped only once nothing is left for it to account
		// for. Removing it beside a surviving object -- which happened
		// whenever the directory-only unlink declined -- turned an object fu
		// had merely refused to delete into residue with no record at all, the
		// one degradation this whole mechanism exists to prevent. A refusal is
		// not a completion.
		if result == payloadRefused {
			outcome.Unaccountable++
			// No Reason from the classifier: this refusal happened at the
			// syscall, after classification had admitted the object, so the
			// explanation is what the syscall said no to.
			outcome.refuseEntries(LeaseFinding{
				LeasePath: finding.LeasePath,
				Name:      finding.Name,
				Reason:    "the object is no longer the empty directory its lease can account for",
			})
			return nil
		}
		// Otherwise last, and after the object: a crash between the two leaves
		// a lease naming an object that is gone, which the next run settles by
		// removing the lease alone -- the boundary converges rather than
		// stranding either.
		if err := unix.Unlinkat(int(parent.Fd()), filepath.Base(finding.LeasePath), 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return &os.PathError{Op: "remove lease", Path: finding.LeasePath, Err: err}
		}
		outcome.Leases++
		return nil
	})
	return outcome, err
}

// objectByLeaseFileName finds the object a lease covers without believing a word
// of its record, by resolving the token in the lease's own file name.
//
// This is not the record-trusting path round 4 closed. A record is content
// anyone may have written; a lease's file name is a directory entry, and there
// can be exactly one of them per token -- AcquireLease creates it with O_EXCL --
// so the name is the one part of a refused lease that is still evidence.
//
// It matters because the refusal is all fu has to offer here. Clearing the name
// outright (round 6) meant a producer killed mid-AttachIdentity left its
// directory unexplained: the object stopped being attributed to the lease
// verdict and fell through to a bare residue-prefix count, in the one bucket
// with no remedy. The count was unchanged; the sentence was gone.
//
// The shape guard still applies, and resolution still refuses a name that does
// not carry the token that spelled it, so a file planted at .fu-lease-clean-<T>
// resolves to nothing rather than onto somebody else's object.
func objectByLeaseFileName(parentFD int, leaseName string) string {
	token := strings.TrimPrefix(leaseName, LeasePrefix)
	if !isLeaseToken(token) {
		return ""
	}
	present, err := resolveLeasedObjectAt(parentFD, token)
	if err != nil {
		return ""
	}
	return present
}

// unreadableRecordReason says what is wrong with a record that would not parse.
//
// An empty one is called out separately because it is not the same news. A
// record fu cannot parse is residue; an empty one is most often a holder that
// has created its lease and not yet written it -- AcquireLease's create and its
// first write are two syscalls, and a sweep landing between them sees this. A
// death in that same window leaves the identical state, which is why it is still
// reported rather than hidden: a leftover nothing ever mentions is the failure
// this mechanism exists to end. Saying which it might be costs one clause.
func unreadableRecordReason(held *os.File, readErr error) string {
	if info, err := held.Stat(); err == nil && info.Size() == 0 {
		return "lease record is empty: either a holder that has just created it, or one that died before its first write"
	}
	return "lease file is unreadable: " + readErr.Error()
}

// refuseEntries records every staging entry one refusal leaves behind: the
// lease file, and the object it covers when one was resolved.
//
// One per entry, not one per lease, because `fu status` counts entries -- and
// the two commands giving different numbers for the same directory is the one
// thing sharing a classifier is supposed to prevent. A refused lease whose
// object resolved leaves two things on disk, and a user told "1" goes looking
// for one of them.
//
// Reasons fu has nothing to say for are skipped rather than padded with an
// empty sentence; nothing reaches here without one today, and a silent entry
// would be a count with a blank line under it.
func (o *LeaseReclaimOutcome) refuseEntries(finding LeaseFinding) {
	if finding.Reason == "" {
		return
	}
	o.Refusals = append(o.Refusals, LeaseRefusal{
		Name:   filepath.Base(finding.LeasePath),
		Reason: finding.Reason,
	})
	if finding.Name != "" {
		o.Refusals = append(o.Refusals, LeaseRefusal{Name: finding.Name, Reason: finding.Reason})
	}
}

// removeLeasedObject deletes the object a collectable lease accounts for, and
// reports whether there was one to delete.
//
// Two authorities, and which one applies depends on what the lease can prove:
//
//   - With an identity recorded, the object must still be that same object.
//     Anything else is a replacement and was classified Unaccountable before
//     reaching here.
//   - Without one -- the crash window between writing the lease and capturing
//     what it created -- the authority is the one this package already uses
//     for exactly this window (cleanupUnidentifiedEmptyScratch): a random
//     private name and a directory-only unlink. AT_REMOVEDIR fails on anything
//     that is not an empty directory, so a file, a symlink or a directory
//     somebody put content into is preserved by the syscall itself rather than
//     by a check that could be wrong.
func removeLeasedObject(parent *os.File, finding LeaseFinding) (payloadOutcome, error) {
	if beforeLeasedPayloadRemoveHook != nil {
		beforeLeasedPayloadRemoveHook(finding.Name)
	}
	err := unix.Unlinkat(int(parent.Fd()), finding.Name, unix.AT_REMOVEDIR)
	switch {
	case err == nil:
		return payloadRemoved, nil
	case errors.Is(err, unix.ENOENT):
		// The object is already gone: the holder removed it and died before
		// dropping the lease, or another run settled it between this one's
		// classification and this syscall. Nothing is left for the lease to
		// account for, so settling the lease alone is the convergence -- and
		// it is a completion, not a refusal. Counting it as unaccountable told
		// the user `fu status` would name something status names nothing of.
		return payloadAbsent, nil
	case errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOTDIR):
		// Not an empty directory. With an identity this cannot be a
		// replacement -- classification proved the object is fu's own -- so it
		// is a populated scratch, which is removed whole against that
		// identity.
		if finding.hasIdentity() {
			if err := removeLeasedTreeWhole(parent, finding); err != nil {
				return payloadRefused, err
			}
			return payloadRemoved, nil
		}
		return payloadRefused, nil
	default:
		return payloadRefused, &os.PathError{Op: "remove leased object", Path: finding.Name, Err: err}
	}
}

// payloadOutcome is what one removal attempt did. "Already gone" and "refused"
// are kept apart because they converge differently: nothing is left to account
// for in the first, so the lease is settled, while the second must keep its
// lease beside the object it declined to delete.
type payloadOutcome int

const (
	payloadRemoved payloadOutcome = iota
	payloadAbsent
	payloadRefused
)

// afterLeasedPayloadRetireHook fires once an object has been retired and
// before it is deleted. It exists so a test can occupy the live name at
// exactly that instant, which is the one moment that tells a retire-then-
// delete apart from a verify-then-delete. Nil outside tests.
var afterLeasedPayloadRetireHook func(live, retired string)

// beforeLeasedPayloadRetireHook fires after the live name's identity has been
// checked and before the rename that retires it. That is the window the
// post-move re-verification exists to cover, and the only place a test can
// stand to show it doing anything: RENAME_NOREPLACE constrains the
// destination, not the source, so a replacement swapped in here is what the
// rename carries away. Nil outside tests.
var beforeLeasedPayloadRetireHook func(live string)

// beforeLeasedPayloadRemoveHook fires after a lease has been classified
// collectable and before its object is removed. That is the window the
// directory-only unlink is the authority for: classification's emptiness probe
// and the syscall are two observations, and only the syscall decides. Nil
// outside tests.
var beforeLeasedPayloadRemoveHook func(name string)

// removeLeasedTreeWhole removes a populated leased object using the protocol
// the rest of this package uses to delete anything by name: retire it to a
// sibling under a no-replace rename, re-verify the identity of what actually
// moved, and only then remove that. Checking the live name and then deleting
// the live name leaves a window between the two; retiring first means the
// object being deleted is no longer reachable under a name anyone else uses.
//
// The retired name carries the lease's own token, so an interrupted removal is
// not stranded: the next run resolves the lease to the retired object exactly
// as it would to the live one, and finishes the job. That is also why the name
// is derived rather than random -- safety rests on the rename and the
// post-move revalidation, not on being unguessable, and a resumable name is
// worth more than an unpredictable one. It is the same reasoning
// RemoveOwnedTreeAt states for its own deterministic retired name.
//
// The residue is the one POSIX leaves everywhere else here: there is no
// portable conditional unlink by inode, so a same-UID racer that observes the
// retired name and replaces it between the revalidation and the removal is not
// excluded.
// The caller's own *os.File is threaded through rather than its descriptor
// number, because os.NewFile is not a borrow: it installs a finalizer that
// closes the descriptor when the wrapper is collected, so re-wrapping somebody
// else's fd hands the garbage collector a licence to close it underneath them.
// That produced a "bad file descriptor" in an unrelated test, by which point
// the number had been reused and the connection to this code was invisible.
func removeLeasedTreeWhole(parent *os.File, finding LeaseFinding) error {
	parentFD := int(parent.Fd())
	retired := RetiredLeasedPayloadPrefix + leaseTokenOf(finding.Name)
	if finding.Name != retired {
		live, _, err := EntryIdentityAt(parentFD, finding.Name)
		if err != nil {
			return &os.PathError{Op: "inspect leased object", Path: finding.Name, Err: err}
		}
		if !live.Same(*finding.identity) {
			return fmt.Errorf("leased object %s was replaced before removal", finding.Name)
		}
		if beforeLeasedPayloadRetireHook != nil {
			beforeLeasedPayloadRetireHook(finding.Name)
		}
		if err := RenameNoReplaceAt(parent, finding.Name, parent, retired); err != nil {
			return fmt.Errorf("retire leased object %s: %w", finding.Name, err)
		}
		if afterLeasedPayloadRetireHook != nil {
			afterLeasedPayloadRetireHook(finding.Name, retired)
		}
	}
	// After the move, against the object that actually moved.
	moved, _, err := EntryIdentityAt(parentFD, retired)
	if err != nil {
		return &os.PathError{Op: "inspect retired leased object", Path: retired, Err: err}
	}
	if !moved.Same(*finding.identity) {
		return fmt.Errorf("retired leased object %s is not the object the lease accounts for", retired)
	}
	// The one read or write in this file that still goes by pathname. Go has
	// no *at form of RemoveAll, and the package's descriptor-based tree removal
	// wants an OwnedTree manifest, which a leased payload -- arbitrary
	// downloaded content -- does not have. Writing a third deletion primitive
	// for one line is not worth it here, so the residual is stated instead: a
	// same-UID process that swaps $FU_HOME/staging between the retire above and
	// this call makes it find nothing at the path, whereupon the caller counts
	// a reclamation that did not happen and drops the lease beside a surviving
	// object. Same threat model as the rest of this class -- it needs write
	// access to $FU_HOME, which is enough to do the damage directly.
	return os.RemoveAll(filepath.Join(filepath.Dir(finding.LeasePath), retired))
}

func (f LeaseFinding) hasIdentity() bool { return f.identity != nil && f.identity.Valid() }

// classifyLeases is the single place a lease's state is decided. Both `fu
// status` and `fu gc` come through here -- status with a nil action, gc with
// one -- because a bucket count that disagrees with what the collector does is
// worse than no count at all.
func classifyLeases(stagingDir string, act func(LeaseFinding, *os.File, *os.File) error) ([]LeaseFinding, error) {
	parentFD, err := unix.Open(stagingDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, nil
		}
		return nil, &os.PathError{Op: "open", Path: stagingDir, Err: err}
	}
	parent := os.NewFile(uintptr(parentFD), stagingDir)
	if parent == nil {
		_ = unix.Close(parentFD)
		return nil, fmt.Errorf("open %s: invalid descriptor", stagingDir)
	}
	defer parent.Close()

	if afterStagingPinHook != nil {
		afterStagingPinHook()
	}

	// Listed through the pinned descriptor, not by pathname. Every decision
	// this function makes, and every unlink and rename it performs, acts
	// through `parent` -- the sole exception being removeLeasedTreeWhole's
	// closing RemoveAll, which says why at its own call site. Reading by name
	// would let a directory swapped in between the pin and the listing decide
	// what a verdict is about while the deletion landed somewhere else --
	// where no identity was ever checked. It takes write access to $FU_HOME to
	// arrange, which is already enough to do the damage directly; the reason
	// to close it anyway is that a half-pinned root invites the next reader to
	// assume the other half.
	names, err := parent.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	// A collector must hold the lease exclusively, because it decides and then
	// acts under the same lock. A scan must not: liveness means "somebody holds
	// this exclusively", and taking LOCK_EX to ask made every reader an answer
	// to its own question. Two concurrent `fu status` runs reported dozens of
	// phantom live holders to each other, and a status running beside `fu gc`
	// made gc skip payloads and call them in use. LOCK_SH still fails against a
	// holder's LOCK_EX, so the question is answered as well as before.
	//
	// What remains is the other direction: a reader's LOCK_SH still blocks a
	// collector's LOCK_EX, so a `fu status` running during `fu gc` can make gc
	// leave a payload it would have taken. flock offers no way to tell "an
	// exclusive holder" from "only readers" -- EWOULDBLOCK is one answer to two
	// questions -- and the error is toward leaving things alone, which the next
	// run settles. Recorded in DESIGN §6 rather than closed.
	probe := unix.LOCK_EX | unix.LOCK_NB
	if act == nil {
		probe = unix.LOCK_SH | unix.LOCK_NB
	}
	var findings []LeaseFinding
	var failures []error
	for _, entryName := range names {
		if !strings.HasPrefix(entryName, LeasePrefix) {
			continue
		}
		finding, held, err := classifyOneLease(parent, stagingDir, entryName, probe)
		if err != nil {
			// One lease that cannot be classified must not cost the rest their
			// sweep: `fu gc`'s posture everywhere else is that partial
			// progress is progress. One unreadable lease file used to make gc
			// reclaim nothing at all and exit 1, and `fu status` lose every
			// lease verdict it had already computed.
			finding = LeaseFinding{
				LeasePath: filepath.Join(stagingDir, entryName),
				State:     LeaseUnaccountable,
				Reason:    "lease could not be inspected: " + err.Error(),
			}
			held = nil
		}
		if act != nil {
			actErr := act(finding, held, parent)
			releaseLeaseFile(held)
			if actErr != nil {
				// Accumulated rather than returned: one payload that cannot be
				// removed must not stop the rest being reclaimed, which is the
				// posture `fu gc` takes everywhere else -- any deletion prefix
				// may be safely resumed, so partial progress is progress.
				failures = append(failures, actErr)
			}
		} else {
			releaseLeaseFile(held)
		}
		findings = append(findings, finding)
	}
	keepLeaseAlive(parent)
	return findings, errors.Join(failures...)
}

// classifyOneLease answers a single lease, returning the held descriptor when
// the holder is gone so the caller can act under the same lock it decided
// under. The returned file is nil for an in-use lease.
func classifyOneLease(parent *os.File, stagingDir, leaseName string, probe int) (LeaseFinding, *os.File, error) {
	parentFD := int(parent.Fd())
	// Kept for messages and for LeasePath only. Every syscall below goes
	// through parentFD, so no name here is ever resolved from the root again.
	path := filepath.Join(stagingDir, leaseName)
	finding := LeaseFinding{LeasePath: path}

	// The record is read before the lock is tried, because a held lease still
	// has to say which object it covers -- a reader that learned nothing from
	// a live lease would leave that object to be judged by its name alone,
	// which is what this whole mechanism replaced. flock is advisory and does
	// not stand in the way of reading.
	//
	// A torn read is possible here and is tolerated rather than prevented: the
	// record is rewritten exactly once, microseconds after it is created, and
	// the cost of losing that race is a live object reported without its name,
	// which is the conservative answer this code gave before leases existed.
	//
	// The same validation the locked path applies, because this name is what a
	// reporting layer keys its buckets by (engine/status.go) and a lie there is
	// this command's whole worth. Checking only that the name was a single
	// component was not enough: a lease file held by another process, naming an
	// object whose token is not its own, had `fu status` call a collectable
	// object "in use" while `fu gc` deleted it -- status and gc disagreeing,
	// which is the one thing sharing this function is supposed to prevent.
	var token string
	if unlocked, err := openLeaseRecordFileAt(parentFD, leaseName, path); err == nil {
		if record, readErr := readLeaseRecord(unlocked); readErr == nil {
			if leaseRecordFault(leaseName, record) == "" {
				finding.Name = record.Name
				finding.Kind = record.Kind
				token = record.Token
			}
		}
		_ = unlocked.Close()
	}

	if beforeLeaseLockHook != nil {
		beforeLeaseLockHook(path)
	}

	held, err := openLeaseFileAt(parentFD, leaseName, path, probe)
	if errors.Is(err, errLeaseHeld) {
		// The one liveness signal. Not a pid, not a timestamp, not the mere
		// existence of this file: only a lock the kernel would have dropped
		// had its holder died.
		//
		// The name is resolved through the token, as everywhere else: a holder
		// that renamed its object since writing the record would otherwise be
		// reported against a spelling that has moved on.
		//
		// The token comes from the validated record, not from the name it
		// carries. Deriving it from the name let a record point this verdict at
		// an object it had no relation to, since nothing then tied the two
		// together.
		finding.State = LeaseInUse
		if token != "" {
			if present, resolveErr := resolveLeasedObjectAt(parentFD, token); resolveErr == nil && present != "" {
				finding.Name = present
			}
		}
		return finding, nil, nil
	}
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			// Gone between the listing and the open, which is the ordinary
			// shape of another fu process releasing its lease. Reporting a
			// plain race as something fu cannot account for would alarm a
			// reader about nothing.
			finding.State = LeaseVanished
			return finding, nil, nil
		}
		if errors.Is(err, unix.ELOOP) {
			// A symlink planted at a lease name. Not fu's own evidence.
			finding.State = LeaseUnaccountable
			finding.Reason = "lease name is a symlink, so it is not fu's record"
			return finding, nil, nil
		}
		if errors.Is(err, errLeaseNotRegular) {
			// A FIFO, a device, a directory. fu only ever creates a regular
			// file at a lease name, so this is somebody else's and is reported
			// rather than opened, locked or acted on.
			finding.State = LeaseUnaccountable
			finding.Reason = "lease name is not a regular file, so it is not fu's record"
			return finding, nil, nil
		}
		return finding, nil, err
	}

	record, readErr := readLeaseRecord(held)
	if readErr != nil {
		// Everything taken from the record is dropped with it. The pre-lock
		// read may have left a name and a kind here, and the record that
		// produced them is now unreadable -- so reporting them would attribute
		// a refusal to whatever an earlier, different read of this file said.
		finding.Kind = ""
		finding.Name = objectByLeaseFileName(parentFD, leaseName)
		finding.State = LeaseUnaccountable
		finding.Reason = unreadableRecordReason(held, readErr)
		return finding, held, nil
	}
	// Nothing is taken from the record until the fault check has admitted it --
	// name, kind and identity alike. Assigning them first meant every refusal
	// still carried whatever the record claimed, which a reporting layer keys
	// its buckets by; the pre-lock read learned that in round 4 and this path
	// went on doing it for Kind and identity.
	if fault := leaseRecordFault(leaseName, record); fault != "" {
		finding.Kind = ""
		finding.Name = objectByLeaseFileName(parentFD, leaseName)
		finding.State = LeaseUnaccountable
		finding.Reason = fault
		return finding, held, nil
	}
	finding.Kind = record.Kind
	finding.identity = record.Identity

	finding.Name = record.Name
	present, err := resolveLeasedObjectAt(parentFD, record.Token)
	if err != nil {
		finding.State = LeaseUnaccountable
		finding.Reason = err.Error()
		return finding, held, nil
	}
	if present == "" {
		// Nothing carries the token. Either the object was never created (the
		// window between writing the lease and the mkdir) or it is already
		// gone (the window between removing it and dropping the lease).
		// Either way the lease is all that is left, and removing it is the
		// convergence.
		//
		// The name is cleared rather than left at whatever the record last
		// said: it names nothing now, and a caller that reads it as a live
		// spelling would think an object survived that does not exist.
		finding.Name = ""
		finding.State = LeaseCollectable
		return finding, held, nil
	}
	finding.Name = present

	if record.Identity == nil {
		// The window between the mkdir and capturing what it created. The
		// object is empty if it is fu's at all, and removeLeasedObject's
		// directory-only unlink is the authority the rest of this package
		// already uses for exactly this state.
		//
		// Asked here as well as there, because the two commands must give the
		// same answer: the unlink refuses anything but an empty directory, and
		// calling such an object collectable made `fu status` count a payload
		// `fu gc` then declined to remove. The syscall remains the authority --
		// this only stops the report contradicting it.
		if !leasedObjectIsEmptyDirectoryAt(parentFD, present) {
			finding.State = LeaseUnaccountable
			finding.Reason = "the lease records no identity, and its object is not the empty directory that window can leave"
			return finding, held, nil
		}
		finding.State = LeaseCollectable
		return finding, held, nil
	}
	actual, _, err := EntryIdentityAt(parentFD, present)
	if err != nil {
		finding.State = LeaseUnaccountable
		finding.Reason = "leased object could not be inspected: " + err.Error()
		return finding, held, nil
	}
	if !actual.Same(*record.Identity) {
		finding.State = LeaseUnaccountable
		finding.Reason = "another object now stands at the leased name"
		return finding, held, nil
	}
	finding.State = LeaseCollectable
	return finding, held, nil
}

// resolveLeasedObject finds the one entry carrying token, whatever prefix it
// currently wears. Empty means nothing does.
//
// More than one is refused rather than picked between: two objects sharing a
// token is a state fu cannot produce, so meeting one means something else made
// it, and guessing which to delete is exactly the guess this design exists to
// avoid.
// The path form is for a holder, which has its own object open and no pinned
// root to work from. A collector uses resolveLeasedObjectAt.
func resolveLeasedObject(stagingDir, token string) (string, error) {
	parentFD, err := unix.Open(stagingDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", &os.PathError{Op: "open", Path: stagingDir, Err: err}
	}
	defer unix.Close(parentFD)
	return resolveLeasedObjectAt(parentFD, token)
}

func resolveLeasedObjectAt(parentFD int, token string) (string, error) {
	var found []string
	for _, prefix := range LeasedObjectPrefixes {
		name := prefix + token
		// The name must resolve back to the token that spelled it. The
		// prefixes nest -- `.fu-src-` is a proper prefix of `.fu-src-clean-`
		// and `.fu-src-orphan-` -- so a token of "clean-<T>" spells, through
		// the shorter prefix, the object a live holder's lease covers under
		// <T>. Every bind rounds 2 and 3 added constrains a *name*, and the
		// deletion is driven by this resolution, so one planted file was
		// enough to make `fu gc` delete a live holder's quarantined scratch
		// and report it as reclaimed. The property belongs here, where the
		// object is chosen, rather than at each caller that acts on the
		// choice.
		if leaseTokenOf(name) != token {
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(parentFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
			found = append(found, name)
		} else if !errors.Is(err, unix.ENOENT) {
			return "", fmt.Errorf("inspect %s: %w", name, err)
		}
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("%d objects carry this lease's token (%s)", len(found), strings.Join(found, ", "))
}

func readLeaseRecord(file *os.File) (leaseRecord, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return leaseRecord{}, err
	}
	body, err := io.ReadAll(io.LimitReader(file, 64<<10))
	if err != nil {
		return leaseRecord{}, err
	}
	var record leaseRecord
	if err := json.Unmarshal(body, &record); err != nil {
		return leaseRecord{}, err
	}
	return record, nil
}

// leasedObjectIsEmptyDirectory reports whether an object is the empty directory
// a lease with no recorded identity can only have left behind. Anything else --
// a file, a symlink, a directory with content -- is not something that window
// produces, so it is not fu's to remove.
func leasedObjectIsEmptyDirectoryAt(parentFD int, name string) bool {
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	dir := os.NewFile(uintptr(fd), name)
	if dir == nil {
		_ = unix.Close(fd)
		return false
	}
	defer dir.Close()
	names, err := dir.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true
	}
	return err == nil && len(names) == 0
}

// beforeLeaseLockHook fires after a lease's record has been read and before its
// lock is tried. That is the exact window in which another process releasing a
// lease turns it into LeaseVanished -- with a name already in hand from the
// read, which is what made that state dangerous when it first went unhandled.
// A single-threaded test cannot otherwise reach it. Nil outside tests.
var beforeLeaseLockHook func(leasePath string)

// afterStagingPinHook fires once the staging directory is pinned and before
// anything is read from it. Every read after this point is supposed to go
// through that descriptor, so a test standing here can swap the directory out
// from under the name and require the pinned one to be what was acted on. It
// has to be this early: a hook placed later cannot see the reads that precede
// it, which is how a claim to cover five reads came to be tested against two.
// Nil outside tests.
var afterStagingPinHook func()

// openLeaseRecordFile opens a lease to read its record, without taking the
// lock. A held lease still has to say which object it covers, and flock is
// advisory, so reading one is legitimate -- but it is opened under the same
// refusals as any other lease descriptor, or a FIFO at a lease name blocks the
// reader forever and a symlink lets an outside file answer for a staging name.
func openLeaseRecordFileAt(parentFD int, name, path string) (*os.File, error) {
	fd, err := openLeaseDescriptorAt(parentFD, name, path)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open lease %s: invalid descriptor", path)
	}
	return file, nil
}

// leaseRecordFault reports why a record may not be believed, or "" when it may.
//
// Everything it checks turns into a path or into a verdict about a name, so it
// runs before either happens, on every path that reads a record -- the locked
// one and the pre-lock read alike. A record is written by fu but read back from
// disk, where anything may have edited it, so it is treated as input.
//
// Without the name check a lease record was a delete-anything instruction: a
// hand-written "name":"../../victim" had `fu gc` removing a tree outside
// $FU_HOME and reporting it as a reclaimed payload.
//
// The lease's own file name must be the one its token derives. Liveness is
// bound to a file's inode, but the object is found through the token inside it;
// a second file carrying the same token would answer for an object whose real
// lease is held elsewhere, and a plain copy of a lease file was enough to make
// gc delete a live holder's scratch.
//
// The object's name must carry that same token. A record naming an object
// whose token is somebody else's ties this lease's liveness to that object:
// held here, collectable there, so status called it in use while gc deleted it.
func leaseRecordFault(leaseName string, record leaseRecord) string {
	switch {
	case record.Version != leaseVersion:
		// A lease from a build that knows more than this one does. Its object
		// is not this build's to judge.
		return fmt.Sprintf("lease format version %d is newer than this build understands", record.Version)
	case !isLeaseToken(record.Token):
		// Stronger than isSingleStagingComponent, which every hex string
		// already satisfies. The shape is what stops a token carrying prefix
		// text: "clean-<T>" is a single component and is not a token.
		return "lease token is not a lease token"
	case record.Name != "" && !isSingleStagingComponent(record.Name):
		return "lease does not name a single entry under staging"
	case leaseName != LeasePrefix+record.Token:
		return "lease file name does not derive from its own token"
	case record.Name != "" && leaseTokenOf(record.Name) != record.Token:
		return "lease names an object that does not carry its token"
	}
	return ""
}

// isLeaseToken reports whether a token has the shape every producer generates:
// lowercase hex from 12 or 16 random bytes. Checked because a token is
// concatenated with a prefix to form a name, so a token carrying prefix text of
// its own can address an object that is not this lease's.
func isLeaseToken(token string) bool {
	if len(token) < 2*12 {
		return false
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isSingleStagingComponent is the guard every string that becomes a path under
// staging must pass. A lease record is written by fu but read back from disk,
// where anything may have edited it, so it is treated as input.
func isSingleStagingComponent(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsRune(name, '/')
}
