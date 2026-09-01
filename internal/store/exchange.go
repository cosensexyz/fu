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
// Both manifests are validated first. A manifest proves what an object is,
// so validating both is what distinguishes fu's own two trees from anything
// an outside writer substituted for either. That check is point-in-time: a
// same-UID writer could still replace content at either name between the
// second validation and renameExchange's own pathname re-resolution, and
// this function cannot close that window without ceasing to be one call.
// Nothing downstream trusts the window held, though -- update's
// crash-recovery rollback undoes a completed exchange by calling this same
// function again with staged and published swapped, so content corrupted
// during the window is caught by that call's own validation rather than
// compounded by it.
//
// The exchange itself never creates: RENAME_SWAP/RENAME_EXCHANGE require
// both names to already exist and fail if either is missing. That is a
// property of the underlying syscall, not something observable through this
// function's own callers, since a vanished side already fails validation
// below before renameExchange is ever reached.
func (s *Store) ExchangeStagedWithSkillOwned(name string, staged, published OwnedTree) error {
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
	if err := s.ValidateSkillOwned(name, published); err != nil {
		return fmt.Errorf("validate the published skill %q before exchanging it: %w", name, err)
	}
	if err := s.ValidateStagedOwned(name, staged); err != nil {
		return fmt.Errorf("validate the staged replacement for %q before exchanging it: %w", name, err)
	}
	skillsFD := int(s.writeRoots.skills.dir.Fd())
	stagingFD := int(s.writeRoots.staging.dir.Fd())
	if err := renameExchange(skillsFD, name, stagingFD, name); err != nil {
		return fmt.Errorf("exchange staged and published %q: %w", name, err)
	}
	return nil
}
