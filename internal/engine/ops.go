// internal/engine/ops.go
package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"fu/internal/agent"
	"fu/internal/skill"
	"fu/internal/store"
)

const skillTemplate = `---
name: %[1]s
description: Describe what this skill does and when to use it.
---

# %[1]s

Write the instructions for this skill here.
`

func digestOwnedPayload(payload store.OwnedTree) (string, error) {
	entries := make([]skill.ManifestEntry, 0, len(payload.Entries))
	for _, entry := range payload.Entries {
		entries = append(entries, skill.ManifestEntry{
			Path:   entry.Path,
			Mode:   fs.FileMode(entry.Mode),
			Digest: entry.Digest,
			Target: entry.Target,
		})
	}
	return skill.DigestManifest(entries)
}

// NewSkill scaffolds a skill inside the store, enabled everywhere by
// default (scenario 7). Content editing happens in external editors;
// later edits are swept into history by the next write command.
func NewSkill(st *store.Store, agents []agent.Agent, name string) (Result, error) {
	return newSkill(st, agents, name, hooks{})
}

// newSkill carries NewSkill's implementation plus the pipeline's test-only
// hooks (see run), so a test can inject the failures that happen at each
// durable boundary and assert what the store is left holding.
func newSkill(st *store.Store, agents []agent.Agent, name string, h hooks) (Result, error) {
	if err := skill.ValidateName(name); err != nil {
		return Result{}, err
	}
	txn := &TxnRecord{
		Op:      "new",
		Name:    name,
		Targets: []string{filepath.Join("staging", name), filepath.Join("store", "skills", name)},
	}
	return run(st, agents, Op{
		Message:        "new: " + name,
		Txn:            txn,
		AllowedChanges: []string{"fu.yaml", filepath.ToSlash(filepath.Join("skills", name))},
		ValidatePrepared: func(st *store.Store, prepared store.PreparedCommit) error {
			if txn.Payload == nil {
				return errors.New("new transaction has no payload manifest at commit preparation")
			}
			if err := st.ValidateSkillOwned(name, *txn.Payload); err != nil {
				return fmt.Errorf("validate published skill before commit: %w", err)
			}
			return st.ValidatePreparedOwnedTree(prepared, filepath.ToSlash(filepath.Join("skills", name)), *txn.Payload)
		},
		Preflight: func(st *store.Store, cfg *store.Config) error {
			return checkNewSkillAvailable(st, cfg, name)
		},
		Mutate: func(st *store.Store, cfg *store.Config) error {
			// Repeat the preflight immediately before mutation. The write lock
			// excludes other fu processes, while this second check protects
			// against an external writer racing the read-only preflight.
			if err := checkNewSkillAvailable(st, cfg, name); err != nil {
				return err
			}
			// Staging and skills are separate descriptors pinned by BeginWrite.
			// Validating a directory and then addressing it by pathname is not
			// validation: either logical name can be replaced after Store.Open,
			// so every operation below stays relative to the descriptor whose
			// identity was checked at session start.
			staged := name
			// Built in staging and published by one rename (DESIGN §6, round
			// 7 finding). Writing straight into the store meant a failure at
			// any later step -- the config save, the commit, a crash -- left
			// content the config did not know about, in a build shipping no
			// `log`, `revert` or `restore` to clear it: the next write swept
			// the residue in as an "external modification" while a retry of
			// the same command refused outright at the guard above, so the
			// operation could neither finish nor be repeated.
			//
			// staging is a sibling of the repository under the same $FU_HOME
			// (store.StagingDir), so this is a same-filesystem rename: it
			// either happened or it did not. Everything before it is
			// invisible to the store, and the residue it can leave behind is
			// in staging, which is outside version control and may be removed
			// only by recovery carrying the matching ownership manifest. The
			// preflight refuses an unmatched same-name entry; the exclusive
			// create below closes the remaining window without replacing or
			// deleting it.
			//
			// Creation and the first manifest are one store operation because
			// splitting them is what let a racer supply the root: see
			// CreateStagedRootOwned for why an ordinary Mkdir followed by a
			// snapshot of the same pathname proves nothing about which
			// directory was enumerated.
			rootPayload, err := st.CreateStagedRootOwned(staged, 0o755)
			if err != nil {
				return fmt.Errorf("create staging area %s exclusively: %w", filepath.Join(st.StagingDir(), name), err)
			}
			if err := h.fire(h.afterStagingCreate); err != nil {
				return err
			}
			// The scaffold is declared in the same revision that records the
			// root, so the window between creating it and recording it is one
			// recovery can classify: the file is either absent, or present with
			// exactly this mode and content. Without the declaration a crash in
			// that window left fu's own file looking like foreign interference,
			// and every later write command refused.
			content := fmt.Sprintf(skillTemplate, name)
			txn.Payload = &rootPayload
			txn.Declared = []store.DeclaredEntry{store.NewDeclaredFile("SKILL.md", 0o644, []byte(content))}
			if err := WriteTxn(st, txn); err != nil {
				return fmt.Errorf("record staging-root ownership: %w", err)
			}
			if err := h.fire(h.afterStagingOwnership); err != nil {
				return err
			}
			entry, err := st.CreateStagedFileOwned(staged, "SKILL.md", []byte(content), 0o644, rootPayload)
			if err != nil {
				return fmt.Errorf("create staged scaffold exclusively: %w", err)
			}
			if err := h.fire(h.afterStagingScaffold); err != nil {
				return err
			}
			payload := rootPayload
			payload.Entries = append([]store.OwnedTreeEntry(nil), rootPayload.Entries...)
			payload.Entries = append(payload.Entries, entry)
			if err := payload.Validate(); err != nil {
				return fmt.Errorf("build staged skill ownership: %w", err)
			}
			txn.Payload = &payload
			txn.Declared = nil
			if err := WriteTxn(st, txn); err != nil {
				return fmt.Errorf("record staged skill ownership: %w", err)
			}
			if err := st.ValidateStagedOwned(staged, payload); err != nil {
				return fmt.Errorf("validate exact staged skill: %w", err)
			}
			// The baseline is derived from the authoritative manifest, not a
			// second live-tree enumeration that could absorb a concurrent child.
			d, err := digestOwnedPayload(payload)
			if err != nil {
				return fmt.Errorf("digest staged skill ownership: %w", err)
			}
			txn.Digest = d
			txn.Stage = "prepared"
			if err := WriteTxn(st, txn); err != nil {
				return fmt.Errorf("record prepared skill: %w", err)
			}
			return cfg.AddSkill(name, d)
		},
		Publish: func(st *store.Store) error {
			// Anchored the same way as Mutate, and for the same reason: the
			// rename must land inside the store that was validated, not
			// wherever the pathname happens to point by the time it runs.
			skillsRoot, err := st.SkillsRoot()
			if err != nil {
				return fmt.Errorf("use checked skills root: %w", err)
			}
			published := name
			// Re-checked immediately before the rename for an early diagnostic.
			// The platform no-replace rename remains the authority and closes
			// the namespace race with writers outside fu's process lock.
			if _, err := skillsRoot.Lstat(published); err == nil {
				return fmt.Errorf("%s appeared while %q was being created; refusing to replace it",
					filepath.Join(st.SkillsDir(), name), name)
			} else if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			// One same-filesystem rename: staging is a sibling of the
			// repository under $FU_HOME, so this either happened or it did
			// not. See Op.Publish for why it runs here rather than inside
			// Mutate.
			if txn.Payload == nil {
				return errors.New("new transaction has no staged ownership manifest")
			}
			return st.PublishStagedOwned(name, *txn.Payload)
		},
	}, h)
}

func checkNewSkillAvailable(st *store.Store, cfg *store.Config, name string) error {
	if cfg.HasSkill(name) {
		return fmt.Errorf("skill %q already exists", name)
	}
	skillsRoot, err := st.SkillsRoot()
	if err != nil {
		return fmt.Errorf("use checked skills root: %w", err)
	}
	if _, err := skillsRoot.Lstat(name); err == nil {
		return fmt.Errorf("store already holds content at %s (not registered in fu.yaml); move it aside or remove it before running `fu new %s` again", filepath.Join(st.SkillsDir(), name), name)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("check existing store content at %s: %w", filepath.Join(st.SkillsDir(), name), err)
	}
	stagingRoot, err := st.StagingRoot()
	if err != nil {
		return fmt.Errorf("use checked staging root: %w", err)
	}
	if _, err := stagingRoot.Lstat(name); err == nil {
		return fmt.Errorf("staging already holds unmatched content at %s; move it aside or remove it before running `fu new %s` again", filepath.Join(st.StagingDir(), name), name)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("check existing staging content at %s: %w", filepath.Join(st.StagingDir(), name), err)
	}
	return nil
}

// SetGlobal flips a skill's global switch (the default all agents
// follow, SPEC §4.1).
func SetGlobal(st *store.Store, agents []agent.Agent, name string, on bool) (Result, error) {
	verb := "disable"
	if on {
		verb = "enable"
	}
	return Run(st, agents, Op{
		Message:        fmt.Sprintf("%s: %s", verb, name),
		AllowedChanges: []string{"fu.yaml"},
		Mutate: func(_ *store.Store, cfg *store.Config) error {
			if !cfg.HasSkill(name) {
				return fmt.Errorf("unknown skill %q", name)
			}
			cfg.SetEnabled(name, on)
			return nil
		},
	})
}

// SetAgentSwitch sets one agent's switch; a value equal to global is
// normalized away (SPEC §4.1 same-value normalization).
func SetAgentSwitch(st *store.Store, agents []agent.Agent, name, agentName string, on bool) (Result, error) {
	if _, ok := agent.ByName(agentName); !ok {
		return Result{}, fmt.Errorf("unknown agent %q", agentName)
	}
	verb := "disable"
	if on {
		verb = "enable"
	}
	return Run(st, agents, Op{
		Message:        fmt.Sprintf("%s: %s --agent %s", verb, name, agentName),
		AllowedChanges: []string{"fu.yaml"},
		Mutate: func(_ *store.Store, cfg *store.Config) error {
			if !cfg.HasSkill(name) {
				return fmt.Errorf("unknown skill %q", name)
			}
			cfg.SetAgent(name, agentName, on)
			return nil
		},
	})
}
