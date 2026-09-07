package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"
	"golang.org/x/sys/unix"
)

// remoteName is the single remote fu manages (SPEC §5.1: one remote,
// simplified). It lives in store/.git/config like any git remote and never
// in fu.yaml (SPEC §4.2), so a clone establishes it and `fu remote` reads
// and sets it through git's own configuration.
const remoteName = "origin"

// Remote reports the configured remote url. An unconfigured remote is a
// normal answer rather than an error: `fu remote` prints it and exits 0.
func (s *Store) Remote() (string, bool, error) {
	remote, err := s.Repo.Remote(remoteName)
	if errors.Is(err, git.ErrRemoteNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read remote %s: %w", remoteName, err)
	}
	urls := remote.Config().URLs
	if len(urls) == 0 {
		return "", false, nil
	}
	return urls[0], true, nil
}

// SetRemote records url as the single remote, replacing any previous one,
// and returns the url it replaced ("" when there was none). Validation is
// go-git's own endpoint parser, so anything a later fetch or push could
// address is accepted, local paths included; only an empty string and a url
// the parser cannot read are refused. Relative local paths are anchored to
// the setting command's working directory so later commands can run anywhere.
func (s *Store) SetRemote(url string) (string, error) {
	if strings.TrimSpace(url) == "" {
		return "", errors.New("remote url must not be empty")
	}
	endpoint, err := transport.NewEndpoint(url)
	if err != nil {
		return "", fmt.Errorf("remote url %q: %w", url, err)
	}
	if endpoint.Protocol == "file" && !filepath.IsAbs(url) && !strings.Contains(url, "://") {
		url = endpoint.Path
	}
	previous, configured, err := s.Remote()
	if err != nil {
		return "", err
	}
	if configured {
		if err := s.Repo.DeleteRemote(remoteName); err != nil {
			return "", fmt.Errorf("replace remote %s: %w", remoteName, err)
		}
	}
	if _, err := s.Repo.CreateRemote(&config.RemoteConfig{Name: remoteName, URLs: []string{url}}); err != nil {
		return "", fmt.Errorf("set remote %s: %w", remoteName, err)
	}
	return previous, nil
}

// currentBranch is the branch HEAD points at. fu syncs exactly that branch,
// so a detached HEAD -- which only direct git could produce -- is refused
// rather than guessed around.
func (s *Store) currentBranch() (plumbing.ReferenceName, error) {
	head, err := s.Repo.Storer.Reference(plumbing.HEAD)
	if err != nil {
		return "", fmt.Errorf("read HEAD: %w", err)
	}
	if head.Type() != plumbing.SymbolicReference {
		return "", errors.New("HEAD is detached; fu syncs the branch HEAD points at")
	}
	return head.Target(), nil
}

// ErrRemoteDiverged means the local branch and the remote branch each hold
// commits the other lacks. fu never merges (SPEC §5.1); the caller says
// which side to reconcile with git.
var ErrRemoteDiverged = errors.New("local and remote branches have diverged")

// ErrRemoteEmpty means the remote exists but holds no commits yet -- the
// state right after `git init --bare`, before the first push.
var ErrRemoteEmpty = errors.New("remote repository has no commits")

// ErrRemoteAuthentication wraps go-git's authentication failures so the
// message can carry the v1 scope: SSH through ssh-agent, HTTPS only for
// public repositories (DESIGN §4).
var ErrRemoteAuthentication = errors.New("remote refused the credentials go-git could offer")

// PushOutcome identifies the branch and commit selected for Push. Branch is
// empty on a local precondition failure; otherwise a transport error may
// still mean the transfer failed, so metadata alone does not imply success.
type PushOutcome struct {
	Branch   string
	Head     plumbing.Hash
	UpToDate bool
}

// Fetch brings the remote's refs/heads/* into refs/remotes/origin/*, pruning
// the tracking refs the remote no longer advertises. It writes nothing else
// -- no branch moves, no worktree change -- so it may run outside fu.lock;
// FastForward later reads what it left, and only that.
// Unlike git fetch --prune, go-git also removes a symbolic origin/HEAD:
// its prune matches it to refs/heads/HEAD, which the remote does not advertise.
// The branch tracking refs remain usable; git remote set-head origin -a can
// recreate the alias, but a later Fetch will prune it again.
func (s *Store) Fetch(ctx context.Context) (bool, error) {
	err := s.Repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: remoteName,
		RefSpecs:   []config.RefSpec{config.RefSpec("refs/heads/*:refs/remotes/" + remoteName + "/*")},
		// Prune drops every refs/remotes/origin/* the current remote does not
		// advertise. SetRemote rewrites only .git/config, so without this a
		// tracking ref left by the previous remote would survive the switch
		// and FastForward would read it as if it were the new remote's.
		//
		// The refspec deliberately carries no leading "+". go-git's prune
		// reverses the spec textually (config/refspec.go Reverse), so a "+"
		// lands mid-string and Dst then looks up "+refs/heads/<b>", which no
		// remote advertises: every tracking ref is pruned and re-created on
		// every fetch, and a no-op fetch reports itself as updated. Force in
		// the options is what the "+" would have meant -- a rewritten remote
		// branch still moves the tracking ref -- and go-git treats the two
		// identically when updating local refs (remote.go
		// updateLocalReferenceStorage).
		Prune: true,
		Force: true,
	})
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, git.NoErrAlreadyUpToDate):
		return false, nil
	case errors.Is(err, transport.ErrEmptyRemoteRepository):
		return false, ErrRemoteEmpty
	default:
		return false, describeTransportError(err)
	}
}

// Push sends the branch HEAD points at to the same-named remote branch. A
// remote that is ahead is reported as ErrRemoteDiverged: go-git checks that
// client-side against the refs the remote advertises, so nothing partial is
// ever sent.
func (s *Store) Push(ctx context.Context) (PushOutcome, error) {
	branch, err := s.currentBranch()
	if err != nil {
		return PushOutcome{}, err
	}
	local, err := s.Repo.Storer.Reference(branch)
	if err != nil {
		return PushOutcome{}, fmt.Errorf("read %s: %w", branch.Short(), err)
	}
	outcome := PushOutcome{Branch: branch.Short(), Head: local.Hash()}
	err = s.Repo.PushContext(ctx, &git.PushOptions{
		RemoteName: remoteName,
		RefSpecs:   []config.RefSpec{config.RefSpec(branch.String() + ":" + branch.String())},
	})
	switch {
	case err == nil:
		return outcome, nil
	case errors.Is(err, git.NoErrAlreadyUpToDate):
		outcome.UpToDate = true
		return outcome, nil
	case strings.Contains(err.Error(), "non-fast-forward update"):
		// go-git has no sentinel for this refusal: remote.go's
		// checkFastForwardUpdate builds it with fmt.Errorf, so the text is
		// the only handle.
		return outcome, fmt.Errorf("%w: %s/%s has commits this store does not have", ErrRemoteDiverged, remoteName, branch.Short())
	default:
		return outcome, describeTransportError(err)
	}
}

// describeTransportError turns go-git's two authentication failures into
// ErrRemoteAuthentication and passes everything else through unchanged.
func describeTransportError(err error) error {
	if errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed) {
		return fmt.Errorf("%w: %v; private HTTPS remotes are not supported, use an SSH url", ErrRemoteAuthentication, err)
	}
	return err
}

// cloneScratchPrefix names the staging entry Clone fills before the rename
// into place. engine/status.go lists it among the residue a process exit can
// leave behind; nothing collects it, because no record could prove it is
// fu's (the same reason .fu-src-* is left alone).
const cloneScratchPrefix = ".fu-clone-"

// cloneHooks is a test-only seam, like the hooks every other write path has.
type cloneHooks struct {
	beforeRename func() error // clone verified, store/ not yet in place
}

// Clone materializes a remote store under home and opens it. The clone lands
// in a staging scratch first and is renamed into $FU_HOME/store only after it
// proved to be a fu store, so an interrupted clone leaves no store/ at all.
// That matters beyond tidiness: a half-cloned directory with an unborn HEAD
// is exactly the shape Init treats as its own interrupted run and resumes
// over (checkResumableStoreDir), which would turn a torn clone into a fresh,
// empty store without anyone asking for one.
func Clone(ctx context.Context, home, url string) (*Store, error) {
	return cloneWithHooks(ctx, home, url, cloneHooks{})
}

func cloneWithHooks(ctx context.Context, home, url string, hooks cloneHooks) (*Store, error) {
	s := &Store{Home: home}
	if _, err := os.Lstat(s.Dir()); err == nil {
		return nil, fmt.Errorf("%w: %s already exists; fu clone needs no store there (remove it, or point FU_HOME elsewhere)", ErrStoreExists, s.Dir())
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect %s: %w", s.Dir(), err)
	}
	if err := checkHomeLayout(home); err != nil {
		return nil, err
	}
	for _, d := range []string{s.StagingDir(), s.RecoveryDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create directory %s: %w", d, err)
		}
	}
	scratch, err := os.MkdirTemp(s.StagingDir(), cloneScratchPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("create clone scratch under %s: %w", s.StagingDir(), err)
	}
	discard := func(err error) (*Store, error) {
		return nil, errors.Join(err, os.RemoveAll(scratch))
	}
	// MkdirTemp creates 0700; the directory becomes store/ and Init makes
	// that 0755.
	if err := os.Chmod(scratch, 0o755); err != nil {
		return discard(fmt.Errorf("set clone scratch mode: %w", err))
	}
	repo, err := git.PlainCloneContext(ctx, scratch, false, &git.CloneOptions{URL: url, RemoteName: remoteName})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return discard(fmt.Errorf("%w: %s is not a fu store", ErrRemoteEmpty, url))
	}
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		// The remote's HEAD names a branch that does not exist -- the shape
		// of a bare repository created with `git init --bare` (whose HEAD
		// defaults to init.defaultBranch, typically "main") whose only push
		// so far landed on a different branch, "master", which is what every
		// fu store pushes (SPEC scenario 4). Real git tolerates this with a
		// warning; go-git's clone does not follow a HEAD it cannot resolve at
		// all, so recover the same way: ask the remote what branches it
		// actually has and, when exactly one exists, clone that instead.
		repo, err = retryCloneAfterUnresolvedHead(ctx, scratch, url)
	}
	if err != nil {
		return discard(fmt.Errorf("clone %s: %w", url, describeTransportError(err)))
	}
	head, err := repo.Head()
	if err != nil {
		return discard(fmt.Errorf("%s is not a fu store: no branch to check out (%w)", url, err))
	}
	if !s.configTrackedAtHead(repo, head) {
		return discard(fmt.Errorf("%s is not a fu store: its %s does not track fu.yaml", url, head.Name().Short()))
	}
	// git records no empty directory, so a store pushed before its first
	// skill arrives without skills/, and Open requires it.
	if err := os.MkdirAll(filepath.Join(scratch, "skills"), 0o755); err != nil {
		return discard(fmt.Errorf("create %s: %w", filepath.Join(scratch, "skills"), err))
	}
	// The store-layout contract Open enforces (storeDirEntries, store.go)
	// must hold before the rename, not after: Open runs it too, but by then
	// the scratch would already be $FU_HOME/store, and a refusal at that
	// point leaves exactly the half store this function exists to avoid.
	if err := checkStoreLayout(scratch); err != nil {
		return discard(fmt.Errorf("%s is not a fu store: %w", url, err))
	}
	// Every other write path refuses a store holding a symlink with an
	// absolute target (checkNoAbsoluteSymlinks, git.go): go-git's chroot
	// filesystem rewrites such a target on checkout, so history can never
	// record it faithfully. Clone is a write path too and must refuse the
	// same shape before it ever reaches $FU_HOME/store, or every later write
	// command wedges against a link only a manual rm can remove.
	if err := checkNoAbsoluteSymlinksUnder(scratch); err != nil {
		return discard(err)
	}
	if hooks.beforeRename != nil {
		if err := hooks.beforeRename(); err != nil {
			return discard(err)
		}
	}
	if err := renameCloneIntoPlace(s.StagingDir(), filepath.Base(scratch), home); err != nil {
		return discard(err)
	}
	return Open(home)
}

// retryCloneAfterUnresolvedHead is called only after a clone has already
// failed with plumbing.ErrReferenceNotFound: the remote advertised a HEAD
// naming a branch it does not have. It lists the remote's actual branches
// and, when exactly one exists, clones that branch by name instead -- the
// one case where the branch fu should check out is unambiguous even though
// the remote's HEAD cannot say so. Any other count is refused with a message
// naming what was found, since fu clone has no flag to say which branch is
// meant and guessing would silently pick one.
//
// scratch is reset to empty first: the failed attempt above left it exactly
// that way (go-git's PlainCloneContext clears a scratch it did not create
// itself on failure), but this does not rely on that -- a retry must start
// from a demonstrably clean scratch regardless of what the failed attempt
// left behind.
func retryCloneAfterUnresolvedHead(ctx context.Context, scratch, url string) (*git.Repository, error) {
	branches, err := remoteBranches(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("its HEAD names a branch that does not exist, and listing its branches failed: %w", err)
	}
	switch len(branches) {
	case 0:
		return nil, errors.New("its HEAD names a branch that does not exist, and it advertises no branches at all; push to it before cloning")
	case 1:
		// fall through to the retry below
	default:
		names := make([]string, len(branches))
		for i, b := range branches {
			names[i] = b.Short()
		}
		return nil, fmt.Errorf("its HEAD names a branch that does not exist, and it advertises %d branches (%s); "+
			"fu clone cannot tell which one you mean, point the remote's HEAD at one of them "+
			"(git -C <remote> symbolic-ref HEAD refs/heads/<branch>) and clone again",
			len(branches), strings.Join(names, ", "))
	}
	if err := resetScratch(scratch); err != nil {
		return nil, err
	}
	repo, err := git.PlainCloneContext(ctx, scratch, false, &git.CloneOptions{
		URL: url, RemoteName: remoteName, ReferenceName: branches[0],
	})
	if err != nil {
		return nil, fmt.Errorf("its HEAD names a branch that does not exist; retrying against its only branch, %s, failed too: %w",
			branches[0].Short(), err)
	}
	return repo, nil
}

// remoteBranches lists the branches url advertises, without cloning
// anything: a bare git.Remote backed by an in-memory storage, the same
// approach `git ls-remote` takes.
func remoteBranches(ctx context.Context, url string) ([]plumbing.ReferenceName, error) {
	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: remoteName, URLs: []string{url}})
	refs, err := remote.ListContext(ctx, &git.ListOptions{})
	if err != nil {
		return nil, err
	}
	var branches []plumbing.ReferenceName
	for _, ref := range refs {
		if ref.Type() == plumbing.HashReference && ref.Name().IsBranch() {
			branches = append(branches, ref.Name())
		}
	}
	sort.Slice(branches, func(i, j int) bool { return branches[i] < branches[j] })
	return branches, nil
}

// resetScratch empties scratch and recreates it with the mode Clone sets up
// front, so a retried clone attempt starts from a directory demonstrably as
// clean as the one MkdirTemp first produced.
func resetScratch(scratch string) error {
	if err := os.RemoveAll(scratch); err != nil {
		return fmt.Errorf("clear clone scratch %s: %w", scratch, err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return fmt.Errorf("recreate clone scratch %s: %w", scratch, err)
	}
	// MkdirAll's mode is umask-masked, so this chmod is what makes a retried
	// clone's scratch the same 0755 the first attempt's was, and store/ end
	// up with one mode regardless of which path the clone took.
	if err := os.Chmod(scratch, 0o755); err != nil {
		return fmt.Errorf("set clone scratch mode %s: %w", scratch, err)
	}
	return nil
}

// checkNoAbsoluteSymlinksUnder refuses a plain directory tree containing a
// symlink whose target is an absolute path. It is checkNoAbsoluteSymlinks's
// (git.go) counterpart for a clone's scratch directory, which is not yet a
// Store when this must run -- verification happens before the rename into
// $FU_HOME/store, precisely so a refusal here leaves no half store behind.
//
// This reads the target with a plain os.Readlink, the same way `git status`
// or `ls -l` would. PlainCloneContext's checkout runs on go-billy's chroot
// filesystem, which rewrites an absolute target on both sides: its Symlink
// joins the target onto the chroot root before writing, and its Readlink
// strips that prefix again (go-billy helper/chroot/chroot.go). So the
// on-disk target a plain read-back sees here is the remote's absolute path
// prefixed with the scratch directory, not the remote's bytes verbatim. The
// question this check answers is unaffected: joining onto an absolute base
// stays absolute, so an absolute target in the remote is still absolute
// here, and DESIGN §6's known gap is that go-git cannot record an absolute
// target faithfully regardless of which absolute string it is.
//
// .git is skipped: git's own internal symlinks, if any, are its concern, not
// the store's, and it can be large.
func checkNoAbsoluteSymlinksUnder(dir string) error {
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(p)
		if err != nil {
			return fmt.Errorf("read symlink %s: %w", p, err)
		}
		if !filepath.IsAbs(target) {
			return nil
		}
		return fmt.Errorf("%s points at %s: %w; this cannot be recorded faithfully", p, target, ErrAbsoluteSymlink)
	})
}

// renameCloneIntoPlace moves staging/<scratchName> to <home>/store without
// replacing anything: a store that appeared meanwhile keeps its place and the
// clone is discarded by the caller.
func renameCloneIntoPlace(stagingDir, scratchName, home string) error {
	from, err := openDirectoryNoFollow(stagingDir)
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := openDirectoryNoFollow(home)
	if err != nil {
		return err
	}
	defer to.Close()
	if err := RenameNoReplaceAt(from, scratchName, to, "store"); err != nil {
		return fmt.Errorf("move the cloned store into %s: %w", filepath.Join(home, "store"), err)
	}
	return nil
}

// FastForwardOutcome reports what FastForward did to the branch HEAD points at.
type FastForwardOutcome struct {
	Branch         string
	From, To       plumbing.Hash // equal when nothing moved
	UpToDate       bool
	NoRemoteBranch bool
	Changed        []string
}

// FastForward moves the current branch to refs/remotes/origin/<branch> when
// that commit descends from the local one, converging the worktree and index
// first the way Revert does. It writes no commit: the remote's commits become
// local history exactly as they are, which is what keeps `fu log` numbering
// identical on every machine. Any other relation -- tracking ref missing,
// remote already contained, histories diverged -- leaves the store untouched
// and says which.
//
// The convergence and the reference update are two steps; a process killed
// between them leaves the worktree at the remote tree with the branch
// behind, and the next sweep records that as an external modification
// (DESIGN §6 known gaps, the same window Revert has).
//
// Runs only inside a write session: applyTreeToWorktree refuses otherwise.
// The caller must Sweep immediately before FastForward in the same locked
// session, so the index equals HEAD and pending hand edits are preserved.
func (s *Store) FastForward() (FastForwardOutcome, error) {
	if s.worktreeFS == nil {
		return FastForwardOutcome{}, errUnpinnedWorktree
	}
	branch, err := s.currentBranch()
	if err != nil {
		return FastForwardOutcome{}, err
	}
	local, err := s.Repo.Storer.Reference(branch)
	if err != nil {
		return FastForwardOutcome{}, fmt.Errorf("read %s: %w", branch.Short(), err)
	}
	outcome := FastForwardOutcome{Branch: branch.Short(), From: local.Hash(), To: local.Hash()}
	trackingName := plumbing.NewRemoteReferenceName(remoteName, branch.Short())
	tracking, err := s.Repo.Storer.Reference(trackingName)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		outcome.NoRemoteBranch = true
		return outcome, nil
	}
	if err != nil {
		return outcome, fmt.Errorf("read %s: %w", trackingName.Short(), err)
	}
	if tracking.Hash() == local.Hash() {
		outcome.UpToDate = true
		return outcome, nil
	}
	localCommit, err := s.Repo.CommitObject(local.Hash())
	if err != nil {
		return outcome, fmt.Errorf("load %s: %w", branch.Short(), err)
	}
	remoteCommit, err := s.Repo.CommitObject(tracking.Hash())
	if err != nil {
		return outcome, fmt.Errorf("load %s: %w", trackingName.Short(), err)
	}
	contained, err := remoteCommit.IsAncestor(localCommit)
	if err != nil {
		return outcome, err
	}
	if contained {
		outcome.UpToDate = true
		return outcome, nil
	}
	ahead, err := localCommit.IsAncestor(remoteCommit)
	if err != nil {
		return outcome, err
	}
	if !ahead {
		return outcome, fmt.Errorf("%w: %s and %s", ErrRemoteDiverged, branch.Short(), trackingName.Short())
	}
	outcome.To = tracking.Hash()
	tree, err := remoteCommit.Tree()
	if err != nil {
		return outcome, fmt.Errorf("load tree of %s: %w", trackingName.Short(), err)
	}
	paths, err := targetTreePaths(tree)
	if err != nil {
		return outcome, err
	}
	changed, err := s.applyTreeToWorktree(paths)
	outcome.Changed = changed
	if err != nil {
		return outcome, fmt.Errorf("fast-forward worktree to %s: %w", tracking.Hash().String()[:7], err)
	}
	updated := plumbing.NewHashReference(branch, tracking.Hash())
	if err := s.Repo.Storer.CheckAndSetReference(updated, local); err != nil {
		return outcome, fmt.Errorf("advance %s to %s: %w", branch.Short(), tracking.Hash().String()[:7], err)
	}
	return outcome, nil
}

func openDirectoryNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open directory %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open directory %s: invalid descriptor", path)
	}
	return f, nil
}
