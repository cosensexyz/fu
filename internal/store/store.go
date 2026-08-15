// internal/store/store.go
package store

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"golang.org/x/sys/unix"
)

var bootstrapConfig = []byte("version: 1\nskills: {}\n")

var (
	// ErrStoreNotFound is returned by Open when home has no store to open.
	ErrStoreNotFound = errors.New("store not initialized; run `fu init` first")
	// ErrStoreExists is returned by Init when home already has a store.
	ErrStoreExists = errors.New("store already initialized")
)

// Store is an opened fu store rooted at $FU_HOME/store (SPEC §4.2 layout).
type Store struct {
	Home string
	Repo *git.Repository

	homeIdentity     os.FileInfo
	storeIdentity    os.FileInfo
	skillsIdentity   os.FileInfo
	gitIdentity      os.FileInfo
	stagingIdentity  os.FileInfo
	recoveryIdentity os.FileInfo
	writeRoots       *checkedRoots
	worktreeFS       *rootFilesystem
}

// WriteSession pins the Store.Open-validated home, store, skills, Git,
// staging, and recovery directories for a complete write command. Each class
// of mutation uses its own held descriptor, so replacing any logical pathname
// cannot redirect config, Git, WAL, publication, cleanup, or rollback.
type WriteSession struct {
	Store    *Store
	original *Store
}

// BeginWrite opens every logical root without following its final component,
// validates the identities captured by Init or Open, and reopens go-git on
// the pinned store and Git descriptors.
func (s *Store) BeginWrite() (*WriteSession, error) {
	if s.homeIdentity == nil || s.storeIdentity == nil || s.skillsIdentity == nil ||
		s.gitIdentity == nil || s.stagingIdentity == nil || s.recoveryIdentity == nil {
		return nil, errors.New("store has no validated filesystem identity; reopen it before writing")
	}
	roots := &checkedRoots{}
	fail := func(err error) (*WriteSession, error) {
		_ = roots.close()
		return nil, err
	}
	var err error
	roots.home, err = openCheckedTop(s.Home, s.homeIdentity)
	if err != nil {
		return fail(err)
	}
	roots.store, err = openCheckedChild(roots.home, "store", s.Dir(), s.storeIdentity)
	if err != nil {
		return fail(err)
	}
	roots.skills, err = openCheckedChild(roots.store, "skills", s.SkillsDir(), s.skillsIdentity)
	if err != nil {
		return fail(err)
	}
	roots.git, err = openCheckedChild(roots.store, git.GitDirName,
		filepath.Join(s.Dir(), git.GitDirName), s.gitIdentity)
	if err != nil {
		return fail(err)
	}
	roots.staging, err = openCheckedChild(roots.home, "staging", s.StagingDir(), s.stagingIdentity)
	if err != nil {
		return fail(err)
	}
	roots.recovery, err = openCheckedChild(roots.home, "recovery", s.RecoveryDir(), s.recoveryIdentity)
	if err != nil {
		return fail(err)
	}

	pinned := &Store{
		Home:             s.Home,
		homeIdentity:     s.homeIdentity,
		storeIdentity:    s.storeIdentity,
		skillsIdentity:   s.skillsIdentity,
		gitIdentity:      s.gitIdentity,
		stagingIdentity:  s.stagingIdentity,
		recoveryIdentity: s.recoveryIdentity,
		writeRoots:       roots,
	}
	pinned.Repo, pinned.worktreeFS, err = openRepositoryForRoots(roots)
	if err != nil {
		return fail(fmt.Errorf("reopen checked store at %s: %w", s.Dir(), err))
	}
	return &WriteSession{Store: pinned, original: s}, nil
}

func openRepositoryForRoots(roots *checkedRoots) (*git.Repository, *rootFilesystem, error) {
	if roots == nil || roots.store == nil || roots.git == nil {
		return nil, nil, errors.New("checked repository roots are unavailable")
	}
	worktreeFS, err := newRootFilesystem(roots.store, ".")
	if err != nil {
		return nil, nil, fmt.Errorf("open checked worktree filesystem: %w", err)
	}
	if roots.skills != nil {
		if err := worktreeFS.Mount("skills", roots.skills); err != nil {
			return nil, nil, fmt.Errorf("mount checked skills root in worktree: %w", err)
		}
	}
	gitFS, err := newRootFilesystem(roots.git, ".")
	if err != nil {
		return nil, nil, fmt.Errorf("open checked git filesystem: %w", err)
	}
	repo, err := git.Open(filesystem.NewStorage(gitFS, cache.NewObjectLRUDefault()), worktreeFS)
	if err != nil {
		return nil, nil, err
	}
	return repo, worktreeFS, nil
}

// Root returns the borrowed FU_HOME root held for this write session. Callers
// use the logical-root accessors below for content operations. The session,
// not the caller, owns and closes every returned root.
func (s *Store) Root() (*os.Root, error) {
	if s.writeRoots == nil || s.writeRoots.home == nil || s.writeRoots.home.root == nil {
		return nil, errors.New("store is not attached to a checked write session")
	}
	return s.writeRoots.home.root, nil
}

func (s *Store) StoreRoot() (*os.Root, error) {
	if s.writeRoots == nil || s.writeRoots.store == nil || s.writeRoots.store.root == nil {
		return nil, errors.New("store is not attached to a checked store-root session")
	}
	return s.writeRoots.store.root, nil
}

func (s *Store) SkillsRoot() (*os.Root, error) {
	if s.writeRoots == nil || s.writeRoots.skills == nil || s.writeRoots.skills.root == nil {
		return nil, errors.New("store is not attached to a checked skills-root session")
	}
	return s.writeRoots.skills.root, nil
}

func (s *Store) StagingRoot() (*os.Root, error) {
	if s.writeRoots == nil || s.writeRoots.staging == nil || s.writeRoots.staging.root == nil {
		return nil, errors.New("store is not attached to a checked staging-root session")
	}
	return s.writeRoots.staging.root, nil
}

// StagingIdentity returns the inode identity validated when the store was
// opened. Source preparation compares it with the staging pathname before it
// creates scratch content there.
func (s *Store) StagingIdentity() (FileIdentity, error) {
	if s.stagingIdentity == nil {
		return FileIdentity{}, errors.New("store has no validated staging directory identity; reopen it before writing")
	}
	root, err := openCheckedTop(s.StagingDir(), s.stagingIdentity)
	if err != nil {
		return FileIdentity{}, fmt.Errorf("validated staging directory was replaced or is unavailable: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(root.dir.Fd()), &stat); err != nil {
		_ = root.close()
		return FileIdentity{}, fmt.Errorf("inspect validated staging directory identity: %w", err)
	}
	if err := root.close(); err != nil {
		return FileIdentity{}, fmt.Errorf("close validated staging directory identity descriptors: %w", err)
	}
	return identityFromStat(&stat), nil
}

func (s *Store) RecoveryRoot() (*os.Root, error) {
	if s.writeRoots == nil || s.writeRoots.recovery == nil || s.writeRoots.recovery.root == nil {
		return nil, errors.New("store is not attached to a checked recovery-root session")
	}
	return s.writeRoots.recovery.root, nil
}

// CheckCanonicalPath verifies that the user-facing FU_HOME pathname still
// reaches this session's identities before any agent-side reconciliation uses
// canonical store paths as durable symlink targets.
func (ws *WriteSession) CheckCanonicalPath() error {
	checks := []struct {
		path string
		want os.FileInfo
	}{
		{ws.original.Home, ws.original.homeIdentity},
		{ws.original.Dir(), ws.original.storeIdentity},
		{ws.original.SkillsDir(), ws.original.skillsIdentity},
		{filepath.Join(ws.original.Dir(), git.GitDirName), ws.original.gitIdentity},
		{ws.original.StagingDir(), ws.original.stagingIdentity},
		{ws.original.RecoveryDir(), ws.original.recoveryIdentity},
	}
	for _, check := range checks {
		info, err := os.Lstat(check.path)
		if err != nil {
			return fmt.Errorf("verify logical root %s after write: %w", check.path, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || check.want == nil || !os.SameFile(check.want, info) {
			return fmt.Errorf("%s was replaced while the write was in progress; validated data was updated through pinned descriptors, but agent links were not reconciled", check.path)
		}
	}
	return nil
}

// Close releases the checked root and the descriptor that backs pinned paths.
func (ws *WriteSession) Close() error {
	if ws.Store == nil || ws.Store.writeRoots == nil {
		return nil
	}
	err := ws.Store.writeRoots.close()
	ws.Store.writeRoots = nil
	return err
}

func (s *Store) rememberIdentity() error {
	identities := []struct {
		path string
		dest *os.FileInfo
	}{
		{s.Home, &s.homeIdentity},
		{s.Dir(), &s.storeIdentity},
		{s.SkillsDir(), &s.skillsIdentity},
		{filepath.Join(s.Dir(), git.GitDirName), &s.gitIdentity},
		{s.StagingDir(), &s.stagingIdentity},
		{s.RecoveryDir(), &s.recoveryIdentity},
	}
	for _, identity := range identities {
		info, err := os.Stat(identity.path)
		if err != nil {
			return fmt.Errorf("stat logical store root %s: %w", identity.path, err)
		}
		*identity.dest = info
	}
	return nil
}

func (s *Store) rememberCheckedIdentities(roots *checkedRoots) error {
	identities := []struct {
		root *checkedRoot
		dest *os.FileInfo
	}{
		{roots.home, &s.homeIdentity},
		{roots.store, &s.storeIdentity},
		{roots.skills, &s.skillsIdentity},
		{roots.git, &s.gitIdentity},
		{roots.staging, &s.stagingIdentity},
		{roots.recovery, &s.recoveryIdentity},
	}
	for _, identity := range identities {
		if identity.root == nil || identity.root.dir == nil {
			return errors.New("cannot capture identity for an unavailable logical root")
		}
		info, err := identity.root.dir.Stat()
		if err != nil {
			return fmt.Errorf("fstat pinned logical root %s: %w", identity.root.display, err)
		}
		*identity.dest = info
	}
	return nil
}

func verifyCheckedRootsStillNamed(roots *checkedRoots) error {
	for _, root := range []*checkedRoot{roots.home, roots.store, roots.skills, roots.git, roots.staging, roots.recovery} {
		if root == nil || root.dir == nil {
			return errors.New("cannot verify an unavailable logical root")
		}
		opened, err := root.dir.Stat()
		if err != nil {
			return fmt.Errorf("fstat pinned logical root %s: %w", root.display, err)
		}
		current, err := os.Lstat(root.display)
		if err != nil {
			return fmt.Errorf("verify logical root %s before returning it: %w", root.display, err)
		}
		if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
			return fmt.Errorf("logical root %s was replaced while the store was being opened", root.display)
		}
	}
	return nil
}

// Home resolves FU_HOME (env override, else ~/.fu). HOME is read directly
// from the environment for macOS test compatibility.
//
// The result is resolved through any symlinks (finding I5): ownership
// decisions (ownsLink) are a raw string comparison between a link's
// recorded target and a freshly computed store path, with no filesystem
// access of their own, so two processes reaching the same physical store
// through different spellings -- FU_HOME set explicitly in one, unset
// (falling back through a symlinked ~/.fu, e.g. from a dotfiles manager)
// in the other -- used to disagree about which links are fu's own.
// resolveExisting leaves a spelling that does not exist on disk yet
// (e.g. before the store has ever been created) unchanged, since there
// is nothing yet to resolve; the first write command to actually create
// it then fixes the canonical form for every later caller.
// Both spellings must be absolute (round 6 finding, closing the other half
// of what DESIGN §6 recorded as an FU_HOME-only gap). A relative value was
// neither refused nor absolutized, so every layout path derived from it
// stayed relative to whatever directory fu happened to be launched from:
// two invocations from different directories address different stores, and
// the absolute link targets written from one are broken when read from the
// other, then rebuilt endlessly.
//
// Refusing beats normalizing with filepath.Abs here. Absolutizing would
// silently make the store's *identity* depend on where `fu init` first ran
// -- a subtler failure than a refusal, and one the user would discover only
// once their skills had gone missing. agent.homeDir refuses a relative HOME
// for the same reason.
func Home() (string, error) {
	h := os.Getenv("FU_HOME")
	if h == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return "", errors.New("HOME not set")
		}
		if !filepath.IsAbs(home) {
			return "", fmt.Errorf("HOME is %q, which is not an absolute path; "+
				"fu resolves its store beneath HOME and cannot do so relative to the current directory", home)
		}
		h = filepath.Join(home, ".fu")
	} else if !filepath.IsAbs(h) {
		return "", fmt.Errorf("FU_HOME is %q, which is not an absolute path; "+
			"a store addressed relative to the current directory would differ between invocations", h)
	}
	return resolveExisting(h), nil
}

// resolveExisting returns path with symlinks resolved via
// filepath.EvalSymlinks, or path unchanged if it cannot be resolved (most
// commonly because it does not exist yet).
func resolveExisting(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}

// Dir returns the store's git repository root, $FU_HOME/store.
func (s *Store) Dir() string { return filepath.Join(s.Home, "store") }

// SkillsDir returns the directory holding skill entities, inside the repo.
func (s *Store) SkillsDir() string { return filepath.Join(s.Dir(), "skills") }

// ConfigPath returns the path to fu.yaml, inside the repo.
func (s *Store) ConfigPath() string { return filepath.Join(s.Dir(), "fu.yaml") }

// StagingDir returns the machine-local write-staging area: a sibling of
// the repo directory, never a descendant of it, so it never appears in
// `git status` run against the store (SPEC §4.2).
func (s *Store) StagingDir() string { return filepath.Join(s.Home, "staging") }

// RecoveryDir returns the machine-local recovery archive, a sibling of
// the repo directory (outside version control, see StagingDir).
func (s *Store) RecoveryDir() string { return filepath.Join(s.Home, "recovery") }

// LockPath returns the machine-local lock file path, a sibling of the
// repo directory (outside version control, see StagingDir).
func (s *Store) LockPath() string { return filepath.Join(s.Home, "fu.lock") }

// Init creates the store skeleton and its initial commit. Init is safe to
// re-run over a partial state left by an interrupted previous run (crash,
// full disk, permission error mid-way): every step below tolerates being
// repeated, so recovering from a half-finished init is just running Init
// again, never manual cleanup.
func Init(home string) (*Store, error) {
	s := &Store{Home: home}

	repo, err := git.PlainOpen(s.Dir())
	switch {
	case err == nil:
		// A git repository is already there. Init's own commit is always
		// its last step, so a repository with a resolvable HEAD means a
		// previous Init already ran to completion: refuse. A repository
		// without one (unborn HEAD -- e.g. git.PlainInit ran but the
		// process died before the config write or the commit) is a
		// partial state, not a real store yet: fall through and reuse
		// the repository to finish the remaining steps.
		if _, headErr := repo.Head(); headErr == nil {
			return nil, ErrStoreExists
		} else if !errors.Is(headErr, plumbing.ErrReferenceNotFound) {
			return nil, fmt.Errorf("check existing store at %s: %w", s.Dir(), headErr)
		}
	case errors.Is(err, git.ErrRepositoryNotExists):
		// No repository yet: a fresh home, or a partial state from an
		// interrupted Init that never reached git.PlainInit. repo is nil
		// here; the repository-creation step below creates one.
	default:
		return nil, fmt.Errorf("check existing store at %s: %w", s.Dir(), err)
	}

	if err := checkHomeLayout(home); err != nil {
		return nil, err
	}
	if err := checkResumableStoreDir(s.Dir()); err != nil {
		return nil, err
	}

	for _, d := range []string{s.SkillsDir(), s.StagingDir(), s.RecoveryDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create directory %s: %w", d, err)
		}
	}

	if repo == nil {
		repo, err = git.PlainInit(s.Dir(), false)
		if err != nil {
			return nil, fmt.Errorf("create repository at %s: %w", s.Dir(), err)
		}
	}
	s.Repo = repo

	// An existing fu.yaml is necessarily the exact bootstrap document:
	// checkResumableStoreDir rejects every other shape before Init writes.
	if _, err := os.Stat(s.ConfigPath()); os.IsNotExist(err) {
		// The no-replace writer, not the replacing one: this branch runs only
		// when the destination is absent, so nothing is being overwritten, and
		// it holds the descriptor through publication and validates the
		// installed object. That leaves WriteFileAtomic with no production
		// caller at all (round 18 finding M4).
		if err := WriteFileAtomicNoReplace(s.ConfigPath(), bootstrapConfig, 0o644); err != nil {
			return nil, fmt.Errorf("write config %s: %w", s.ConfigPath(), err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("check config %s: %w", s.ConfigPath(), err)
	}

	if _, err := s.Commit("init: store"); err != nil {
		return nil, fmt.Errorf("create initial commit: %w", err)
	}
	if err := s.rememberIdentity(); err != nil {
		return nil, err
	}
	return s, nil
}

// storeDirEntries maps everything Init itself ever puts directly inside
// $FU_HOME/store to the filesystem type it creates it as. Anything else
// found there belongs to somebody, and it is not fu; anything of the wrong
// type at one of these names is not fu's either (round 7 finding).
//
// The type matters as much as the name, because every write addresses these
// positions by pathname. A symlink at "skills" was accepted by an earlier,
// name-only version of this check, and Init's MkdirAll, NewSkill's
// MkdirAll/WriteFile and every skill path built from SkillsDir then followed
// it: a successful `fu new` wrote outside the repository while git recorded
// nothing but the link. stageAll's absolute-symlink guard does not cover
// this -- a *relative* link is invisible to it. Names are the attacker's
// (or an accident's) to choose; types are not.
var storeDirEntries = map[string]struct {
	mode os.FileMode // the type bits Lstat must report, or 0 for a regular file
	what string
}{
	".git":    {os.ModeDir, "a directory"},
	"skills":  {os.ModeDir, "a directory"},
	"fu.yaml": {0, "a regular file"},
}

// machineLocalDirs are the directories fu creates directly under $FU_HOME,
// outside the git repository: the write staging area and the recovery
// archive. They are as much a write target as the repository is, and round
// 8 found them unguarded while the repository was not.
//
// os.MkdirAll accepts an existing symlink to a directory, so "create it if
// missing" silently adopted whatever a redirected name pointed at.
// NewSkill then cleared its own scratch path beneath staging with
// RemoveAll, and a staging root pointing at a directory of the user's own
// made that a recursive delete of their content: reproduced against the
// compiled binary as `fu new alpha` removing ~/precious/alpha entirely,
// while reporting "created alpha".
var machineLocalDirs = []string{"staging", "recovery"}

// checkHomeLayout verifies that $FU_HOME and the machine-local directories
// beneath it are real directories rather than links to somewhere else.
// Missing entries are not an error: Init and Open both create them.
func checkHomeLayout(home string) error {
	if err := requireRealDir(home); err != nil {
		return err
	}
	for _, name := range machineLocalDirs {
		if err := requireRealDir(filepath.Join(home, name)); err != nil {
			return err
		}
	}
	return nil
}

// requireRealDir reports an error unless path is absent or a real
// directory. A symlink is refused even when it points at a perfectly good
// directory: the question is not whether the destination is usable but
// whether fu put it there, and following it means writing somewhere the
// caller never named.
func requireRealDir(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink, but fu creates it as a real directory: refusing to "+
			"write through it (check FU_HOME, and inspect the path by hand)", path)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory, but fu creates it as one (check FU_HOME)", path)
	}
	return nil
}

// checkStoreLayout verifies that every layout entry present under dir is of
// the type fu creates it as, and that dir itself is a real directory rather
// than a link to one. Missing entries are not an error here: Init has yet
// to create them, and Open's own checks cover what must exist by then.
func checkStoreLayout(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect %s: %w", dir, err)
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a directory: refusing to treat it as a store "+
			"(check FU_HOME)", dir)
	}
	for name, want := range storeDirEntries {
		p := filepath.Join(dir, name)
		fi, err := os.Lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("inspect %s: %w", p, err)
		}
		if fi.Mode().Type() != want.mode {
			return fmt.Errorf("%s is not %s: fu creates it as %s, so this is not fu's own layout "+
				"and writing through it could land outside the store (check FU_HOME, and inspect "+
				"the path by hand)", p, want.what, want.what)
		}
	}
	return nil
}

func checkStoreLayoutRoot(root *os.Root, display string) error {
	if root == nil {
		return fmt.Errorf("inspect %s: pinned store root is unavailable", display)
	}
	for name, want := range storeDirEntries {
		fi, err := root.Lstat(name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("inspect %s: %w", filepath.Join(display, name), err)
		}
		if fi.Mode().Type() != want.mode {
			return fmt.Errorf("%s is not %s: fu creates it as %s, so this is not fu's own layout "+
				"and writing through it could land outside the store (check FU_HOME, and inspect "+
				"the path by hand)", filepath.Join(display, name), want.what, want.what)
		}
	}
	return nil
}

// checkResumableStoreDir refuses to initialize on top of a directory whose
// contents fu does not recognize as its own partial work (round 6 finding).
//
// Init has to tolerate *some* pre-existing state: a crash between
// git.PlainInit and the first commit leaves a repository with an unborn
// HEAD, and re-running Init is meant to be the whole recovery procedure for
// that. But "tolerate a partial init" had been implemented as "tolerate
// anything", so a mistyped or stale FU_HOME pointed at an ordinary
// directory made Init create a repository inside it and commit whatever it
// held -- somebody's documents, keys, an unrelated checkout -- under fu's
// own name and with fu's own message.
//
// A missing directory is fine (the ordinary first init). A recognized name
// is not enough, though: before the first commit Init can only have created
// an empty skills directory and the exact bootstrap config. Anything beneath
// skills, or any other config bytes, are unmanaged content and must be left
// untouched. checkStoreLayout first verifies the recognized entries' types so
// the descendant checks below never follow a substituted symlink.
func checkResumableStoreDir(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect %s: %w", dir, err)
	}
	if err := checkStoreLayout(dir); err != nil {
		return err
	}
	for _, e := range ents {
		if _, ok := storeDirEntries[e.Name()]; ok {
			switch e.Name() {
			case "skills":
				children, err := os.ReadDir(filepath.Join(dir, e.Name()))
				if err != nil {
					return fmt.Errorf("inspect %s: %w", filepath.Join(dir, e.Name()), err)
				}
				if len(children) != 0 {
					return fmt.Errorf("%s already contains skill content, which Init cannot create before its first commit: refusing to initialize over unmanaged content", filepath.Join(dir, e.Name()))
				}
			case "fu.yaml":
				raw, err := ReadConfigFile(filepath.Join(dir, e.Name()))
				if err != nil {
					return fmt.Errorf("inspect %s: %w", filepath.Join(dir, e.Name()), err)
				}
				if !bytes.Equal(raw, bootstrapConfig) {
					return fmt.Errorf("%s is not the bootstrap config Init writes before its first commit: refusing to initialize over unmanaged configuration", filepath.Join(dir, e.Name()))
				}
			}
			continue
		}
		return fmt.Errorf("%s already holds %q, which fu did not create: refusing to initialize a "+
			"store over content that is not fu's (check FU_HOME, or choose an empty directory)",
			dir, e.Name())
	}
	return nil
}

// configTrackedAtHead reports whether HEAD's tree carries fu.yaml. That is
// the store's identity marker: Init commits fu.yaml as its last step, so
// every real store has it tracked, and a clone of one does too.
//
// Named for exactly what it inspects (round 7 finding). Its predecessor was
// called configInHistory and its comment claimed to answer "has this
// repository ever been a fu store", which HEAD's tree cannot tell you --
// history could hold it on another branch, or have dropped it since. The
// narrower reading is the one the callers need and the one that is true.
func (s *Store) configTrackedAtHead(repo *git.Repository, head *plumbing.Reference) bool {
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return false
	}
	_, err = commit.File("fu.yaml")
	return err == nil
}

type storeOpenHooks struct {
	afterValidation func()
}

// Open opens an existing store; machine-local dirs are recreated if a
// manual copy dropped them (they are outside the git repo).
func Open(home string) (*Store, error) {
	return openStore(home, storeOpenHooks{})
}

func openStore(home string, hooks storeOpenHooks) (*Store, error) {
	s := &Store{Home: home}
	repo, err := git.PlainOpen(s.Dir())
	if err != nil {
		if errors.Is(err, git.ErrRepositoryNotExists) {
			return nil, ErrStoreNotFound
		}
		return nil, fmt.Errorf("open store at %s: %w", s.Dir(), err)
	}

	roots := &checkedRoots{}
	defer roots.close()
	roots.home, err = openPinnedTop(s.Home)
	if err != nil {
		return nil, err
	}
	roots.store, err = openPinnedChild(roots.home, "store", s.Dir())
	if err != nil {
		return nil, err
	}
	roots.git, err = openPinnedChild(roots.store, git.GitDirName, filepath.Join(s.Dir(), git.GitDirName))
	if err != nil {
		return nil, err
	}
	pinnedRepo, _, err := openRepositoryForRoots(roots)
	if err != nil {
		return nil, fmt.Errorf("open store at %s through pinned roots: %w", s.Dir(), err)
	}

	// A repository whose HEAD does not resolve yet is the same partial
	// state Init resumes (see Init's comment): git.PlainInit ran, but the
	// process died before the first commit, so there is no fu.yaml and no
	// history. That is not a real store to hand back to a caller -- report
	// it the same way as a genuinely absent repository.
	head, headErr := pinnedRepo.Head()
	if headErr != nil {
		if errors.Is(headErr, plumbing.ErrReferenceNotFound) {
			return nil, ErrStoreNotFound
		}
		return nil, fmt.Errorf("check store at %s: %w", s.Dir(), headErr)
	}
	// A git repository is not by itself a fu store (round 6 finding), and
	// neither is one that merely has a fu.yaml lying in its worktree (round
	// 7 finding). Identity is *tracked* fu.yaml at HEAD: Init commits it as
	// its last step, so every real store has it and so does any clone, while
	// nothing an unrelated checkout happens to contain -- a stray file, a
	// half-written note, a file some other tool dropped there -- can forge
	// it. Checking the worktree first, as the previous version did, let such
	// a lookalike through: Sweep then committed that repository's own
	// pending working-tree content under fu's name and message, before the
	// requested mutation had even been attempted.
	//
	// This test comes before the worktree is consulted at all, so its answer
	// cannot be influenced by what is on disk right now.
	if !s.configTrackedAtHead(pinnedRepo, head) {
		return nil, ErrStoreNotFound
	}
	// Tracked, so this is fu's store. Whether its config is currently on
	// disk is a separate question with its own answer: a deleted fu.yaml is
	// recoverable from the history that just proved this is a store, and
	// telling that user to run `fu init` would send them to a command that
	// refuses (the store already exists) while saying nothing about the copy
	// git is holding for them.
	if _, err := roots.store.root.Lstat("fu.yaml"); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("check store config %s: %w", s.ConfigPath(), err)
		}
		return nil, missingConfigErr(s.ConfigPath(), "does not exist")
	}
	roots.skills, err = openPinnedChild(roots.store, "skills", s.SkillsDir())
	if err != nil {
		return nil, err
	}
	// The layout must also be fu's own shape, not merely fu's own names
	// (round 7 finding): a store that acquired a symlink at "skills" after
	// Init must be refused here, before any write follows it out of the
	// repository. See checkStoreLayout.
	if err := checkStoreLayoutRoot(roots.store.root, s.Dir()); err != nil {
		return nil, err
	}

	// The machine-local roots are created through the already pinned home and
	// immediately opened without following links. Their identities therefore
	// describe the exact directories whose type was validated, never a later
	// pathname replacement.
	roots.staging, err = openOrCreatePinnedChild(roots.home, "staging", s.StagingDir(), 0o755)
	if err != nil {
		return nil, err
	}
	roots.recovery, err = openOrCreatePinnedChild(roots.home, "recovery", s.RecoveryDir(), 0o755)
	if err != nil {
		return nil, err
	}
	if err := s.rememberCheckedIdentities(roots); err != nil {
		return nil, err
	}
	if hooks.afterValidation != nil {
		hooks.afterValidation()
	}
	if err := verifyCheckedRootsStillNamed(roots); err != nil {
		return nil, err
	}
	s.Repo = repo
	return s, nil
}
