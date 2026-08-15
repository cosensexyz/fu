package engine

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/skill"
	"github.com/cosensexyz/fu/internal/source"
	"github.com/cosensexyz/fu/internal/store"
)

func AddSkill(st *store.Store, agents []agent.Agent, p *source.Prepared, cand Candidate, fields map[string]string) (Result, error) {
	return addSkillDefault(st, agents, p, cand, fields)
}

func AddSkills(st *store.Store, agents []agent.Agent, p *source.Prepared, cands []Candidate, fields func(Candidate) map[string]string) (Result, []string, []string, error) {
	return addSkills(st, agents, p, cands, fields)
}

// makeGitSource builds a bare repository holding one skill and returns its
// file:// URL. withTag additionally tags the commit v1.2.3.
func makeGitSource(t *testing.T, withTag bool) string {
	return makeGitSourceNamed(t, withTag, "pdf-tools")
}

// makeGitSourceNamed is makeGitSource with the skill's directory name.
func makeGitSourceNamed(t *testing.T, withTag bool, skillName string) string {
	t.Helper()
	work := t.TempDir()
	repo, err := git.PlainInit(work, false)
	if err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, work, skillName, "---\nname: "+skillName+"\ndescription: d\n---\n")
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	head, err := wt.Commit("seed", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t"}})
	if err != nil {
		t.Fatal(err)
	}
	if withTag {
		if _, err := repo.CreateTag("v1.2.3", head, &git.CreateTagOptions{
			Tagger:  &object.Signature{Name: "tagger", Email: "tagger@example.com"},
			Message: "annotated release",
		}); err != nil {
			t.Fatal(err)
		}
	}
	bare := t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	remote, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}})
	if err != nil {
		t.Fatal(err)
	}
	headRef, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/" + headRef.Name().Short() + ":refs/heads/" + headRef.Name().Short())},
	}); err != nil {
		t.Fatal(err)
	}
	if withTag {
		if err := remote.Push(&git.PushOptions{
			RefSpecs: []config.RefSpec{config.RefSpec("refs/tags/v1.2.3:refs/tags/v1.2.3")},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return "file://" + bare
}

// writeSkillTree plants a skill directory inside root.
func writeSkillTree(t *testing.T, root, name, frontmatter string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(frontmatter+"\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func prepareLocal(t *testing.T, dir string) *source.Prepared {
	t.Helper()
	src, err := source.ParseArg(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := src.Prepare(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func localFields(t *testing.T, p *source.Prepared, subdir string) map[string]string {
	t.Helper()
	src, err := source.ParseArg(p.Dir())
	if err != nil {
		t.Fatal(err)
	}
	return src.EncodeFields(subdir, p.Lock())
}

func TestAddSkillLocalInstallsAndLinks(t *testing.T) {
	s, _ := setupStore(t)
	srcDir := t.TempDir()
	writeSkillTree(t, srcDir, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	p := prepareLocal(t, srcDir)
	cands, invalid, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Name != "pdf-tools" || cands[0].Subdir != "pdf-tools" {
		t.Fatalf("candidates = %+v, invalid = %v", cands, invalid)
	}
	agentDir := t.TempDir()
	agents := []agent.Agent{fakeAgent{"claude", agentDir}}
	fields := localFields(t, p, cands[0].Subdir)
	if _, err := AddSkill(s, agents, p, cands[0], fields); err != nil {
		t.Fatal(err)
	}
	// Store content is a copy, not a link.
	info, err := os.Lstat(filepath.Join(s.SkillsDir(), "pdf-tools"))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("store content wrong: %v %v", info, err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasSkill("pdf-tools") || !cfg.Enabled("pdf-tools") {
		t.Fatal("config entry missing or not enabled")
	}
	if got := cfg.SourceFields("pdf-tools"); got["type"] != "local" || got["subdir"] != "pdf-tools" || got["path"] == "" {
		t.Fatalf("source fields = %v", got)
	}
	if cfg.Digest("pdf-tools") == "" {
		t.Fatal("digest baseline must be recorded")
	}
	// Agent link materialized, pointing into the store.
	target, err := os.Readlink(filepath.Join(agentDir, "pdf-tools"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(target, s.SkillsDir()) {
		t.Fatalf("link target %q not under store", target)
	}
}

func TestAddSkillRejectsDuplicateName(t *testing.T) {
	s, _ := setupStore(t, "pdf-tools")
	srcDir := t.TempDir()
	writeSkillTree(t, srcDir, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	p := prepareLocal(t, srcDir)
	cands, _, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AddSkill(s, nil, p, cands[0], nil); !errors.Is(err, ErrSkillExists) {
		t.Fatalf("duplicate install must be refused with ErrSkillExists, got %v", err)
	}
}

func TestScanSourceRejectsDuplicateCandidateNames(t *testing.T) {
	srcDir := t.TempDir()
	for _, subdir := range []string{"tools/pdf-tools", "vendor/pdf-tools"} {
		writeSkillTree(t, srcDir, subdir, "---\nname: pdf-tools\ndescription: d\n---\n")
	}
	p := prepareLocal(t, srcDir)
	cands, invalid, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("duplicate name must remove every ambiguous candidate: %+v", cands)
	}
	for _, subdir := range []string{"tools/pdf-tools", "vendor/pdf-tools"} {
		duplicateErr := invalid[subdir]
		if duplicateErr == nil {
			t.Fatalf("duplicate path %q was not reported: %+v", subdir, invalid)
		}
		for _, want := range []string{"pdf-tools", "tools/pdf-tools", "vendor/pdf-tools", "local source", "git source", "remove or rename"} {
			if !strings.Contains(duplicateErr.Error(), want) {
				t.Fatalf("duplicate error %q does not name %q", duplicateErr, want)
			}
		}
	}
}

func TestAddPublishNamesDestinationThatAppeared(t *testing.T) {
	s, _ := setupStore(t)
	srcDir := t.TempDir()
	writeSkillTree(t, srcDir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	p := prepareLocal(t, srcDir)
	cands, _, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(s.SkillsDir(), "alpha")
	h := hooks{beforePublish: func() error {
		return os.WriteFile(destination, []byte("foreign"), 0o644)
	}}
	_, err = addSkill(s, nil, p, cands[0], nil, h)
	if err == nil || !strings.Contains(err.Error(), destination) || !strings.Contains(err.Error(), "appeared") {
		t.Fatalf("publish conflict must name the destination and race, got %v", err)
	}
	if got, readErr := os.ReadFile(destination); readErr != nil || string(got) != "foreign" {
		t.Fatalf("foreign destination must survive, got %q err=%v", got, readErr)
	}
}

func TestAddSkillsBatchSkipsDuplicates(t *testing.T) {
	s, _ := setupStore(t, "alpha")
	srcDir := t.TempDir()
	writeSkillTree(t, srcDir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	writeSkillTree(t, srcDir, "beta", "---\nname: beta\ndescription: d\n---\n")
	p := prepareLocal(t, srcDir)
	cands, _, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}
	fields := func(c Candidate) map[string]string { return localFields(t, p, c.Subdir) }
	_, added, skipped, err := AddSkills(s, nil, p, cands, fields)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(added) != 1 || added[0] != "beta" {
		t.Fatalf("added = %v", added)
	}
	if len(skipped) != 1 || skipped[0] != "alpha" {
		t.Fatalf("skipped = %v", skipped)
	}
}

func TestAddSkillRejectsPathSafetyViolation(t *testing.T) {
	s, _ := setupStore(t)
	srcDir := t.TempDir()
	writeSkillTree(t, srcDir, "evil", "---\nname: evil\ndescription: d\n---\n")
	if err := os.Symlink("../../../../../../etc", filepath.Join(srcDir, "evil", "etc-link")); err != nil {
		t.Fatal(err)
	}
	p := prepareLocal(t, srcDir)
	// ScanSource now refuses this candidate outright (round 18 finding I7), so
	// the candidate is built directly: the install-time path-safety check is
	// the second line of defence and must keep refusing even when a caller
	// hands it a candidate the scan would never have offered.
	proj, err := skill.ProjectDir(p.FS(), "evil")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := skill.DigestManifest(proj)
	if err != nil {
		t.Fatal(err)
	}
	cand := Candidate{Name: "evil", Description: "d", Subdir: "evil", Digest: digest}
	if _, err := AddSkill(s, nil, p, cand, nil); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("an escaping symlink must be refused, got %v", err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasSkill("evil") {
		t.Fatal("refused skill must not be registered")
	}
}

func TestAddSkillRejectsChangedMetaAtInstall(t *testing.T) {
	s, _ := setupStore(t)
	srcDir := t.TempDir()
	writeSkillTree(t, srcDir, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	p := prepareLocal(t, srcDir)
	cands, _, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}
	// The source changes after scanning: the frontmatter name no longer
	// matches the directory. Install must re-validate.
	writeSkillTree(t, srcDir, "pdf-tools", "---\nname: renamed\ndescription: d\n---\n")
	if _, err := AddSkill(s, nil, p, cands[0], nil); err == nil {
		t.Fatal("changed frontmatter must be caught at install time")
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasSkill("pdf-tools") {
		t.Fatal("failed install must not register the skill")
	}
}

func TestAddSkillsReportsCommittedNameOnReconcileFailure(t *testing.T) {
	s, _ := setupStore(t)
	srcDir := t.TempDir()
	writeSkillTree(t, srcDir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	p := prepareLocal(t, srcDir)
	candidates, _, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}
	brokenSkills := filepath.Join(t.TempDir(), "skills")
	if err := os.WriteFile(brokenSkills, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, added, _, err := AddSkills(s, []agent.Agent{fakeAgent{"broken", brokenSkills}}, p, candidates, func(Candidate) map[string]string { return nil })
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("add error = %v, want ErrOperationFailed", err)
	}
	if len(added) != 1 || added[0] != "alpha" {
		t.Fatalf("added = %v, want durable alpha", added)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasSkill("alpha") {
		t.Fatal("alpha must remain durably registered")
	}
}

func TestAddSkillsContinuesAfterReconcileFailure(t *testing.T) {
	s, _ := setupStore(t)
	srcDir := t.TempDir()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		writeSkillTree(t, srcDir, name, "---\nname: "+name+"\ndescription: d\n---\n")
	}
	p := prepareLocal(t, srcDir)
	candidates, _, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}
	brokenSkills := filepath.Join(t.TempDir(), "skills")
	if err := os.WriteFile(brokenSkills, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, added, _, unattempted, _, err := addSkillsDetailed(
		s,
		[]agent.Agent{fakeAgent{"broken", brokenSkills}},
		p,
		candidates,
		func(Candidate) map[string]string { return nil },
		hooks{},
	)
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("add error = %v, want accumulated ErrOperationFailed", err)
	}
	if got := strings.Count(err.Error(), "reconcile links:"); got > 1 {
		t.Fatalf("repeated reconcile failures must collapse at the error boundary, got %d copies in %q", got, err)
	}
	if got, want := strings.Join(added, ","), "alpha,beta,gamma"; got != want {
		t.Fatalf("added = %v, want every independent candidate", added)
	}
	if len(unattempted) != 0 {
		t.Fatalf("unattempted = %v, want none after isolated reconcile failures", unattempted)
	}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if _, statErr := os.Stat(filepath.Join(s.SkillsDir(), name, "SKILL.md")); statErr != nil {
			t.Fatalf("%s was not installed: %v", name, statErr)
		}
	}
}

func TestAddSkillsContinuesAfterIsolatedCandidateFailure(t *testing.T) {
	s, _ := setupStore(t)
	srcDir := t.TempDir()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		writeSkillTree(t, srcDir, name, "---\nname: "+name+"\ndescription: d\n---\n")
	}
	if err := os.MkdirAll(filepath.Join(s.SkillsDir(), "beta"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := prepareLocal(t, srcDir)
	candidates, _, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}

	res, added, _, unattempted, _, err := addSkillsDetailed(
		s, nil, p, candidates, func(Candidate) map[string]string { return nil }, hooks{},
	)
	if !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("isolated candidate failure must make the batch exit non-zero, got %v", err)
	}
	if got, want := strings.Join(added, ","), "alpha,gamma"; got != want {
		t.Fatalf("added = %q, want %q", got, want)
	}
	if len(unattempted) != 0 {
		t.Fatalf("independent candidates must all be attempted, got unattempted %v", unattempted)
	}
	if len(res.Failed) != 1 || res.Failed[0].Action.Skill != "beta" {
		t.Fatalf("failed candidate must retain its reason in the structured result: %+v", res.Failed)
	}
}

func TestAddSkillsStopsBeforeRecoveryCanRollBackReportedCommit(t *testing.T) {
	s, _ := setupStore(t)
	srcDir := t.TempDir()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		writeSkillTree(t, srcDir, name, "---\nname: "+name+"\ndescription: d\n---\n")
	}
	p := prepareLocal(t, srcDir)
	candidates, _, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("interrupted after first commit")
	commits := 0
	res, added, _, unattempted, operations, err := addSkillsDetailed(
		s, nil, p, candidates, func(Candidate) map[string]string { return nil }, hooks{
			afterCommit: func() error {
				commits++
				if commits == 1 {
					return interrupted
				}
				return nil
			},
		},
	)
	if !errors.Is(err, interrupted) {
		t.Fatalf("batch error = %v, want first post-commit failure", err)
	}
	if got, want := strings.Join(added, ","), ""; got != want {
		t.Fatalf("added = %q, want %q", got, want)
	}
	if got, want := strings.Join(unattempted, ","), "beta,gamma"; got != want {
		t.Fatalf("unattempted = %q, want %q", got, want)
	}
	if len(operations) != 1 || !operations[0].RecoveryPending {
		t.Fatalf("batch must stop with the first operation pending recovery: %+v", operations)
	}
	if len(res.Failed) != 1 || res.Failed[0].Action.Skill != "alpha" {
		t.Fatalf("recovery-pending candidate must be reported once as failed: %+v", res.Failed)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasSkill("alpha") || cfg.HasSkill("beta") || cfg.HasSkill("gamma") {
		t.Fatalf("reported commit must remain durable at command exit: %+v", cfg.SkillNames())
	}
}

func TestAddSkillsStopsAfterCommittedCanonicalPathFailure(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "fu-home")
	s, err := store.Init(home)
	if err != nil {
		t.Fatal(err)
	}
	srcDir := t.TempDir()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		writeSkillTree(t, srcDir, name, "---\nname: "+name+"\ndescription: d\n---\n")
	}
	p := prepareLocal(t, srcDir)
	candidates, _, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}
	moved := home + ".moved"
	movedOnce := false
	res, added, _, unattempted, operations, err := addSkillsDetailed(
		s, nil, p, candidates, func(Candidate) map[string]string { return nil }, hooks{
			afterCommit: func() error {
				if movedOnce {
					return nil
				}
				movedOnce = true
				return os.Rename(home, moved)
			},
		},
	)
	if err == nil {
		t.Fatal("replaced canonical home must fail the batch")
	}
	if got, want := strings.Join(added, ","), ""; got != want {
		t.Fatalf("added = %q, want %q", got, want)
	}
	if len(res.Failed) != 1 || res.Failed[0].Action.Skill != "alpha" {
		t.Fatalf("canonical-path failure must report alpha once as failed: %+v", res.Failed)
	}
	if got, want := strings.Join(unattempted, ","), "beta,gamma"; got != want {
		t.Fatalf("unattempted = %q, want %q", got, want)
	}
	if len(operations) != 1 || !operations[0].Committed || operations[0].CanonicalChecked {
		t.Fatalf("canonical failure must stop after one committed attempt: %+v", operations)
	}
}

func TestAddSkillsSingleCandidateFatalNamesCandidate(t *testing.T) {
	s, _ := setupStore(t)
	srcDir := t.TempDir()
	writeSkillTree(t, srcDir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	p := prepareLocal(t, srcDir)
	candidates, _, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("interrupted after commit")
	res, added, _, unattempted, operations, err := addSkillsDetailed(
		s, nil, p, candidates, func(Candidate) map[string]string { return nil }, hooks{
			afterCommit: func() error { return interrupted },
		},
	)
	if !errors.Is(err, interrupted) {
		t.Fatalf("single-candidate error = %v, want %v", err, interrupted)
	}
	if len(added) != 0 || len(unattempted) != 0 {
		t.Fatalf("single fatal classification: added=%v unattempted=%v", added, unattempted)
	}
	if len(res.Failed) != 1 || res.Failed[0].Action.Skill != "alpha" {
		t.Fatalf("single fatal candidate must be named once: %+v", res.Failed)
	}
	if len(operations) != 1 || !operations[0].RecoveryPending {
		t.Fatalf("single fatal operation outcome = %+v", operations)
	}
}

func TestAddSkillsBatchFatalPrecedesOperationFailed(t *testing.T) {
	s, _ := setupStore(t)
	srcDir := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		writeSkillTree(t, srcDir, name, "---\nname: "+name+"\ndescription: d\n---\n")
	}
	p := prepareLocal(t, srcDir)
	candidates, _, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}
	fatal := errors.Join(ErrOperationFailed, ErrConcurrentStoreChange)
	res, added, _, unattempted, operations, err := addSkillsDetailed(
		s, nil, p, candidates, func(Candidate) map[string]string { return nil }, hooks{
			beforePublish: func() error { return fatal },
		},
	)
	if !errors.Is(err, ErrOperationFailed) || !errors.Is(err, ErrConcurrentStoreChange) {
		t.Fatalf("fatal joined error was not preserved: %v", err)
	}
	if len(operations) != 1 || len(added) != 0 || strings.Join(unattempted, ",") != "beta" {
		t.Fatalf("batch-fatal precedence did not stop after alpha: operations=%d added=%v unattempted=%v", len(operations), added, unattempted)
	}
	if len(res.Failed) != 1 || res.Failed[0].Action.Skill != "alpha" {
		t.Fatalf("batch-fatal candidate must be reported once: %+v", res.Failed)
	}
}

func TestAddSkillsStopsOnceOnInitialWALInfrastructureFailure(t *testing.T) {
	s, _ := setupStore(t)
	srcDir := t.TempDir()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		writeSkillTree(t, srcDir, name, "---\nname: "+name+"\ndescription: d\n---\n")
	}
	p := prepareLocal(t, srcDir)
	candidates, _, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.RecoveryDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.RecoveryDir(), 0o700) })

	res, added, skipped, unattempted, operations, err := addSkillsDetailed(
		s, nil, p, candidates, func(Candidate) map[string]string { return nil }, hooks{},
	)
	if err == nil {
		t.Fatal("read-only recovery directory must fail the batch")
	}
	if len(res.Failed) != 1 || res.Failed[0].Action.Skill != "alpha" || len(added) != 0 || len(skipped) != 0 {
		t.Fatalf("WAL infrastructure failure must name the aborting candidate: res=%+v added=%v skipped=%v", res, added, skipped)
	}
	if got, want := strings.Join(unattempted, ","), "beta,gamma"; got != want {
		t.Fatalf("unattempted = %q, want %q", got, want)
	}
	if len(operations) != 1 {
		t.Fatalf("WAL infrastructure failure must be attempted once, got %d operations", len(operations))
	}
}

func TestAddSkillsStopsOnceOnStoreSetupFailure(t *testing.T) {
	s, _ := setupStore(t)
	srcDir := t.TempDir()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		writeSkillTree(t, srcDir, name, "---\nname: "+name+"\ndescription: d\n---\n")
	}
	p := prepareLocal(t, srcDir)
	candidates, _, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteTxn(s, &TxnRecord{Op: "future-op", Stage: "started"}); err != nil {
		t.Fatal(err)
	}
	res, added, skipped, unattempted, operations, err := addSkillsDetailed(
		s, nil, p, candidates, func(Candidate) map[string]string { return nil }, hooks{},
	)
	if !errors.Is(err, ErrUnknownTxn) {
		t.Fatalf("batch error = %v, want ErrUnknownTxn", err)
	}
	if len(res.Failed) != 1 || res.Failed[0].Action.Skill != "alpha" || len(added) != 0 || len(skipped) != 0 {
		t.Fatalf("store failure must name the aborting candidate: res=%+v added=%v skipped=%v", res, added, skipped)
	}
	if got, want := strings.Join(unattempted, ","), "beta,gamma"; got != want {
		t.Fatalf("unattempted = %q, want %q", got, want)
	}
	if len(operations) != 1 {
		t.Fatalf("store failure must be attempted once, got %d operations", len(operations))
	}
}

func TestAddSkillsPreservesEarlierReconcileFailureWhenLaterCandidateFails(t *testing.T) {
	s, _ := setupStore(t)
	srcDir := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		writeSkillTree(t, srcDir, name, "---\nname: "+name+"\ndescription: d\n---\n")
	}
	p := prepareLocal(t, srcDir)
	candidates, _, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}
	brokenSkills := filepath.Join(t.TempDir(), "skills")
	if err := os.WriteFile(brokenSkills, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	commits := 0
	laterFailure := fmt.Errorf("%w: later candidate", ErrConcurrentStoreChange)
	_, _, _, _, _, err = addSkillsDetailed(
		s, []agent.Agent{fakeAgent{"broken", brokenSkills}}, p, candidates,
		func(Candidate) map[string]string { return nil }, hooks{
			beforePublish: func() error {
				commits++
				if commits == 2 {
					return laterFailure
				}
				return nil
			},
		},
	)
	if !errors.Is(err, ErrOperationFailed) || !errors.Is(err, ErrConcurrentStoreChange) {
		t.Fatalf("later batch-fatal failure must retain both verdicts, got %v", err)
	}
}

func TestAddTrackedOutcomeReportsPostCommitPhases(t *testing.T) {
	s, _ := setupStore(t)
	srcDir := t.TempDir()
	writeSkillTree(t, srcDir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	p := prepareLocal(t, srcDir)
	candidates, _, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("interrupted after commit")
	var outcome OperationOutcome

	_, err = addSkillTracked(s, nil, p, candidates[0], nil, hooks{
		afterCommit: func() error { return interrupted },
	}, &outcome)
	if !errors.Is(err, interrupted) {
		t.Fatalf("add error = %v, want %v", err, interrupted)
	}
	if !outcome.Committed || !outcome.RecoveryPending || outcome.PostCommitComplete || outcome.WALComplete || outcome.CanonicalChecked {
		t.Fatalf("add outcome must stop after durable commit: %+v", outcome)
	}
}

// TestAddSkillGitSourceEndToEnd installs from a git URL (branch and tag),
// asserting the locked source record lands in fu.yaml with the resolved
// commit, ref form, and subdir.
func TestAddSkillGitSourceEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ref      string
		wantKind string
	}{
		{"branch", "", "branch"},
		{"tag", "v1.2.3", "tag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := makeGitSource(t, tc.ref != "")
			src, err := source.ParseArgWithRef(url, tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			p, err := src.Prepare(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			cands, _, err := ScanSource(p)
			if err != nil {
				t.Fatal(err)
			}
			if len(cands) != 1 || cands[0].Name != "pdf-tools" {
				t.Fatalf("candidates = %+v", cands)
			}

			storeDir := t.TempDir()
			s, err := store.Init(storeDir)
			if err != nil {
				t.Fatal(err)
			}
			lock := p.Lock()
			bareRepo, err := git.PlainOpen(strings.TrimPrefix(url, "file://"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := bareRepo.CommitObject(plumbing.NewHash(lock.Commit)); err != nil {
				t.Fatalf("recorded lock %q is not a commit object: %v", lock.Commit, err)
			}
			fields := src.EncodeFields(cands[0].Subdir, lock)
			if _, err := AddSkill(s, nil, p, cands[0], fields); err != nil {
				t.Fatal(err)
			}
			cfg, err := store.LoadConfig(s.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			got := cfg.SourceFields("pdf-tools")
			if got["type"] != "git" || got["url"] != url {
				t.Fatalf("source fields = %v", got)
			}
			if got["ref_kind"] != tc.wantKind || got["commit"] != lock.Commit {
				t.Fatalf("lock fields = %v (want kind %s commit %s)", got, tc.wantKind, lock.Commit)
			}
			if got["subdir"] != "pdf-tools" {
				t.Fatalf("subdir = %q", got["subdir"])
			}
		})
	}
}

// TestScanSourceRootLevelSkill pins that a skill living at the source root
// itself is a candidate: the source root's directory name is the caller's
// artifact (a clone checkout dir, a fu add argument), not a skill-directory
// name, so the frontmatter name-match rule does not apply to it.
func TestScanSourceRootLevelSkill(t *testing.T) {
	srcDir := t.TempDir()
	writeSkillTree(t, srcDir, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	p := prepareLocal(t, srcDir)
	// The scan root is the skills collection; the root-level candidate is
	// the collection itself.
	src := filepath.Join(srcDir, "pdf-tools")
	root, err := source.ParseArg(src)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := root.Prepare(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p2.Close() })
	cands, invalid, err := ScanSource(p2)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Name != "pdf-tools" || cands[0].Subdir != "." {
		t.Fatalf("root-level skill must be a candidate, got %+v invalid=%v", cands, invalid)
	}
	_ = p
}

func TestScanSourceRootSkillStillAppliesEveryMetadataValidation(t *testing.T) {
	cases := []struct {
		name, declaredName, description string
	}{
		{"empty name", "", "d"},
		{"name over 64 characters", strings.Repeat("a", 65), "d"},
		{"uppercase name", "Alpha", "d"},
		{"leading hyphen", "-alpha", "d"},
		{"trailing hyphen", "alpha-", "d"},
		{"consecutive hyphens", "alpha--beta", "d"},
		{"empty description", "alpha", ""},
		{"description over 1024 characters", "alpha", strings.Repeat("d", 1025)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			content := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n", tc.declaredName, tc.description)
			if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			candidates, invalid, err := ScanSource(prepareLocal(t, root))
			if err != nil {
				t.Fatal(err)
			}
			if len(candidates) != 0 {
				t.Fatalf("invalid root skill must not be offered: %+v", candidates)
			}
			if _, ok := invalid["."]; !ok {
				t.Fatalf("root skill violation must be reported at dot, got %v", invalid)
			}
		})
	}
}

// TestAddSkillRecoversAfterProcessInterruption crashes an add at each
// durable boundary in a child process and asserts that the next add retry
// recovers the WAL, converges, and never sweeps the interrupted state in as
// an external modification. Every stage runs against both a local and a git
// source: the git record carries a multi-field source map (review finding
// C1), which is exactly the shape whose reconstruction used to diverge
// byte-wise from the saved config.
func TestAddSkillRecoversAfterProcessInterruption(t *testing.T) {
	if os.Getenv("FU_TEST_CRASH_ADD_HELPER") == "1" {
		home := os.Getenv("FU_TEST_CRASH_ADD_HOME")
		src := os.Getenv("FU_TEST_CRASH_ADD_SRC")
		url := os.Getenv("FU_TEST_CRASH_ADD_URL")
		stage := os.Getenv("FU_TEST_CRASH_ADD_STAGE")
		s, err := store.Open(home)
		if err != nil {
			panic(err)
		}
		crash := func() error { os.Exit(86); return nil }
		var p *source.Prepared
		var fields map[string]string
		if url != "" {
			src2, err := source.ParseArg(url)
			if err != nil {
				panic(err)
			}
			p, err = src2.Prepare(t.TempDir())
			if err != nil {
				panic(err)
			}
			t.Cleanup(func() { _ = p.Close() })
		} else {
			p = prepareLocal(t, src)
		}
		cands, _, err := ScanSource(p)
		if err != nil {
			panic(err)
		}
		if url != "" {
			src2, err := source.ParseArg(url)
			if err != nil {
				panic(err)
			}
			fields = src2.EncodeFields(cands[0].Subdir, p.Lock())
		} else {
			fields = localFields(t, p, cands[0].Subdir)
		}
		var h hooks
		switch stage {
		case "after-txn-start":
			h.afterTxnStart = crash
		case "after-staging-create":
			h.afterStagingCreate = crash
		case "after-declared-txn":
			// Crash with the declaration recorded but the copy unfinished:
			// plant one fully-written declared file so recovery must settle a
			// partial tree (a crash mid-copy leaves exactly this shape).
			// The hook runs inside the write session with the lock held, so
			// the pathname write cannot race another fu process.
			h.afterDeclaredTxn = func() error {
				content, err := os.ReadFile(filepath.Join(p.Dir(), cands[0].Subdir, "SKILL.md"))
				if err != nil {
					panic(err)
				}
				if err := os.WriteFile(filepath.Join(home, "staging", "alpha", "SKILL.md"), content, 0o644); err != nil {
					panic(err)
				}
				return crash()
			}
		case "after-copy":
			h.afterCopy = crash
		case "after-save":
			h.afterSave = crash
		case "after-publish":
			h.afterPublish = crash
		case "after-commit":
			h.afterCommit = crash
		default:
			panic("unknown crash stage " + stage)
		}
		_, _ = addSkill(s, nil, p, cands[0], fields, h)
		panic("crash hook did not run")
	}

	for _, stage := range []string{"after-txn-start", "after-staging-create", "after-declared-txn", "after-copy", "after-save", "after-publish", "after-commit"} {
		for _, kind := range []string{"local", "git"} {
			t.Run(stage+"/"+kind, func(t *testing.T) {
				home := filepath.Join(t.TempDir(), "home")
				if _, err := store.Init(home); err != nil {
					t.Fatal(err)
				}
				var env []string
				if kind == "git" {
					env = append(env, "FU_TEST_CRASH_ADD_URL="+makeGitSourceNamed(t, false, "alpha"))
				} else {
					srcDir := t.TempDir()
					writeSkillTree(t, srcDir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
					env = append(env, "FU_TEST_CRASH_ADD_SRC="+srcDir)
				}
				cmd := exec.Command(os.Args[0], "-test.run=^TestAddSkillRecoversAfterProcessInterruption$")
				cmd.Env = append([]string{}, os.Environ()...)
				cmd.Env = append(cmd.Env,
					"FU_TEST_CRASH_ADD_HELPER=1",
					"FU_TEST_CRASH_ADD_HOME="+home,
					"FU_TEST_CRASH_ADD_STAGE="+stage,
				)
				cmd.Env = append(cmd.Env, env...)
				output, err := cmd.CombinedOutput()
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
					t.Fatalf("child must terminate at %s with code 86, err=%v output=%s", stage, err, output)
				}

				s, err := store.Open(home)
				if err != nil {
					t.Fatal(err)
				}
				var p *source.Prepared
				var fields map[string]string
				if kind == "git" {
					src2, err := source.ParseArg(env[0][len("FU_TEST_CRASH_ADD_URL="):])
					if err != nil {
						t.Fatal(err)
					}
					p, err = src2.Prepare(t.TempDir())
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = p.Close() })
				} else {
					p = prepareLocal(t, env[0][len("FU_TEST_CRASH_ADD_SRC="):])
				}
				cands, _, err := ScanSource(p)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "git" {
					src2, err := source.ParseArg(env[0][len("FU_TEST_CRASH_ADD_URL="):])
					if err != nil {
						t.Fatal(err)
					}
					fields = src2.EncodeFields(cands[0].Subdir, p.Lock())
				} else {
					fields = localFields(t, p, cands[0].Subdir)
				}
				if _, err := AddSkill(s, nil, p, cands[0], fields); err != nil {
					t.Fatalf("retry after %s must recover and succeed: %v", stage, err)
				}
				cfg, err := store.LoadConfig(s.ConfigPath())
				if err != nil {
					t.Fatal(err)
				}
				if !cfg.HasSkill("alpha") {
					t.Fatal("successful retry must register alpha")
				}
				pending, err := PendingTxns(s)
				if err != nil {
					t.Fatal(err)
				}
				if len(pending) != 0 {
					t.Fatalf("successful recovery must clear its WAL, got %+v", pending)
				}
				entries, err := s.Log(10)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if entry.Message == "external: manual modifications" {
						t.Fatalf("interrupted AddSkill state must not be swept as external work: %+v", entries)
					}
				}
			})
		}
	}
}

// TestAddNestedSkillRecoversAfterDeclaredTxnCrash crashes an add of a
// *nested* skill immediately after the declaration revision, with nothing
// created: the first and most likely crash point of the copy phase. The
// settle path must drop the entries whose parents never materialized
// instead of refusing with ErrTxnConflict (round 4 finding I1), so the next
// add recovers and installs.
func TestAddNestedSkillRecoversAfterDeclaredTxnCrash(t *testing.T) {
	if os.Getenv("FU_TEST_CRASH_ADD_NESTED_HELPER") == "1" {
		home := os.Getenv("FU_TEST_CRASH_ADD_NESTED_HOME")
		src := os.Getenv("FU_TEST_CRASH_ADD_NESTED_SRC")
		s, err := store.Open(home)
		if err != nil {
			panic(err)
		}
		crash := func() error { os.Exit(86); return nil }
		h := hooks{afterDeclaredTxn: crash}
		p := prepareLocal(t, src)
		cands, _, err := ScanSource(p)
		if err != nil {
			panic(err)
		}
		_, _ = addSkill(s, nil, p, cands[0], localFields(t, p, cands[0].Subdir), h)
		panic("crash hook did not run")
	}

	home := filepath.Join(t.TempDir(), "home")
	if _, err := store.Init(home); err != nil {
		t.Fatal(err)
	}
	srcDir := t.TempDir()
	writeSkillTree(t, srcDir, "alpha", "---\nname: alpha\ndescription: d\n---\n")
	if err := os.MkdirAll(filepath.Join(srcDir, "alpha", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "alpha", "sub", "file.md"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAddNestedSkillRecoversAfterDeclaredTxnCrash$")
	cmd.Env = append([]string{}, os.Environ()...)
	cmd.Env = append(cmd.Env,
		"FU_TEST_CRASH_ADD_NESTED_HELPER=1",
		"FU_TEST_CRASH_ADD_NESTED_HOME="+home,
		"FU_TEST_CRASH_ADD_NESTED_SRC="+srcDir,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("child must terminate with code 86, err=%v output=%s", err, output)
	}

	s, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	p := prepareLocal(t, srcDir)
	cands, _, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AddSkill(s, nil, p, cands[0], localFields(t, p, cands[0].Subdir)); err != nil {
		t.Fatalf("retry after nested declared crash must recover and succeed: %v", err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasSkill("alpha") {
		t.Fatal("successful retry must register alpha")
	}
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("recovery must clear its WAL, got %+v", pending)
	}
}

// TestScanSourceRejectsPathSafetyViolation pins round 18 finding I7. SPEC rule
// 7 lists SKILL.md presence, name/description well-formedness, the name/dirname
// match and the no-out-of-bounds-reference check as one validation, applied at
// add and adopt. ScanSource ran the first three and skipped the fourth, even
// though it already computes the projection that check consumes -- so an
// escaping skill was offered to the user as installable and only refused
// mid-batch, after earlier skills had committed and with the remaining ones
// never attempted.
func TestScanSourceRejectsPathSafetyViolation(t *testing.T) {
	srcDir := t.TempDir()
	writeSkillTree(t, srcDir, "evil", "---\nname: evil\ndescription: d\n---\n")
	if err := os.Symlink("../../../../../../etc", filepath.Join(srcDir, "evil", "etc-link")); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, srcDir, "good", "---\nname: good\ndescription: d\n---\n")
	p := prepareLocal(t, srcDir)

	cands, invalid, err := ScanSource(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if c.Name == "evil" {
			t.Fatalf("an escaping skill must never be offered as installable: %+v", cands)
		}
	}
	found := false
	for path, reason := range invalid {
		if strings.HasSuffix(path, "evil") {
			found = true
			if !strings.Contains(reason.Error(), "escapes") {
				t.Fatalf("invalid reason must name the escape: %v", reason)
			}
		}
	}
	if !found {
		t.Fatalf("the escaping skill must be reported as invalid, got %v", invalid)
	}
	if len(cands) != 1 || cands[0].Name != "good" {
		t.Fatalf("the valid skill must still be offered: %+v", cands)
	}
}
