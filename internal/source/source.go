// Package source abstracts a skill's origin: a git repository (URL + ref +
// resolved commit) or a local directory (path). It owns the fu.yaml source
// field schema (DESIGN §3) and the preparation step that turns a user-supplied
// source argument into a stable tree ready for scanning and copying.
//
// Dependency rule: store never imports this package. Config's source support
// is a generic scalar-mapping accessor (store.Config.SetSourceFields); the
// field names themselves live only here.
package source

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// Kind classifies a source record.
type Kind string

const (
	KindGit   Kind = "git"
	KindLocal Kind = "local"
)

// Source is a parsed, user-supplied source argument.
type Source struct {
	Kind Kind
	URL  string // git: the repository URL as given
	Ref  string // git: user-given ref text (branch or tag name); "" = default branch
	Path string // local: absolute, symlink-resolved directory path
}

// fullHashRe matches the 40-hex full commit hashes fu locks. A user-supplied
// ref of this shape is refused (see ParseArg): go-git cannot shallow-clone a
// bare sha, and a commit-pinned source can only arrive from a locked record
// (update/clone), not from `fu add`.
var fullHashRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ErrCommitRefUnsupported reports a 40-hex ref given to `fu add`.
var ErrCommitRefUnsupported = errors.New(
	"ref is a full commit hash; fu add locks whatever the given branch or tag currently points at, " +
		"so install with a branch or tag ref instead (a commit-pinned source can only come from an existing lock)")

// ErrRefRequiresGit reports an explicit --ref used with a local source.
var ErrRefRequiresGit = errors.New("--ref can only be used with a git URL")

// ErrInvalidRef marks a ref string that cannot be accepted as the short
// branch-or-tag value expected by fu add. Callers use it to distinguish a
// malformed flag value from a transport or filesystem failure.
var ErrInvalidRef = errors.New("invalid ref")

// IsGitURL reports whether go-git's endpoint parser recognizes arg as a
// non-file transport, including arbitrary-user and userless SCP forms.
func IsGitURL(arg string) bool {
	endpoint, err := transport.NewEndpoint(arg)
	return err == nil && (endpoint.Protocol != "file" || strings.HasPrefix(strings.ToLower(arg), "file:"))
}

// ParseArg classifies a `fu add` source argument without interpreting any
// part of a git URL as a ref. Refs are supplied separately through
// ParseArgWithRef, so repository paths containing '@' remain unambiguous. A
// local path must already exist and be a directory; it is absolutized and
// resolved through symlinks so the recorded source path is canonical.
func ParseArg(arg string) (Source, error) {
	if arg == "" {
		return Source{}, errors.New("empty source argument")
	}
	local, exists, localErr := parseExistingLocal(arg)
	if exists {
		return local, localErr
	}
	if IsGitURL(arg) {
		return Source{Kind: KindGit, URL: arg}, nil
	}
	// A bare full commit hash (no URL) is the same D1 refusal as the
	// url@<hash> form: it would otherwise be classified as a local path and
	// fail with a confusing "no such file or directory". Matched
	// case-insensitively because commit hashes are conventionally lowercase
	// but a user may type one uppercase; git refs keep their own
	// case-sensitive handling above.
	if fullHashRe.MatchString(strings.ToLower(arg)) {
		return Source{}, fmt.Errorf("source %q: %w", arg, ErrCommitRefUnsupported)
	}
	return Source{}, localErr
}

// parseExistingLocal resolves arg as a filesystem path before transport
// parsing. This gives a real local directory precedence over ambiguous SCP
// syntax such as host:path while still allowing that syntax when no such
// local path exists.
func parseExistingLocal(arg string) (Source, bool, error) {
	abs, err := filepath.Abs(arg)
	if err != nil {
		return Source{}, true, fmt.Errorf("resolve source path %q: %w", arg, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		exists := !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, unix.ENOTDIR)
		return Source{}, exists, fmt.Errorf("source path %q: %w", arg, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return Source{}, !errors.Is(err, fs.ErrNotExist), fmt.Errorf("source path %q: %w", arg, err)
	}
	if !info.IsDir() {
		return Source{}, true, fmt.Errorf("source path %q is not a directory", arg)
	}
	return Source{Kind: KindLocal, Path: resolved}, true, nil
}

// ParseArgWithRef parses a source argument plus the explicit ref supplied by
// the command syntax. It is kept separate from ParseArg so '@' remains an
// ordinary, unambiguous part of repository URLs.
func ParseArgWithRef(arg, ref string) (Source, error) {
	src, err := ParseArg(arg)
	if err != nil {
		return Source{}, err
	}
	if ref == "" {
		return src, nil
	}
	if src.Kind != KindGit {
		return Source{}, ErrRefRequiresGit
	}
	if fullHashRe.MatchString(strings.ToLower(ref)) {
		return Source{}, fmt.Errorf("source %q ref %q: %w", arg, ref, ErrCommitRefUnsupported)
	}
	// A fully-qualified ref is refused here rather than left to fail later.
	// NewBranchReferenceName("refs/heads/main").Validate() returns nil, so it
	// passed this check and cloneSource then probed refs/heads/refs/heads/main
	// and refs/tags/refs/heads/main, failing with a two-part transport error
	// that named neither the mistake nor the fix (round 18 finding M7). --ref
	// takes the short name; a branch containing '/' is unaffected.
	if strings.HasPrefix(ref, "refs/") {
		return Source{}, fmt.Errorf("%w: source %q ref %q is fully qualified; pass the branch or tag name alone (e.g. %q)",
			ErrInvalidRef, arg, ref, shortRefSuggestion(ref))
	}
	if err := plumbing.NewBranchReferenceName(ref).Validate(); err != nil {
		return Source{}, fmt.Errorf("%w: source %q has invalid ref %q: %v", ErrInvalidRef, arg, ref, err)
	}
	src.Ref = ref
	return src, nil
}

// shortRefSuggestion turns a fully-qualified ref into the short name --ref
// takes. The two standard prefixes are stripped whole, so a branch containing
// '/' survives ("refs/heads/feature/x" -> "feature/x"). Anything else falls
// back to the last component: the bare double TrimPrefix left an unrecognised
// prefix untouched, so `--ref refs/remotes/origin/main` was refused with a
// message suggesting exactly what had just been refused.
func shortRefSuggestion(ref string) string {
	short := strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/")
	if strings.HasPrefix(short, "refs/") {
		return path.Base(short)
	}
	return short
}

// LockInfo is the resolved, lockable state of a source at prepare time. For a
// git source it carries the full ref form and the exact commit the clone
// resolved to; for a local source it carries nothing git-shaped.
type LockInfo struct {
	Ref     string
	RefKind string // "branch" | "tag"; "" for local
	Commit  string // full commit hash; "" for local
}

// EncodeFields renders the fu.yaml `source` field set for one installed skill.
// subdir is the skill's path relative to the source root, or "." for the root
// itself, which is omitted from the record (DESIGN §3).
func (s Source) EncodeFields(subdir string, lock LockInfo) map[string]string {
	fields := make(map[string]string, 7)
	if subdir != "." && subdir != "" {
		fields["subdir"] = subdir
	}
	switch s.Kind {
	case KindGit:
		fields["type"] = "git"
		fields["url"] = s.URL
		if lock.Ref != "" {
			fields["ref"] = lock.Ref
		}
		if lock.RefKind != "" {
			fields["ref_kind"] = lock.RefKind
		}
		if lock.Commit != "" {
			fields["commit"] = lock.Commit
		}
	case KindLocal:
		fields["type"] = "local"
		fields["path"] = s.Path
	}
	return fields
}

// Prepared is a source made ready for scanning and copying: for a git source
// a shallow clone under staging, for a local source the path itself. Close
// removes a git clone's staging directory; the caller must Close when done.
type Prepared struct {
	src     Source
	dir     string
	root    *os.Root
	lock    LockInfo
	cleanup func() error
}

// Dir returns the source root: the clone worktree root for git sources, the
// local directory for local sources.
func (p *Prepared) Dir() string { return p.dir }

// FS returns the prepared source through the descriptor pinned at Prepare.
func (p *Prepared) FS() fs.FS {
	if p == nil || p.root == nil {
		return nil
	}
	return p.root.FS()
}

// Root returns the borrowed pinned root used for copying. The Prepared value
// owns it; callers must not close it.
func (p *Prepared) Root() (*os.Root, error) {
	if p == nil || p.root == nil {
		return nil, errors.New("prepared source is closed")
	}
	return p.root, nil
}

// Lock returns the lockable state resolved at prepare time.
func (p *Prepared) Lock() LockInfo { return p.lock }

// Close removes the prepared staging directory for a git source. Safe to call
// multiple times; a local source has nothing to clean up.
func (p *Prepared) Close() error {
	if p.cleanup != nil {
		if err := p.cleanup(); err != nil {
			return err
		}
		p.cleanup = nil
	}
	p.root = nil
	return nil
}
