package source

import (
	"context"
	"errors"
	"fmt"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"
)

// ErrRemoteRefNotFound reports a ref the remote does not advertise. It is a
// distinct sentinel because the caller's response differs from a transport
// failure: a vanished ref is a per-skill refusal that names the recorded ref,
// while an unreachable remote leaves the skill's state simply unknown.
var ErrRemoteRefNotFound = errors.New("remote does not advertise the recorded ref")

// ErrRemoteRefUnusable reports a recorded ref that cannot name a commit, which
// is a fact about the record and not about the network. It is a distinct
// sentinel for the same reason ErrRemoteRefNotFound is: without it every
// non-vanished failure collapsed into "unreachable", so a fu.yaml hand-edited
// to `ref: HEAD` with `ref_kind: branch` told a perfectly online user their
// network was down (internal/engine/outdated.go's describeRemoteErr).
var ErrRemoteRefUnusable = errors.New("recorded ref cannot name a commit")

// ResolveRemoteRef reports the commit fullRef currently points at, without
// fetching any object. `fu outdated` is a read-only command and SPEC §9 forbids
// it from writing anything, so comparing upstream content is out of reach; the
// commit is all it may look at.
//
// fullRef is the fully-qualified form recorded in fu.yaml (refs/heads/main),
// so unlike cloneSource this does not probe branch-then-tag: one advertisement
// carries every ref and the recorded name is looked up directly.
func (s Source) ResolveRemoteRef(ctx context.Context, fullRef string) (string, error) {
	if s.Kind != KindGit {
		return "", fmt.Errorf("resolve remote ref for a %s source: only git sources have a remote", s.Kind)
	}
	if fullRef == "" {
		return "", fmt.Errorf("%w: resolve remote ref for %s: empty ref", ErrRemoteRefUnusable, s.URL)
	}
	// The URL needs the same refusal as the ref, and for a sharper reason: an
	// empty ref fails loudly, while an empty URL succeeds against the wrong
	// repository. go-git's transport.NewEndpoint("") falls into parseFile, which
	// does filepath.Abs("") and yields a file endpoint on the process's working
	// directory, so this function would answer from wherever fu was run rather
	// than from the record. ErrRemoteRefUnusable is the right sentinel: like the
	// empty ref and the symbolic ref below, this is a fact about a hand-edited
	// fu.yaml and not about the network, and describeRemoteErr must not label it
	// "unreachable" (internal/engine/outdated.go).
	//
	// judgeGitUpdate holds the primary guard, one layer up and before its
	// resolve cache; this is defence in depth for any other caller.
	if s.URL == "" {
		return "", fmt.Errorf("%w: resolve remote ref %s: empty url", ErrRemoteRefUnusable, fullRef)
	}
	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: "origin",
		URLs: []string{s.URL},
	})
	refs, err := remote.ListContext(ctx, &git.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list refs of %s: %w", s.URL, err)
	}
	want := plumbing.ReferenceName(fullRef)
	for _, ref := range refs {
		if ref.Name() != want {
			continue
		}
		// A symbolic reference carries no hash of its own -- go-git advertises
		// HEAD that way when the server supports symrefs -- and Hash() then
		// answers ZeroHash. Returning that with a nil error broke this
		// function's stated contract silently: a record with `ref: HEAD` and
		// `ref_kind: branch` (hand-edit only, since EncodeFields never writes
		// HEAD) compared forty zeros against the locked commit and reported the
		// skill updatable forever, with forty zeros in its Latest column. Round
		// 2, Minor #5, confirmed by probe against a real file:// repository.
		if ref.Type() != plumbing.HashReference || ref.Hash().IsZero() {
			return "", fmt.Errorf("%w: resolve %s of %s: the remote advertises it as a symbolic reference with no commit of its own",
				ErrRemoteRefUnusable, fullRef, s.URL)
		}
		return ref.Hash().String(), nil
	}
	return "", fmt.Errorf("%w: %s advertises no %s", ErrRemoteRefNotFound, s.URL, fullRef)
}
