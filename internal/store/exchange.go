package store

import (
	"errors"
	"fmt"
)

// ExchangeStagedWithSkillOwned replaces a published skill with a staged tree in
// one atomic step, leaving the replaced tree at the staging name.
//
// This is what `fu update` needs and no other operation does: add and new
// publish into a free name, rm empties one, and adopt moves one in. Only
// update replaces a name that every agent's symlink is already pointing at,
// so a two-step retire-then-publish would leave a window in which the skill
// is simply absent -- a window an agent starting a session would observe as
// a broken link. One exchange has no such window: the name resolves to a
// complete skill before and after, and never to anything in between.
//
// Both manifests admit the inputs before the exchange. The resulting live
// observations then prove each object under its destination name. A failed
// post-exchange proof retains a conflict and leaves both objects in place for
// recovery; it never swaps unknown replacements back or reports success.
//
// The exchange itself never creates: RENAME_SWAP/RENAME_EXCHANGE require
// both names to already exist and fail if either is missing. That is a
// property of the underlying syscall, not something observable through this
// function's own callers, since a vanished side already fails validation
// below before renameExchange is ever reached.
func (s *Store) ExchangeStagedWithSkillOwned(name string, staged, published OwnedTree) error {
	return s.exchangeStagedWithSkillOwnedWithOps(name, staged, published, snapshotOwnedTree, renameExchange)
}

func (s *Store) exchangeStagedWithSkillOwnedWithOps(name string, staged, published OwnedTree, snapshot ownedTreeSnapshotFunc, exchange func(int, string, int, string) error) error {
	defer keepDescriptorOwnersAlive(s)
	// Both roots are descriptors pinned at BeginWrite, checked first so every
	// later step in this function runs against the directories whose identity
	// was validated at session start -- not wherever the pathnames happen to
	// resolve by now.
	if s.writeRoots == nil || s.writeRoots.skills == nil || s.writeRoots.skills.dir == nil ||
		s.writeRoots.staging == nil || s.writeRoots.staging.dir == nil {
		return errors.New("store is not attached to checked skills and staging roots")
	}
	if !validPublicLogicalEntry(name) {
		return fmt.Errorf("exchange staged and published tree requires a public single-component name outside the .fu- namespace: %q", name)
	}
	observe := func(root *checkedRoot, expected OwnedTree) (OwnedTree, error) {
		if err := validateTransactionOwnedTreeManifest(expected); err != nil {
			return OwnedTree{}, err
		}
		actual, err := snapshot(root, name)
		if err != nil {
			return OwnedTree{}, classifyOwnedSnapshotError("exchange tree", name, err)
		}
		if err := compareOwnedTreeExact(actual, expected); err != nil {
			return OwnedTree{}, err
		}
		return actual, nil
	}
	observedPublished, err := observe(s.writeRoots.skills, published)
	if err != nil {
		return fmt.Errorf("validate the published skill %q before exchanging it: %w", name, err)
	}
	observedStaged, err := observe(s.writeRoots.staging, staged)
	if err != nil {
		return fmt.Errorf("validate the staged replacement for %q before exchanging it: %w", name, err)
	}
	skillsFD := int(s.writeRoots.skills.dir.Fd())
	stagingFD := int(s.writeRoots.staging.dir.Fd())
	if err := exchange(skillsFD, name, stagingFD, name); err != nil {
		return fmt.Errorf("exchange staged and published %q: %w", name, err)
	}
	// The records only admitted the inputs. Prove both moved objects against
	// the observations captured before this exchange, including live handles.
	// On failure leave both names in place for the retained WAL to describe;
	// another exchange would move an object whose ownership is now unknown.
	_, skillsErr := observe(s.writeRoots.skills, observedStaged)
	_, stagingErr := observe(s.writeRoots.staging, observedPublished)
	return errors.Join(
		classifyPostRenameOwnedSnapshotError("exchanged skill", name, skillsErr),
		classifyPostRenameOwnedSnapshotError("exchanged staging tree", name, stagingErr),
	)
}
