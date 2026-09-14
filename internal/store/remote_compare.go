package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// ErrNoRemoteConfigured means this store has no remote to compare against.
// It is an error rather than a relation because there is no comparison to
// describe: the caller prints nothing rather than printing "unknown", which
// would read as a failure to answer a question nobody asked.
//
// Distinct from engine.ErrNoRemote, which says the same thing one layer up
// for `fu push` and `fu pull`. Neither wraps the other: they are two layers
// each refusing for their own reason, and a reader who assumes one is the
// other will look for a wrapping that is not there.
var ErrNoRemoteConfigured = errors.New("no remote configured")

// ErrRemoteUnreachable marks the one failure that is actually about the
// remote: fu asked and did not get an answer.
//
// Every other way CompareRemote can fail is local -- a detached HEAD, a
// branch with no commit, an unreadable config, a damaged object database --
// and fu never contacts the remote on those paths at all. Reporting them as
// "the remote could not be reached" asserts something the run never
// established, which is the failure this whole comparison is built to avoid;
// the sentinel is what lets the report tell the two apart.
var ErrRemoteUnreachable = errors.New("remote could not be reached")

// unreachable marks err as a transport failure without putting the sentinel's
// words into the message.
//
// A plain fmt.Errorf("%w: %w", ErrRemoteUnreachable, err) wrap works for
// errors.Is and reads terribly: the report already says "could not be
// reached" in its own words, so the line came out as "could not be reached:
// remote could not be reached: repository not found". Classification and
// prose are different jobs; this type does the first and leaves the second to
// whoever is writing the sentence.
type unreachable struct{ err error }

func (u unreachable) Error() string        { return u.err.Error() }
func (u unreachable) Unwrap() error        { return u.err }
func (u unreachable) Is(target error) bool { return target == ErrRemoteUnreachable }

// RemoteRelation is where the store's branch stands against the remote's
// branch of the same name.
//
// Seven values in one enum rather than a relation plus two booleans, because
// the reporting side's question is "which of these am I in" and one switch
// covers every answer -- with two flags beside it, a reader has three things
// to check and one of them is easy to forget.
//
// The subject shifts between them, which the names do not show: Ahead and
// Behind are about the local branch, while Empty and NoBranch are about the
// remote. Each constant says which below, and the mapping test catches an
// inversion by value -- but read the comment rather than the name.
type RemoteRelation int

const (
	// RemoteSynced: both sides name the same commit. The only relation the
	// hashes settle on their own, with no local object required.
	RemoteSynced RemoteRelation = iota
	// RemoteAhead: the remote's commit is an ancestor of the local branch, so
	// this store holds everything the remote does and more. `fu push` sends it.
	RemoteAhead
	// RemoteBehind: the local branch is an ancestor of the remote's commit.
	// `fu pull` fast-forwards to it.
	RemoteBehind
	// RemoteDiverged: each side holds a commit the other lacks. fu never
	// merges (SPEC §5.1); git resolves it.
	RemoteDiverged
	// RemoteUnknown: the remote named a commit this store does not have, so
	// the relation cannot be proved without fetching -- which a read-only
	// command must not do (SPEC §9).
	//
	// This is the value the whole contract exists for. It is NOT "behind",
	// though behind is by far the likeliest truth: after someone else pushes,
	// a store that has not fetched since holds no object that could establish
	// it, and reporting a guess as a finding is the one thing a status report
	// must not do. Neither is it a remote failure -- the remote answered, and
	// answered completely.
	RemoteUnknown
	// RemoteEmpty: the remote advertises no refs at all, the state right after
	// `git init --bare`. Not a failure, and not a relation between commits.
	RemoteEmpty
	// RemoteNoBranch: the remote has commits, but none on the branch this
	// store syncs.
	RemoteNoBranch
)

func (r RemoteRelation) String() string {
	switch r {
	case RemoteSynced:
		return "synced"
	case RemoteAhead:
		return "ahead"
	case RemoteBehind:
		return "behind"
	case RemoteDiverged:
		return "diverged"
	case RemoteUnknown:
		return "unknown"
	case RemoteEmpty:
		return "empty remote"
	case RemoteNoBranch:
		return "no matching branch"
	}
	return fmt.Sprintf("RemoteRelation(%d)", int(r))
}

// RemoteComparison is one answer to "where does my store stand against its
// remote", with both commits named so a reader can act on it directly.
type RemoteComparison struct {
	URL      string
	Branch   string // short name, e.g. "main"
	Local    string // the local branch's commit
	Remote   string // the remote branch's commit; empty for RemoteEmpty and RemoteNoBranch
	Relation RemoteRelation
}

// CompareRemote asks the remote which commit its copy of this store's branch
// is on, and says how the two relate -- without writing anything.
//
// One ls-remote and no object transfer at all: no tracking ref, no object, no
// FETCH_HEAD, no lock (SPEC §9). That restriction is what makes RemoteUnknown
// a real answer rather than a gap, and the reason it must stay: the moment
// this fetched an object to sharpen its verdict, `fu status` would be a write
// command, and the read-only guarantee the whole command rests on would be
// gone for the sake of a nicer sentence.
func (s *Store) CompareRemote(ctx context.Context) (RemoteComparison, error) {
	url, configured, err := s.Remote()
	if err != nil {
		return RemoteComparison{}, err
	}
	if !configured {
		return RemoteComparison{}, ErrNoRemoteConfigured
	}
	// The URL goes in before the first thing that can fail, so a report can
	// always say which store's remote the comparison was about -- a detached
	// HEAD used to come back with an empty URL and print a remote line naming
	// no remote.
	comparison := RemoteComparison{URL: url}
	branch, err := s.currentBranch()
	if err != nil {
		return comparison, err
	}
	comparison.Branch = branch.Short()

	local, err := s.Repo.Reference(branch, true)
	if err != nil {
		// An unborn branch: HEAD names it, nothing points at it yet. fu's own
		// Init commits, so this needs a hand-built store -- but a comparison
		// against nothing is not a relation, and saying so beats a panic.
		return comparison, fmt.Errorf("read local branch %s: %w", branch.Short(), err)
	}
	comparison.Local = local.Hash().String()

	refs, err := remoteBranchRefs(ctx, url)
	if err != nil {
		// An empty remote is a state, not a failure: go-git raises it as a
		// transport error because its callers are fetch and push, for which
		// there is genuinely nothing to transfer. A status report has
		// something to say about it, so it is mapped back to a relation here
		// rather than handed to the caller as an error.
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			comparison.Relation = RemoteEmpty
			return comparison, nil
		}
		return comparison, unreachable{describeTransportError(err)}
	}
	// No special case for an empty map. A remote holding only tags answers
	// perfectly well, so go-git does not call it empty, and calling it empty
	// here told the user it "has no commits yet" about a remote that plainly
	// has one -- what it lacks is the branch this store syncs, which the
	// lookup below already says, with the remedy that goes with it.
	// RemoteEmpty is left to mean exactly what the transport error above
	// means: the remote advertises nothing at all.
	remoteHash, found := refs[branch]
	if !found {
		comparison.Relation = RemoteNoBranch
		return comparison, nil
	}
	comparison.Remote = remoteHash.String()

	if remoteHash == local.Hash() {
		comparison.Relation = RemoteSynced
		return comparison, nil
	}
	relation, err := s.relateLocally(local.Hash(), remoteHash)
	if err != nil {
		return comparison, err
	}
	comparison.Relation = relation
	return comparison, nil
}

// relateLocally answers ahead/behind/diverged from the local object database
// alone, or Unknown when it does not hold the remote's commit.
//
// The presence check comes first and is separate from the ancestry walk on
// purpose: "this store has never seen that commit" is the ordinary, expected
// state this command reports as Unknown, while an ancestry walk that fails
// with both objects present means the object database is damaged. Folding the
// two together would report a corrupt repository as a routine "unknown" and
// leave the user with nothing to investigate.
func (s *Store) relateLocally(local, remote plumbing.Hash) (RemoteRelation, error) {
	localCommit, err := s.Repo.CommitObject(local)
	if err != nil {
		return RemoteUnknown, fmt.Errorf("read local commit %s: %w", local, err)
	}
	remoteCommit, err := s.Repo.CommitObject(remote)
	if err != nil {
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			return RemoteUnknown, nil
		}
		return RemoteUnknown, fmt.Errorf("read remote commit %s from the local object database: %w", remote, err)
	}
	// IsAncestor walks the local object database only; with both commits
	// present it never reaches the network.
	remoteIsAncestor, err := remoteCommit.IsAncestor(localCommit)
	if err != nil {
		return RemoteUnknown, fmt.Errorf("compare %s against %s: %w", remote, local, err)
	}
	localIsAncestor, err := localCommit.IsAncestor(remoteCommit)
	if err != nil {
		return RemoteUnknown, fmt.Errorf("compare %s against %s: %w", local, remote, err)
	}
	switch {
	case remoteIsAncestor:
		return RemoteAhead, nil
	case localIsAncestor:
		return RemoteBehind, nil
	}
	return RemoteDiverged, nil
}

// remoteBranchRefs is remoteBranches with the hashes kept: the comparison
// needs the commit each branch points at, which the name-only form discards.
func remoteBranchRefs(ctx context.Context, url string) (map[plumbing.ReferenceName]plumbing.Hash, error) {
	refs, err := listRemoteRefs(ctx, url)
	if err != nil {
		return nil, err
	}
	branches := map[plumbing.ReferenceName]plumbing.Hash{}
	for _, ref := range refs {
		if ref.Type() == plumbing.HashReference && ref.Name().IsBranch() {
			branches[ref.Name()] = ref.Hash()
		}
	}
	return branches, nil
}
