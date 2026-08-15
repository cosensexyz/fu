package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/go-git/go-git/v5/storage/transactional"

	"github.com/cosensexyz/fu/internal/store"
)

const maxGitCloneBytes int64 = 512 << 20

type cloneRepositoryFunc func(context.Context, string, *git.CloneOptions, *cloneByteBudget) (*git.Repository, error)

// cloneRef clones src at a full reference form into dir and returns the
// repository, the reference name that was used, and its kind.
func cloneRef(ctx context.Context, src Source, dir string, ref plumbing.ReferenceName, kind string, budget *cloneByteBudget, clone cloneRepositoryFunc) (*git.Repository, plumbing.ReferenceName, string, error) {
	repo, err := clone(ctx, dir, &git.CloneOptions{
		URL:           src.URL,
		ReferenceName: ref,
		SingleBranch:  true,
		Depth:         1,
	}, budget)
	if err != nil {
		return nil, "", "", err
	}
	return repo, ref, kind, nil
}

// cloneSource shallow-clones src into dir and resolves the lock state: the
// full ref form that was checked out and the exact commit at its head. A
// user-supplied ref is tried as a branch first, then as a tag -- go-git's
// error for a missing branch is a transport-level one that does not say
// whether the name exists as a tag, so both forms are probed.
func cloneSource(src Source, dir string, reset func() error) (LockInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return cloneSourceWithContext(ctx, src, dir, reset)
}

func cloneSourceWithContext(ctx context.Context, src Source, dir string, reset func() error) (LockInfo, error) {
	return cloneSourceWithContextLimit(ctx, src, dir, reset, maxGitCloneBytes)
}

func cloneSourceWithContextLimit(ctx context.Context, src Source, dir string, reset func() error, limit int64) (LockInfo, error) {
	if limit <= 0 {
		return LockInfo{}, ErrSourceTooLarge
	}
	return cloneSourceWithContextBudget(ctx, src, dir, reset, &cloneByteBudget{remaining: limit}, cloneRepository)
}

func cloneSourceWithContextBudget(ctx context.Context, src Source, dir string, reset func() error, budget *cloneByteBudget, clone cloneRepositoryFunc) (LockInfo, error) {
	repo, refName, refKind, err := func() (*git.Repository, plumbing.ReferenceName, string, error) {
		if src.Ref == "" {
			repo, err := clone(ctx, dir, &git.CloneOptions{
				URL:          src.URL,
				SingleBranch: true,
				Depth:        1,
			}, budget)
			if err != nil {
				return nil, "", "", fmt.Errorf("clone %s: %w", src.URL, err)
			}
			head, err := repo.Head()
			if err != nil {
				return nil, "", "", fmt.Errorf("resolve cloned source HEAD: %w", err)
			}
			return repo, head.Name(), "branch", nil
		}
		branch, tag := plumbing.NewBranchReferenceName(src.Ref), plumbing.NewTagReferenceName(src.Ref)
		repo, ref, kind, err := cloneRef(ctx, src, dir, branch, "branch", budget, clone)
		if err == nil {
			return repo, ref, kind, nil
		}
		branchErr := err
		// A failed clone may have left partial output. Clear it through the
		// identity-bound scratch root before trying the tag form.
		if resetErr := reset(); resetErr != nil {
			return nil, "", "", fmt.Errorf("clear staging area after failed branch clone: %w", resetErr)
		}
		repo, ref, kind, err = cloneRef(ctx, src, dir, tag, "tag", budget, clone)
		if err != nil {
			return nil, "", "", fmt.Errorf("clone URL %s with ref %q: branch attempt: %w; tag attempt: %w", src.URL, src.Ref, branchErr, err)
		}
		return repo, ref, kind, nil
	}()
	if err != nil {
		return LockInfo{}, err
	}
	resolved, err := repo.ResolveRevision(plumbing.Revision(refName.String()))
	if err != nil {
		return LockInfo{}, fmt.Errorf("resolve cloned source revision %s: %w", refName, err)
	}
	if _, err := repo.CommitObject(*resolved); err != nil {
		return LockInfo{}, fmt.Errorf("resolve cloned source commit %s: %w", refName, err)
	}
	return LockInfo{
		Ref:     refName.String(),
		RefKind: refKind,
		Commit:  resolved.String(),
	}, nil
}

func cloneRepository(ctx context.Context, dir string, options *git.CloneOptions, budget *cloneByteBudget) (*git.Repository, error) {
	if budget == nil {
		return nil, ErrSourceTooLarge
	}
	worktree := &limitedCloneFilesystem{
		Filesystem: osfs.New(dir),
		budget:     budget,
		ctx:        ctx,
	}
	endpoint, endpointErr := transport.NewEndpoint(options.URL)
	if endpointErr == nil && endpoint.Protocol == "file" {
		return checkoutLocalRepository(ctx, endpoint.Path, worktree, options)
	}
	dotGit, err := worktree.Chroot(git.GitDirName)
	if err != nil {
		return nil, err
	}
	storage := filesystem.NewStorage(dotGit, cache.NewObjectLRUDefault())
	return git.CloneContext(ctx, storage, worktree, options)
}

// checkoutLocalRepository avoids go-git's file transport, whose upload-pack
// subprocess can block forever after the destination refuses a packfile write.
// Objects stay in the source storer and only the selected tree is materialized
// through the context- and byte-limited destination filesystem.
func checkoutLocalRepository(ctx context.Context, sourcePath string, worktree *limitedCloneFilesystem, options *git.CloneOptions) (*git.Repository, error) {
	if err := cloneContextError(ctx); err != nil {
		return nil, err
	}
	// This path materializes one selected commit directly from an existing
	// local object store; it is not a general replacement for clone history.
	// Reject other clone shapes instead of silently ignoring their semantics.
	if options == nil || !options.SingleBranch || options.Depth != 1 {
		return nil, errors.New("local repository checkout requires SingleBranch with Depth: 1")
	}
	sourceRepo, err := git.PlainOpen(sourcePath)
	if err != nil {
		return nil, err
	}
	refName := options.ReferenceName
	if refName == "" {
		head, err := sourceRepo.Head()
		if err != nil {
			return nil, err
		}
		refName = head.Name()
	}
	resolved, err := sourceRepo.ResolveRevision(plumbing.Revision(refName.String()))
	if err != nil {
		return nil, err
	}

	temporal := memory.NewStorage()
	storage := transactional.NewStorage(sourceRepo.Storer, temporal)
	cfg, err := sourceRepo.Config()
	if err != nil {
		return nil, err
	}
	cfg.Core.IsBare = false
	cfg.Core.Worktree = ""
	if err := storage.SetConfig(cfg); err != nil {
		return nil, err
	}
	if err := storage.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, refName)); err != nil {
		return nil, err
	}
	repo, err := git.Open(storage, worktree)
	if err != nil {
		return nil, err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, err
	}
	if err := wt.Checkout(&git.CheckoutOptions{Hash: *resolved, Force: true}); err != nil {
		return nil, err
	}
	if err := cloneContextError(ctx); err != nil {
		return nil, err
	}
	if err := storage.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, refName)); err != nil {
		return nil, err
	}
	return repo, nil
}

// Prepare makes a source ready for scanning and copying. A git source is
// shallow-cloned under stagingDir (the clone's worktree is the scan/copy
// root); a local source is the path itself. Close removes a git clone's
// staging directory. Git scratch is held by parent/root descriptors and an
// inode identity so cleanup cannot follow a replaced pathname.
func (s Source) Prepare(stagingDir string) (*Prepared, error) {
	return s.prepare(stagingDir, store.FileIdentity{})
}

// PrepareChecked prepares a source while requiring git scratch to be created
// in the exact staging directory validated by Store.Open.
func (s Source) PrepareChecked(stagingDir string, stagingIdentity store.FileIdentity) (*Prepared, error) {
	if s.Kind == KindGit && stagingIdentity.Inode == 0 {
		return nil, errors.New("validated staging directory identity is missing")
	}
	return s.prepare(stagingDir, stagingIdentity)
}

func (s Source) prepare(stagingDir string, stagingIdentity store.FileIdentity) (*Prepared, error) {
	switch s.Kind {
	case KindGit:
		var scratch *ownedScratch
		var err error
		if stagingIdentity.Inode == 0 {
			scratch, err = newOwnedScratch(stagingDir)
		} else {
			scratch, err = newOwnedScratchChecked(stagingDir, stagingIdentity)
		}
		if err != nil {
			return nil, fmt.Errorf("create staging area for %s: %w", s.URL, err)
		}
		lock, err := cloneSource(s, scratch.Path(), scratch.Reset)
		if err != nil {
			if cleanupErr := scratch.Close(); cleanupErr != nil {
				return nil, errors.Join(err, fmt.Errorf("clean source staging area: %w", cleanupErr))
			}
			return nil, err
		}
		return &Prepared{src: s, dir: scratch.Path(), root: scratch.root, lock: lock, cleanup: scratch.Close}, nil
	case KindLocal:
		root, err := openPreparedRoot(s.Path)
		if err != nil {
			return nil, fmt.Errorf("local source %s: %w", s.Path, err)
		}
		return &Prepared{src: s, dir: s.Path, root: root, cleanup: root.Close}, nil
	default:
		return nil, errors.New("unknown source kind")
	}
}

func openPreparedRoot(path string) (*os.Root, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), path)
	if dir == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open prepared source: invalid descriptor")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	rootInfo, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		_ = dir.Close()
		return nil, err
	}
	dirInfo, err := dir.Stat()
	if err != nil {
		_ = root.Close()
		_ = dir.Close()
		return nil, err
	}
	if !os.SameFile(rootInfo, dirInfo) {
		_ = root.Close()
		_ = dir.Close()
		return nil, errors.New("prepared source was replaced while opening it")
	}
	if err := dir.Close(); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}
