package engine

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/agent"
	"github.com/cosensexyz/fu/internal/store"
)

// wholeDirFixture builds a HOME whose agent skills directory is itself a
// symlink into a target directory holding one skill plus a non-skill file.
func wholeDirFixture(t *testing.T) (fuHome, homeDir, target string) {
	t.Helper()
	fuHome = t.TempDir()
	if _, err := store.Init(fuHome); err != nil {
		t.Fatal(err)
	}
	homeDir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(homeDir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	target = t.TempDir()
	writeSkillTree(t, target, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	if err := os.WriteFile(filepath.Join(target, "notes.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(homeDir, ".claude", "skills")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", homeDir)
	return fuHome, homeDir, target
}

func TestDescribeDirSwitchDiffNamesAddedRemovedAndChangedEntries(t *testing.T) {
	want := []DirSwitchEntry{
		{Name: "changed", Mode: uint32(os.ModeDir)},
		{Name: "removed", Mode: 0},
	}
	got := []DirSwitchEntry{
		{Name: "added", Mode: 0},
		{Name: "changed", Mode: 0},
	}

	description := describeDirSwitchDiff(want, got)
	for _, phrase := range []string{"changed type changed", "removed removed", "added added"} {
		if !strings.Contains(description, phrase) {
			t.Fatalf("diff %q must contain %q", description, phrase)
		}
	}
}

func TestRestoreUnexpectedDirSwitchMoveRefusesOccupiedOriginal(t *testing.T) {
	parentPath := t.TempDir()
	for _, name := range []string{"moved", "original"} {
		if err := os.Mkdir(filepath.Join(parentPath, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	parent, err := os.Open(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()

	err = restoreUnexpectedDirSwitchMove(parent, "moved", "original")
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("occupied restore destination must return ErrTxnConflict, got %v", err)
	}
	for _, name := range []string{"moved", "original"} {
		if _, statErr := os.Stat(filepath.Join(parentPath, name)); statErr != nil {
			t.Fatalf("%s must remain untouched: %v", name, statErr)
		}
	}
}

func TestRestoreUnexpectedDirSwitchMoveJoinsRestoreFailure(t *testing.T) {
	parentPath := t.TempDir()
	for _, name := range []string{"moved", "original"} {
		if err := os.Mkdir(filepath.Join(parentPath, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	parent, err := os.Open(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	validationErr := errors.New("replacement validation failed")
	err = restoreUnexpectedDirSwitchMoveAfterError(parent, "moved", "original", validationErr)
	if !errors.Is(err, validationErr) || !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("joined error = %v, want validation and restore failures", err)
	}
}

func TestDirSwitchResumeDoesNotRestoreArtifactsItDidNotRetire(t *testing.T) {
	t.Run("replacement root", func(t *testing.T) {
		parentPath := t.TempDir()
		name := ".fu-skills-owned"
		sw := &DirSwitchState{Sibling: filepath.Join(parentPath, name), CleanupID: "0011223344556677"}
		retired := dirSwitchRetiredName(sw, "root", name)
		if err := os.Mkdir(filepath.Join(parentPath, retired), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("expected", filepath.Join(parentPath, retired, "alpha")); err != nil {
			t.Fatal(err)
		}
		parent, err := os.Open(parentPath)
		if err != nil {
			t.Fatal(err)
		}
		defer parent.Close()
		rootStat, err := statAdoptEntry(int(parent.Fd()), retired)
		if err != nil {
			t.Fatal(err)
		}
		child, err := os.Open(filepath.Join(parentPath, retired))
		if err != nil {
			t.Fatal(err)
		}
		childStat, err := statAdoptEntry(int(child.Fd()), "alpha")
		_ = child.Close()
		if err != nil {
			t.Fatal(err)
		}
		sw.SiblingIdentity = adoptIdentity(&rootStat)
		sw.SiblingManifest = []DirSwitchEntry{{Name: "alpha", LinkTarget: "expected", Identity: adoptIdentity(&childStat)}}
		moved := filepath.Join(parentPath, retired+".owned")
		h := hooks{beforeDirSwitchChildRetire: func(string) error {
			if err := os.Rename(filepath.Join(parentPath, retired), moved); err != nil {
				return err
			}
			return os.Mkdir(filepath.Join(parentPath, retired), 0o755)
		}}

		if err := removeDirSwitchSibling(parent, parentPath, name, sw, false, h); !errors.Is(err, ErrTxnConflict) {
			t.Fatalf("changed retired root must conflict, got %v", err)
		}
		if _, err := os.Lstat(filepath.Join(parentPath, name)); !os.IsNotExist(err) {
			t.Fatalf("resume must not restore the root to its live name: %v", err)
		}
		if _, err := os.Stat(filepath.Join(parentPath, retired)); err != nil {
			t.Fatalf("retired root must remain preserved: %v", err)
		}
	})

	t.Run("replacement child", func(t *testing.T) {
		parentPath := t.TempDir()
		sw := &DirSwitchState{CleanupID: "0011223344556677"}
		expected := DirSwitchEntry{Name: "alpha", LinkTarget: "expected", Identity: store.FileIdentity{Device: 1, Inode: 1}}
		retired := dirSwitchRetiredName(sw, "child", expected.Name)
		if err := os.Symlink("foreign", filepath.Join(parentPath, retired)); err != nil {
			t.Fatal(err)
		}
		parent, err := os.Open(parentPath)
		if err != nil {
			t.Fatal(err)
		}
		defer parent.Close()

		if err := retireDirSwitchLink(parent, parentPath, expected, sw, nil); !errors.Is(err, ErrTxnConflict) {
			t.Fatalf("changed retired child must conflict, got %v", err)
		}
		if _, err := os.Lstat(filepath.Join(parentPath, expected.Name)); !os.IsNotExist(err) {
			t.Fatalf("resume must not restore the child to its live name: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(parentPath, retired)); err != nil {
			t.Fatalf("retired child must remain preserved: %v", err)
		}
	})

	t.Run("backup", func(t *testing.T) {
		parentPath := t.TempDir()
		name := ".fu-skills-old-owned"
		sw := &DirSwitchState{Backup: filepath.Join(parentPath, name), CleanupID: "0011223344556677", BackupIdentity: store.FileIdentity{Device: 1, Inode: 1}}
		target := AdoptTarget{EntryIdentity: sw.BackupIdentity, LinkTarget: "expected"}
		retired := dirSwitchRetiredName(sw, "backup", name)
		if err := os.Symlink("foreign", filepath.Join(parentPath, retired)); err != nil {
			t.Fatal(err)
		}
		parent, err := os.Open(parentPath)
		if err != nil {
			t.Fatal(err)
		}
		defer parent.Close()

		if err := removeDirSwitchBackup(nil, parent, target, "alpha", sw, hooks{}); !errors.Is(err, ErrTxnConflict) {
			t.Fatalf("changed retired backup must conflict, got %v", err)
		}
		if _, err := os.Lstat(filepath.Join(parentPath, name)); !os.IsNotExist(err) {
			t.Fatalf("resume must not restore the backup to its live name: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(parentPath, retired)); err != nil {
			t.Fatalf("retired backup must remain preserved: %v", err)
		}
	})
}

// TestResumeDirSwitchRefusesLegacyBuildingWithoutLink pins the defensive
// check in the "building" resume: a transaction written before the WAL-first
// ordering can record "building" while the parent link is already archived
// and no backup name is recorded. The resume must refuse that legacy shape
// with a conflict -- leaving the fully-built sibling in place for manual
// repair -- instead of deleting it and failing at wholeDirTarget's ENOENT.
func TestResumeDirSwitchRefusesLegacyBuildingWithoutLink(t *testing.T) {
	s, _ := setupStore(t)
	parent := t.TempDir()
	txn := &TxnRecord{
		Op: "adopt", Name: "alpha",
		DirSwitch: &DirSwitchState{
			Agent: "claude", Target: filepath.Join(parent, "target"),
			Sibling: filepath.Join(parent, ".fu-skills-legacy"), Stage: "building",
		},
	}
	if err := os.MkdirAll(txn.DirSwitch.Sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	// The skills position is absent while the sibling exists: the exact
	// legacy crash shape.
	a := fakeAgent{"claude", filepath.Join(parent, "skills")}
	err := resumeDirSwitch(s, a, "alpha", txn, hooks{})
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("legacy building shape must refuse with ErrTxnConflict, got %v", err)
	}
	if _, err := os.Stat(txn.DirSwitch.Sibling); err != nil {
		t.Fatalf("the replacement directory must survive the refusal for manual repair: %v", err)
	}
}

// TestAdoptWholeDirSymlink switches a symlinked skills directory: the
// adopted skill becomes a store link, non-skill content becomes a
// passthrough link to the (untouched) target, and the parent link is
// archived then removed.
func TestAdoptWholeDirSymlink(t *testing.T) {
	fuHome, homeDir, target := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	agents := []agent.Agent{agent.Claude{}}
	res, err := Adopt(s, agents, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 1 || res.Adopted[0].Name != "pdf-tools" {
		t.Fatalf("adopted = %+v", res.Adopted)
	}
	if len(res.Reconcile.Skipped) != 0 {
		t.Fatalf("successful whole-directory adoption must not retain stale prologue skips: %+v", res.Reconcile.Skipped)
	}
	skillsDir := filepath.Join(homeDir, ".claude", "skills")
	info, err := os.Lstat(skillsDir)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("skills dir must become a real directory: %v %v", info, err)
	}
	// The adopted skill is a store link.
	linkInfo, err := os.Lstat(filepath.Join(skillsDir, "pdf-tools"))
	if err != nil || linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("adopted skill must be a store link: %v %v", linkInfo, err)
	}
	// The non-skill content is a passthrough link into the untouched target.
	passInfo, err := os.Lstat(filepath.Join(skillsDir, "notes.txt"))
	if err != nil || passInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("non-skill content must be a passthrough link: %v %v", passInfo, err)
	}
	passTarget, err := os.Readlink(filepath.Join(skillsDir, "notes.txt"))
	wantTarget := target
	if err != nil || passTarget != filepath.Join(wantTarget, "notes.txt") {
		t.Fatalf("passthrough target %q, want %q", passTarget, filepath.Join(wantTarget, "notes.txt"))
	}
	// The original target is untouched.
	if _, err := os.Stat(filepath.Join(target, "pdf-tools", "SKILL.md")); err != nil {
		t.Fatalf("target must stay untouched: %v", err)
	}
	// The agent still sees the notes content through the passthrough.
	if got, err := os.ReadFile(filepath.Join(skillsDir, "notes.txt")); err != nil || string(got) != "keep" {
		t.Fatalf("agent view of notes.txt = %q, err %v", got, err)
	}
	cfg, err := store.LoadConfig(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	fields := cfg.SourceFields("pdf-tools")
	canonicalTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if fields["type"] != "local" || fields["path"] != filepath.Join(canonicalTarget, "pdf-tools") {
		t.Fatalf("whole-directory adoption must record its local child source, got %v", fields)
	}
	// No archived parent link remains.
	entries, err := os.ReadDir(filepath.Join(homeDir, ".claude"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".fu-skills-") {
			t.Fatalf("leftover whole-directory switch entry %s", e.Name())
		}
	}
	records := readAdoptLinkArchives(t, s)
	if len(records) != 1 || records[0].Kind != adoptLinkArchiveWholeDirectory ||
		records[0].OriginalPath != filepath.Join(homeDir, ".claude", "skills") || records[0].RawTarget != target {
		t.Fatalf("removed whole-directory link was not durably archived: %+v", records)
	}
	if _, err := PruneCompletedTransactions(s); err != nil {
		t.Fatal(err)
	}
	if after := readAdoptLinkArchives(t, s); len(after) != 1 {
		t.Fatalf("gc removed whole-directory link archive: %+v", after)
	}
}

func TestAdoptWholeDirAcceptsSkillSymlinkResolvingOutsideTarget(t *testing.T) {
	fuHome, homeDir, target := wholeDirFixture(t)
	if err := os.RemoveAll(filepath.Join(target, "pdf-tools")); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	externalSkill := writeSkillTree(t, external, "pdf-tools", "---\nname: pdf-tools\ndescription: external\n---\n")
	if err := os.Symlink(externalSkill, filepath.Join(target, "pdf-tools")); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Adopt(s, []agent.Agent{agent.Claude{}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 1 || res.Adopted[0].Name != "pdf-tools" || len(res.Adopted[0].Agents) != 1 {
		t.Fatalf("out-of-target skill symlink must be adopted and switched: %+v", res)
	}
	managed := filepath.Join(homeDir, ".claude", "skills", "pdf-tools")
	if info, statErr := os.Lstat(managed); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("agent entry must become a managed symlink: %v, %v", info, statErr)
	}
}

func TestReplacementDirManifestPreservesParentLinkIndirection(t *testing.T) {
	target := AdoptTarget{
		SkillsDir:  "/home/user/.claude/skills",
		LinkTarget: "../dotfiles/skills",
		SourcePath: "/resolved/current/skills",
		TargetManifest: []DirSwitchEntry{
			{Name: "notes.txt"},
		},
	}
	manifest := replacementDirManifest(&store.Store{}, target, "adopted")
	if len(manifest) != 1 || manifest[0].LinkTarget != "../../dotfiles/skills/notes.txt" {
		t.Fatalf("passthrough collapsed the user's parent-link indirection: %+v", manifest)
	}
}

func TestAdoptWholeDirRefusesRootSkill(t *testing.T) {
	for _, withChild := range []bool{false, true} {
		label := "root-only"
		if withChild {
			label = "root-and-child"
		}
		t.Run(label, func(t *testing.T) {
			fuHome := t.TempDir()
			if _, err := store.Init(fuHome); err != nil {
				t.Fatal(err)
			}
			homeDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(homeDir, ".claude"), 0o755); err != nil {
				t.Fatal(err)
			}
			target := t.TempDir()
			rootSkill := "---\nname: root-skill\ndescription: root\n---\n"
			if err := os.WriteFile(filepath.Join(target, "SKILL.md"), []byte(rootSkill), 0o644); err != nil {
				t.Fatal(err)
			}
			if withChild {
				writeSkillTree(t, target, "pdf-tools", "---\nname: pdf-tools\ndescription: child\n---\n")
			}
			skillsDir := filepath.Join(homeDir, ".claude", "skills")
			if err := os.Symlink(target, skillsDir); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FU_HOME", fuHome)
			t.Setenv("HOME", homeDir)
			s, err := store.Open(fuHome)
			if err != nil {
				t.Fatal(err)
			}

			// The refusal is per agent, not per run (round 18 finding I12), so
			// it arrives as a reported failure rather than a returned error.
			// Everything else this test pins -- the untouched link, the
			// untouched root skill, no pending transaction -- is unchanged.
			res, err := Adopt(s, []agent.Agent{agent.Claude{}}, "")
			if err != nil {
				t.Fatalf("a per-agent refusal must not abort the run: %v", err)
			}
			refused := false
			for _, f := range res.Failed {
				if errors.Is(f.Err, ErrWholeDirRootSkillUnsupported) {
					refused = true
				}
			}
			if !refused {
				t.Fatalf("res.Failed = %+v, want ErrWholeDirRootSkillUnsupported", res.Failed)
			}
			if len(res.Adopted) != 0 {
				t.Fatalf("the refused agent must adopt nothing: %+v", res.Adopted)
			}
			info, statErr := os.Lstat(skillsDir)
			if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("skills entry = %v, %v; want untouched symlink", info, statErr)
			}
			if got, readErr := os.ReadFile(filepath.Join(target, "SKILL.md")); readErr != nil || string(got) != rootSkill {
				t.Fatalf("root skill = %q, %v; want untouched", got, readErr)
			}
			pending, pendingErr := PendingTxns(s)
			if pendingErr != nil || len(pending) != 0 {
				t.Fatalf("pending transactions = %+v, %v; want none", pending, pendingErr)
			}
		})
	}
}

// TestAdoptWholeDirSecondSkill falls back to per-entry switching once the
// directory is real: a second skill from the same target is adopted through
// its passthrough link, which is archived and replaced by a store link.
func TestAdoptWholeDirSecondSkill(t *testing.T) {
	fuHome, homeDir, target := wholeDirFixture(t)
	writeSkillTree(t, target, "linter", "---\nname: linter\ndescription: d\n---\n")
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	agents := []agent.Agent{agent.Claude{}}
	res, err := Adopt(s, agents, "")
	if err != nil {
		t.Fatal(err)
	}
	// Both skills are adopted in one run: the first triggers the directory
	// swap, the second's passthrough link is archived per entry.
	if len(res.Adopted) != 2 {
		t.Fatalf("adopted = %+v", res.Adopted)
	}
	skillsDir := filepath.Join(homeDir, ".claude", "skills")
	for _, name := range []string{"pdf-tools", "linter"} {
		info, err := os.Lstat(filepath.Join(skillsDir, name))
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s must be a store link: %v %v", name, info, err)
		}
	}
	// The non-skill content survives as a passthrough across the swap.
	if got, err := os.ReadFile(filepath.Join(skillsDir, "notes.txt")); err != nil || string(got) != "keep" {
		t.Fatalf("notes.txt view = %q, err %v", got, err)
	}
}

func TestSwitchWholeDirAgentRecognizesAlreadySwitchedDirectory(t *testing.T) {
	fuHome, homeDir, _ := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	a := agent.Claude{}
	digest, err := digestDir(filepath.Join(homeDir, ".claude", "skills", "pdf-tools"))
	if err != nil {
		t.Fatal(err)
	}
	targets, err := captureAdoptTargets([]agent.Agent{a}, []agent.Agent{a}, "pdf-tools", digest)
	if err != nil {
		t.Fatal(err)
	}
	txn := &TxnRecord{Op: "adopt", Name: "pdf-tools", AdoptTargets: targets, Agents: []string{"claude"}, WholeDirAgents: []string{"claude"}}
	if err := switchWholeDirAgent(s, a, "pdf-tools", txn, hooks{}); err != nil {
		t.Fatal(err)
	}
	if txn.DirSwitch != nil {
		t.Fatalf("completed switch state = %+v, want nil", txn.DirSwitch)
	}

	if err := switchWholeDirAgent(s, a, "pdf-tools", txn, hooks{}); err != nil {
		t.Fatalf("revisiting an already-switched agent must be an idempotent success: %v", err)
	}
	info, err := os.Lstat(filepath.Join(homeDir, ".claude", "skills"))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("already-switched directory changed: info=%v err=%v", info, err)
	}
}

// TestAdoptWholeDirRecoversAfterProcessInterruption crashes the
// whole-directory switch at its three durable boundaries and asserts
// recovery completes the swap deterministically. "after-archive" covers the
// window between the swapped WAL revision and the archive rename (the
// parent link is still in place); "after-swap" covers the window between
// the archive rename and the swap rename.
func TestAdoptWholeDirRecoversAfterProcessInterruption(t *testing.T) {
	if os.Getenv("FU_TEST_CRASH_WHOLEDIR_HELPER") == "1" {
		fuHome := os.Getenv("FU_TEST_CRASH_WHOLEDIR_HOME")
		stage := os.Getenv("FU_TEST_CRASH_WHOLEDIR_STAGE")
		s, err := store.Open(fuHome)
		if err != nil {
			panic(err)
		}
		crash := func() error { os.Exit(86); return nil }
		var h hooks
		switch stage {
		case "after-build":
			h.afterDirSwitchBuild = crash
		case "after-child-create":
			h.afterDirSwitchChildCreate = func(string) error { return crash() }
		case "after-archive":
			h.beforeDirSwitchArchive = crash
		case "after-swap":
			h.afterDirSwitchSwap = crash
		case "after-second-agent-swap":
			swapped := 0
			h.afterDirSwitchSwap = func() error {
				swapped++
				if swapped == 2 {
					return crash()
				}
				return nil
			}
		default:
			panic("unknown crash stage " + stage)
		}
		agents := []agent.Agent{agent.Claude{}}
		if stage == "after-second-agent-swap" {
			agents = append(agents, agent.Codex{})
		}
		_, _ = adopt(s, agents, "", h)
		panic("crash hook did not run")
	}

	for _, stage := range []string{"after-child-create", "after-build", "after-archive", "after-swap", "after-second-agent-swap"} {
		t.Run(stage, func(t *testing.T) {
			fuHome, homeDir, target := wholeDirFixture(t)
			targets := []string{target}
			if stage == "after-second-agent-swap" {
				codexTarget := t.TempDir()
				writeSkillTree(t, codexTarget, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
				if err := os.WriteFile(filepath.Join(codexTarget, "notes.txt"), []byte("keep"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(homeDir, ".codex"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(codexTarget, filepath.Join(homeDir, ".codex", "skills")); err != nil {
					t.Fatal(err)
				}
				targets = append(targets, codexTarget)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestAdoptWholeDirRecoversAfterProcessInterruption$")
			cmd.Env = append(os.Environ(),
				"FU_TEST_CRASH_WHOLEDIR_HELPER=1",
				"FU_TEST_CRASH_WHOLEDIR_HOME="+fuHome,
				"FU_HOME="+fuHome,
				"HOME="+homeDir,
				"FU_TEST_CRASH_WHOLEDIR_STAGE="+stage,
			)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
				t.Fatalf("child must terminate at %s with code 86, err=%v output=%s", stage, err, output)
			}

			s, err := store.Open(fuHome)
			if err != nil {
				t.Fatal(err)
			}
			agents := []agent.Agent{agent.Claude{}}
			if stage == "after-second-agent-swap" {
				agents = append(agents, agent.Codex{})
			}
			if _, err := NewSkill(s, agents, "beta"); err != nil {
				t.Fatalf("next write after %s must recover: %v", stage, err)
			}
			for _, agentDir := range []string{".claude", ".codex"} {
				if agentDir == ".codex" && stage != "after-second-agent-swap" {
					continue
				}
				skillsDir := filepath.Join(homeDir, agentDir, "skills")
				info, err := os.Lstat(skillsDir)
				if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
					t.Fatalf("%s skills dir must be a real directory after %s: %v %v", agentDir, stage, info, err)
				}
				// The adopted skill's link is delivered by the recovery's
				// trailing reconcile.
				linkInfo, err := os.Lstat(filepath.Join(skillsDir, "pdf-tools"))
				if err != nil || linkInfo.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("%s pdf-tools must be a store link after %s: %v %v", agentDir, stage, linkInfo, err)
				}
				// The passthrough survived.
				if got, err := os.ReadFile(filepath.Join(skillsDir, "notes.txt")); err != nil || string(got) != "keep" {
					t.Fatalf("%s notes.txt view after %s = %q, err %v", agentDir, stage, got, err)
				}
			}
			// The whole-directory target stays untouched: recovery must
			// never archive or delete the original from the user's target
			// through the parent symlink (finding I1).
			for _, target := range targets {
				if _, err := os.Stat(filepath.Join(target, "pdf-tools", "SKILL.md")); err != nil {
					t.Fatalf("target must stay untouched after %s: %v", stage, err)
				}
			}
			pending, err := PendingTxns(s)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 0 {
				t.Fatalf("recovery must clear its WAL after %s, got %+v", stage, pending)
			}
		})
	}
}

func TestAdoptWholeDirBatchesChildManifestRevision(t *testing.T) {
	fuHome, _, target := wholeDirFixture(t)
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("note-%02d.txt", i)
		if err := os.WriteFile(filepath.Join(target, name), []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	a := agent.Claude{}
	digest, err := digestDir(filepath.Join(target, "pdf-tools"))
	if err != nil {
		t.Fatal(err)
	}
	targets, err := captureAdoptTargets([]agent.Agent{a}, []agent.Agent{a}, "pdf-tools", digest)
	if err != nil {
		t.Fatal(err)
	}
	txn := &TxnRecord{Op: "adopt", Name: "pdf-tools", AdoptTargets: targets}
	stop := errors.New("stop after replacement build")
	revisions := 0
	err = startDirSwitch(s, a, "pdf-tools", txn, hooks{
		afterDirSwitchBuild: func() error {
			revisions = len(txnRevisionPaths(t, s))
			return stop
		},
	})
	if !errors.Is(err, stop) {
		t.Fatalf("switch error = %v, want stop hook", err)
	}
	if revisions > 4 {
		t.Fatalf("building 10 children wrote %d WAL revisions; child identities must be persisted as one batch", revisions)
	}
}

// TestAbandonDirSwitch pins the legacy-record boundary: path-only switch
// records carry no authority to restore or remove anything. Cleanup must
// conflict and preserve every artifact for manual recovery.
func TestAbandonDirSwitch(t *testing.T) {
	t.Run("building preserves sibling", func(t *testing.T) {
		parent := t.TempDir()
		skills := filepath.Join(parent, "skills")
		target := filepath.Join(parent, "real")
		if err := os.Symlink(target, skills); err != nil {
			t.Fatal(err)
		}
		sibling := filepath.Join(parent, ".fu-skills-x")
		if err := os.MkdirAll(sibling, 0o755); err != nil {
			t.Fatal(err)
		}
		txn := &TxnRecord{DirSwitch: &DirSwitchState{Agent: "claude", Target: target, Sibling: sibling, Stage: "building"}}
		a := fakeAgent{"claude", skills}
		if _, err := abandonDirSwitch(a, txn); !errors.Is(err, ErrTxnConflict) {
			t.Fatalf("legacy cleanup error = %v, want ErrTxnConflict", err)
		}
		if _, err := os.Stat(sibling); err != nil {
			t.Fatalf("unowned sibling must be preserved, err=%v", err)
		}
		if _, err := os.Lstat(skills); err != nil {
			t.Fatalf("parent link must be untouched, err=%v", err)
		}
		if txn.DirSwitch == nil {
			t.Fatal("DirSwitch must remain recoverable")
		}
	})
	t.Run("swapped preserves backup and sibling", func(t *testing.T) {
		parent := t.TempDir()
		skills := filepath.Join(parent, "skills")
		target := filepath.Join(parent, "real")
		if err := os.Symlink(target, skills); err != nil {
			t.Fatal(err)
		}
		backup := filepath.Join(parent, ".fu-skills-old-x")
		sibling := filepath.Join(parent, ".fu-skills-x")
		if err := os.MkdirAll(sibling, 0o755); err != nil {
			t.Fatal(err)
		}
		// The archive rename ran; the swap rename never did.
		if err := os.Rename(skills, backup); err != nil {
			t.Fatal(err)
		}
		txn := &TxnRecord{DirSwitch: &DirSwitchState{Agent: "claude", Target: target, Sibling: sibling, Backup: backup, Stage: "swapped"}}
		a := fakeAgent{"claude", skills}
		if _, err := abandonDirSwitch(a, txn); !errors.Is(err, ErrTxnConflict) {
			t.Fatalf("legacy cleanup error = %v, want ErrTxnConflict", err)
		}
		if _, err := os.Lstat(skills); !os.IsNotExist(err) {
			t.Fatalf("skills position must remain absent, err=%v", err)
		}
		if _, err := os.Stat(sibling); err != nil {
			t.Fatalf("unowned sibling must be preserved, err=%v", err)
		}
		if got, err := os.Readlink(backup); err != nil || got != target {
			t.Fatalf("unowned backup must be preserved, got %q err=%v", got, err)
		}
		if txn.DirSwitch == nil {
			t.Fatal("DirSwitch must remain recoverable")
		}
	})
	t.Run("swapped with link still in place preserves sibling", func(t *testing.T) {
		parent := t.TempDir()
		skills := filepath.Join(parent, "skills")
		target := filepath.Join(parent, "real")
		if err := os.Symlink(target, skills); err != nil {
			t.Fatal(err)
		}
		sibling := filepath.Join(parent, ".fu-skills-x")
		if err := os.MkdirAll(sibling, 0o755); err != nil {
			t.Fatal(err)
		}
		// The swapped revision was recorded but the archive rename never ran.
		txn := &TxnRecord{DirSwitch: &DirSwitchState{Agent: "claude", Target: target, Sibling: sibling, Backup: filepath.Join(parent, ".fu-skills-old-x"), Stage: "swapped"}}
		a := fakeAgent{"claude", skills}
		if _, err := abandonDirSwitch(a, txn); !errors.Is(err, ErrTxnConflict) {
			t.Fatalf("legacy cleanup error = %v, want ErrTxnConflict", err)
		}
		if got, err := os.Readlink(skills); err != nil || got != target {
			t.Fatalf("parent link must stay in place, got %q err %v", got, err)
		}
		if _, err := os.Stat(sibling); err != nil {
			t.Fatalf("unowned sibling must be preserved, err=%v", err)
		}
		if txn.DirSwitch == nil {
			t.Fatal("DirSwitch must remain recoverable")
		}
	})
}

// TestAdoptWholeDirSkipsFuLinksInTarget pins round 9 finding M1: the
// whole-directory scan must skip entries that are fu's own links into the
// store, mirroring the per-entry KindFuLink skip. A hand-placed store link
// for a managed name must not produce the "still holds an unadopted copy"
// warning (its way out is unreachable for that shape), and a store link for
// an unmanaged name must not be adopted (its content is the store's own).
func TestAdoptWholeDirSkipsFuLinksInTarget(t *testing.T) {
	fuHome := t.TempDir()
	if _, err := store.Init(fuHome); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSkill(s, nil, "alpha"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FU_HOME", fuHome)
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	if err := os.MkdirAll(filepath.Join(homeDir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	writeSkillTree(t, target, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	// A hand-placed fu link for the managed name inside the target.
	if err := os.Symlink(filepath.Join(s.SkillsDir(), "alpha"), filepath.Join(target, "alpha")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(homeDir, ".claude", "skills")); err != nil {
		t.Fatal(err)
	}

	res, err := Adopt(s, []agent.Agent{agent.Claude{}}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "alpha") && strings.Contains(w, "unadopted copy") {
			t.Fatalf("a store link in the target must not produce the unadopted-copy warning: %q", w)
		}
	}
	if len(res.Adopted) != 1 || res.Adopted[0].Name != "pdf-tools" {
		t.Fatalf("adopted = %+v", res.Adopted)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("store links must not be skipped as foreign copies, skipped = %v", res.Skipped)
	}
}

// TestAdoptWholeDirRefusesChangedTargetEntry pins round 11 finding I2: the
// whole-directory swap must re-verify the adopted entry's content against
// the inventory digest, mirroring freshArchive's refusal for the per-entry
// path. The mutation lands between the inventory and the swap (via the
// afterDirSwitchBuild hook); the switch must isolate the agent, preserve the
// original symlink, and close the WAL.
func TestAdoptWholeDirRefusesChangedTargetEntry(t *testing.T) {
	fuHome, homeDir, target := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", homeDir)
	h := hooks{afterDirSwitchBuild: func() error {
		body := "---\nname: pdf-tools\ndescription: changed\n---\n\nbody\n"
		return os.WriteFile(filepath.Join(target, "pdf-tools", "SKILL.md"), []byte(body), 0o644)
	}}

	res, err := adopt(s, []agent.Agent{agent.Claude{}}, "", h)
	if err != nil {
		t.Fatalf("changed user target must be isolated: %v", err)
	}
	if len(res.Pending) != 1 || res.Pending[0].Name != "pdf-tools" {
		t.Fatalf("isolated whole-directory adoption must be pending: %+v", res.Pending)
	}
	if len(res.Warnings) == 0 || !strings.Contains(strings.Join(res.Warnings, "\n"), "changed") {
		t.Fatalf("whole-directory isolation must retain the concrete cause: %v", res.Warnings)
	}
	// The skills position was left as the original symlink: the swap was
	// refused. The identity-bound sibling remains named in the WAL rather
	// than being treated as disposable after a conflict.
	skillsDir := filepath.Join(homeDir, ".claude", "skills")
	info, err := os.Lstat(skillsDir)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("skills dir must stay a symlink after a refused swap: %v %v", info, err)
	}
	// The mutated content survives in the target.
	if got, err := os.ReadFile(filepath.Join(target, "pdf-tools", "SKILL.md")); err != nil || !strings.Contains(string(got), "changed") {
		t.Fatalf("mutated target content must survive, got %q err %v", got, err)
	}
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("isolated conflict must close its WAL, got %+v", pending)
	}
}

func TestAdoptWholeDirRejectsParentEntryReplacementBeforeArchive(t *testing.T) {
	fuHome, homeDir, target := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	skillsDir := filepath.Join(homeDir, ".claude", "skills")
	foreignMarker := filepath.Join(skillsDir, "foreign.txt")
	h := hooks{beforeDirSwitchArchive: func() error {
		if err := os.Remove(skillsDir); err != nil {
			return err
		}
		if err := os.Mkdir(skillsDir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(foreignMarker, []byte("foreign\n"), 0o644)
	}}

	res, err := adopt(s, []agent.Agent{agent.Claude{}}, "", h)
	if err != nil {
		t.Fatalf("a pre-archive live-name replacement must isolate the agent: %v", err)
	}
	if len(res.Pending) != 1 || res.Pending[0].Name != "pdf-tools" {
		t.Fatalf("isolated whole-directory adoption must be pending: %+v", res.Pending)
	}
	if got, err := os.ReadFile(foreignMarker); err != nil || string(got) != "foreign\n" {
		t.Fatalf("foreign replacement = %q, %v; want preserved", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "pdf-tools", "SKILL.md")); err != nil || !strings.Contains(string(got), "name: pdf-tools") {
		t.Fatalf("target source = %q, %v; want preserved", got, err)
	}
	records, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("pending transactions = %d, want 0 after isolation", len(records))
	}
}

func TestAdoptWholeDirRejectsTargetSetChangeBeforeSwap(t *testing.T) {
	fuHome, homeDir, target := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	late := filepath.Join(target, "late.txt")
	h := hooks{afterDirSwitchBuild: func() error {
		return os.WriteFile(late, []byte("late\n"), 0o644)
	}}

	res, err := adopt(s, []agent.Agent{agent.Claude{}}, "", h)
	if err != nil {
		t.Fatalf("a changed user target set must isolate the agent: %v", err)
	}
	if len(res.Pending) != 1 || res.Pending[0].Name != "pdf-tools" {
		t.Fatalf("isolated whole-directory adoption must be pending: %+v", res.Pending)
	}
	info, statErr := os.Lstat(filepath.Join(homeDir, ".claude", "skills"))
	if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("skills entry = %v, %v; want original symlink", info, statErr)
	}
	if got, readErr := os.ReadFile(late); readErr != nil || string(got) != "late\n" {
		t.Fatalf("late target entry = %q, %v; want preserved", got, readErr)
	}
	records, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("pending transactions = %d, want 0 after isolation", len(records))
	}
}

func TestAdoptWholeDirPreservesReplacementSiblingDuringCleanup(t *testing.T) {
	fuHome, homeDir, _ := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(homeDir, ".claude")
	var foreignMarker string
	injected := errors.New("injected switch failure")
	h := hooks{beforeDirSwitchArchive: func() error {
		sibling := findWholeDirArtifact(t, parent, ".fu-skills-")
		if err := os.Rename(sibling, sibling+".owned"); err != nil {
			return err
		}
		if err := os.Mkdir(sibling, 0o755); err != nil {
			return err
		}
		foreignMarker = filepath.Join(sibling, "foreign.txt")
		if err := os.WriteFile(foreignMarker, []byte("foreign\n"), 0o644); err != nil {
			return err
		}
		return injected
	}}

	_, err = adopt(s, []agent.Agent{agent.Claude{}}, "", h)
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("adopt error = %v, want cleanup ErrTxnConflict", err)
	}
	if got, readErr := os.ReadFile(foreignMarker); readErr != nil || string(got) != "foreign\n" {
		t.Fatalf("replacement sibling = %q, %v; want preserved", got, readErr)
	}
	if records, readErr := PendingTxns(s); readErr != nil || len(records) != 1 {
		t.Fatalf("pending transactions = %+v, %v; want one", records, readErr)
	}
}

func TestAdoptWholeDirPreservesReplacementBackupDuringCleanup(t *testing.T) {
	fuHome, homeDir, _ := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(homeDir, ".claude")
	var foreignMarker string
	injected := errors.New("injected switch failure")
	h := hooks{afterDirSwitchSwap: func() error {
		backup := findWholeDirArtifact(t, parent, ".fu-skills-old-")
		if err := os.Remove(backup); err != nil {
			return err
		}
		if err := os.Mkdir(backup, 0o755); err != nil {
			return err
		}
		foreignMarker = filepath.Join(backup, "foreign.txt")
		if err := os.WriteFile(foreignMarker, []byte("foreign\n"), 0o644); err != nil {
			return err
		}
		return injected
	}}

	_, err = adopt(s, []agent.Agent{agent.Claude{}}, "", h)
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("adopt error = %v, want cleanup ErrTxnConflict", err)
	}
	if got, readErr := os.ReadFile(foreignMarker); readErr != nil || string(got) != "foreign\n" {
		t.Fatalf("replacement backup = %q, %v; want preserved", got, readErr)
	}
	if records, readErr := PendingTxns(s); readErr != nil || len(records) != 1 {
		t.Fatalf("pending transactions = %+v, %v; want one", records, readErr)
	}
}

func TestAdoptWholeDirRetiresChildBeforeCleanup(t *testing.T) {
	fuHome, homeDir, _ := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("abandon replacement directory")
	var foreignPath string
	h := hooks{
		afterDirSwitchBuild: func() error { return injected },
		beforeDirSwitchChildRetire: func(path string) error {
			foreignPath = path
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("foreign child"), 0o644)
		},
	}
	_, err = adopt(s, []agent.Agent{agent.Claude{}}, "", h)
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("child replacement at retirement must conflict, got %v", err)
	}
	if got, readErr := os.ReadFile(foreignPath); readErr != nil || string(got) != "foreign child" {
		t.Fatalf("foreign child must survive: %q, %v", got, readErr)
	}
	if records, readErr := PendingTxns(s); readErr != nil || len(records) != 1 {
		t.Fatalf("child cleanup conflict must retain the WAL: %+v, %v", records, readErr)
	}
	_ = homeDir
}

func TestAdoptWholeDirRetiresSiblingRootBeforeCleanup(t *testing.T) {
	fuHome, homeDir, _ := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("abandon replacement directory")
	var marker string
	h := hooks{
		afterDirSwitchBuild: func() error { return injected },
		beforeDirSwitchRootRetire: func(root string) error {
			if err := os.Rename(root, root+".owned"); err != nil {
				return err
			}
			if err := os.Mkdir(root, 0o755); err != nil {
				return err
			}
			marker = filepath.Join(root, "foreign.txt")
			return os.WriteFile(marker, []byte("foreign root"), 0o644)
		},
	}
	_, err = adopt(s, []agent.Agent{agent.Claude{}}, "", h)
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("sibling-root replacement at retirement must conflict, got %v", err)
	}
	if got, readErr := os.ReadFile(marker); readErr != nil || string(got) != "foreign root" {
		t.Fatalf("foreign sibling root must survive: %q, %v", got, readErr)
	}
	_ = homeDir
}

func TestAdoptWholeDirRetiresBackupBeforeCleanup(t *testing.T) {
	fuHome, homeDir, _ := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	var backup string
	h := hooks{beforeDirSwitchBackupRetire: func(path string) error {
		backup = path
		if err := os.Remove(path); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("foreign backup"), 0o644)
	}}
	_, err = adopt(s, []agent.Agent{agent.Claude{}}, "", h)
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("backup replacement at retirement must conflict, got %v", err)
	}
	if got, readErr := os.ReadFile(backup); readErr != nil || string(got) != "foreign backup" {
		t.Fatalf("foreign backup must survive: %q, %v", got, readErr)
	}
	_ = homeDir
}

// TestAdoptWholeDirLandedReentryToleratesTargetChange replaces the test that
// used to assert the opposite. Once the replacement has landed, the target is
// not re-validated at all: the only operation left is removing fu's own
// backup, which validateDirSwitchBackup proves by inode, mode and raw link
// text on its own.
//
// The check that used to sit here was narrowed three times and refused an
// ordinary user action every time -- by digest (round 18 I6), then by child
// inode (round 19), then still by name set even with every failure mode
// tagged, because the abandon's own landed arm re-runs it and refuses
// identically. A user who adds a file to their dotfiles directory while an
// adopt is finishing is doing nothing wrong, and fu holds nothing at that
// point that the change could invalidate.
func TestAdoptWholeDirLandedReentryToleratesTargetChange(t *testing.T) {
	fuHome, homeDir, target := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	h := hooks{afterDirSwitchLand: func() error {
		return os.WriteFile(filepath.Join(target, "appeared.txt"), []byte("new child"), 0o644)
	}}
	if _, err = adopt(s, []agent.Agent{agent.Claude{}}, "", h); err != nil {
		t.Fatalf("a target change after landing must not refuse: %v", err)
	}
	if records, readErr := PendingTxns(s); readErr != nil || len(records) != 0 {
		t.Fatalf("the transaction must reach a terminal state: %+v, %v", records, readErr)
	}
	// The backup is gone -- the switch finished rather than stalling.
	entries, readErr := os.ReadDir(filepath.Join(homeDir, ".claude"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".fu-skills-old-") {
			t.Fatalf("the backup must be removed once the switch completes: %s", e.Name())
		}
	}
	info, statErr := os.Lstat(filepath.Join(homeDir, ".claude", "skills"))
	if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("the landed replacement directory must remain: %v %v", info, statErr)
	}
	// The user's new file is untouched.
	if _, statErr := os.Lstat(filepath.Join(target, "appeared.txt")); statErr != nil {
		t.Fatalf("the user's own new child must survive: %v", statErr)
	}
}

// TestAdoptWholeDirRejectsRootSkillInsertedBeforeTargetCapture covers the race
// the scan-time detection cannot see: the root SKILL.md appears between the
// scan and the pinned capture. The capture must still refuse it, before the
// WAL and with the link untouched -- but as an isolated per-candidate refusal,
// not by aborting the run. Returning it out of adopt was the same defect the
// scan-time path was fixed for, one code path over: DESIGN §6 states the
// refusal as 拒绝整个 agent，保持原链接与目标不变, alongside 任一 skill 失败不影响
// 其他项.
func TestAdoptWholeDirRejectsRootSkillInsertedBeforeTargetCapture(t *testing.T) {
	fuHome, homeDir, target := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	h := hooks{beforeAdoptTargetCapture: func() error {
		return os.WriteFile(filepath.Join(target, "SKILL.md"), []byte("---\nname: root\ndescription: late root\n---\n"), 0o644)
	}}
	res, err := adopt(s, []agent.Agent{agent.Claude{}}, "", h)
	if err != nil {
		t.Fatalf("a late root SKILL.md must isolate the candidate, not abort the run: %v", err)
	}
	reported := false
	for _, f := range res.Failed {
		if errors.Is(f.Err, ErrWholeDirRootSkillUnsupported) &&
			strings.Contains(f.Err.Error(), "named child directory") {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("the refusal must reach the user with its actionable hint: %+v", res.Failed)
	}
	if len(res.Adopted) != 0 || len(res.Pending) != 0 {
		t.Fatalf("nothing may be reported as adopted: %+v", res)
	}
	if records, readErr := PendingTxns(s); readErr != nil || len(records) != 0 {
		t.Fatalf("capture refusal must happen before the WAL: %+v, %v", records, readErr)
	}
	info, statErr := os.Lstat(filepath.Join(homeDir, ".claude", "skills"))
	if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("original skills link must remain: %v, %v", info, statErr)
	}
}

func findWholeDirArtifact(t *testing.T, parent, prefix string) string {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			if prefix == ".fu-skills-" && strings.HasPrefix(entry.Name(), ".fu-skills-old-") {
				continue
			}
			return filepath.Join(parent, entry.Name())
		}
	}
	t.Fatalf("no whole-directory artifact with prefix %q", prefix)
	return ""
}

// TestAbandonDirSwitchMissingBackupKeepsSibling pins round 13 finding I1:
// in the "swapped" state with the skills position absent, a backup that
// disappeared (external interference) must not silently destroy the
// sibling -- the only record that lets recovery complete the swap. The
// cleanup must refuse with a conflict and keep the sibling.
func TestAbandonDirSwitchMissingBackupKeepsSibling(t *testing.T) {
	parent := t.TempDir()
	skills := filepath.Join(parent, "skills")
	target := filepath.Join(parent, "real")
	if err := os.Symlink(target, skills); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(parent, ".fu-skills-old-x")
	sibling := filepath.Join(parent, ".fu-skills-x")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	// The archive rename ran (skills position absent) and the backup was
	// externally deleted; the sibling is intact.
	if err := os.Rename(skills, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
	txn := &TxnRecord{DirSwitch: &DirSwitchState{Agent: "claude", Target: target, Sibling: sibling, Backup: backup, Stage: "swapped"}}
	a := fakeAgent{"claude", skills}

	_, err := abandonDirSwitch(a, txn)
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("a missing backup must surface as a conflict, got %v", err)
	}
	// The sibling survives so recovery can complete the swap from it.
	if _, statErr := os.Stat(sibling); statErr != nil {
		t.Fatalf("the sibling must survive the refusal, err=%v", statErr)
	}
}

// TestAbandonDirSwitchRestoreFailureKeepsSibling pins round 14 finding I2:
// when the backup restore itself fails (here: a backup path whose
// intermediate component is a regular file, so Lstat returns ENOTDIR), the
// cleanup must keep the sibling -- the only record of the swap -- instead
// of dropping it after recording the error.
func TestAbandonDirSwitchRestoreFailureKeepsSibling(t *testing.T) {
	parent := t.TempDir()
	skills := filepath.Join(parent, "skills")
	target := filepath.Join(parent, "real")
	sibling := filepath.Join(parent, ".fu-skills-x")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	// The skills position is absent (archive ran) and the recorded backup
	// path is unusable: an intermediate component is a plain file.
	backup := filepath.Join(parent, "sub", ".fu-skills-old-x")
	if err := os.WriteFile(filepath.Join(parent, "sub"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	txn := &TxnRecord{DirSwitch: &DirSwitchState{Agent: "claude", Target: target, Sibling: sibling, Backup: backup, Stage: "swapped"}}
	a := fakeAgent{"claude", skills}

	if _, err := abandonDirSwitch(a, txn); err == nil {
		t.Fatal("a failed backup restore must surface as an error")
	}
	if _, statErr := os.Stat(sibling); statErr != nil {
		t.Fatalf("the sibling must survive a failed restore, err=%v", statErr)
	}
}

// TestAdoptWholeDirMissingBackupIsConflict pins the identity-bound recovery
// rule: once an external writer removes the recorded original-link backup,
// Fu must retain the WAL and replacement sibling instead of synthesizing
// ownership or completing the swap from pathnames alone.
func TestAdoptWholeDirMissingBackupIsConflict(t *testing.T) {
	fuHome, homeDir, _ := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", homeDir)
	h := hooks{afterDirSwitchSwap: func() error {
		// The archive rename ran; delete the archived link so the isolation
		// cleanup cannot restore it, and fail the switch.
		entries, _ := os.ReadDir(filepath.Join(homeDir, ".claude"))
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".fu-skills-old-") {
				if rmErr := os.Remove(filepath.Join(homeDir, ".claude", e.Name())); rmErr != nil {
					return rmErr
				}
			}
		}
		return errors.New("simulated whole-dir switch failure")
	}}

	res, err := adopt(s, []agent.Agent{agent.Claude{}}, "", h)
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("missing backup error = %v, want ErrTxnConflict", err)
	}
	if len(res.Adopted) != 0 {
		t.Fatalf("conflicted switch must not report completion, adopted = %+v", res.Adopted)
	}
	pending, perr := PendingTxns(s)
	if perr != nil {
		t.Fatal(perr)
	}
	if len(pending) != 1 {
		t.Fatalf("the transaction must stay open, pending = %+v", pending)
	}

	// A later write must surface the same conflict and preserve the sibling;
	// the missing ownership proof cannot be repaired automatically.
	if _, err := NewSkill(s, []agent.Agent{agent.Claude{}}, "beta"); !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("next write error = %v, want ErrTxnConflict", err)
	}
	skillsDir := filepath.Join(homeDir, ".claude", "skills")
	if _, lerr := os.Lstat(skillsDir); !os.IsNotExist(lerr) {
		t.Fatalf("skills position must remain absent, err=%v", lerr)
	}
	pending, perr = PendingTxns(s)
	if perr != nil {
		t.Fatal(perr)
	}
	if len(pending) != 1 {
		t.Fatalf("recovery must retain its WAL, got %+v", pending)
	}
}

// TestSwitchAdoptEntryRefusesWholeDirTarget pins round 18 finding I2: the
// per-entry switch must refuse a whole-directory target outright. Damage was
// avoided only incidentally -- pairBoundAdoptRoot compares against the
// descriptor for Dir(SkillsDir), so os.SameFile happened to fail -- and one
// refactor away the per-entry path would archive and delete the original from
// the user's target through the parent symlink (SPEC rule 10).
func TestSwitchAdoptEntryRefusesWholeDirTarget(t *testing.T) {
	fuHome, homeDir, _ := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	a := agent.Claude{}
	digest, err := digestDir(filepath.Join(homeDir, ".claude", "skills", "pdf-tools"))
	if err != nil {
		t.Fatal(err)
	}
	targets, err := captureAdoptTargets([]agent.Agent{a}, []agent.Agent{a}, "pdf-tools", digest)
	if err != nil {
		t.Fatal(err)
	}
	txn := &TxnRecord{Op: "adopt", Name: "pdf-tools", AdoptTargets: targets}

	err = switchAdoptEntry(s, a, "pdf-tools", txn)
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("per-entry switch of a whole-directory target must refuse with ErrTxnConflict, got %v", err)
	}
	if !strings.Contains(err.Error(), "whole-directory") {
		t.Fatalf("the refusal must name the whole-directory form, got %v", err)
	}
}

// TestIsolatedWholeDirAgentLeavesTxnAgents pins the other half of round 18
// finding I2: an isolated whole-directory agent must be dropped from
// txn.Agents as well as txn.WholeDirAgents. finishCommittedAdopt builds the
// per-entry list as record.Agents minus record.WholeDirAgents, so an agent
// left in the first list alone is handed to the per-entry switch on recovery
// -- the path adopt_txn.go documents as forbidden for whole-directory agents.
func TestIsolatedWholeDirAgentLeavesTxnAgents(t *testing.T) {
	fuHome, homeDir, _ := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	a := agent.Claude{}
	digest, err := digestDir(filepath.Join(homeDir, ".claude", "skills", "pdf-tools"))
	if err != nil {
		t.Fatal(err)
	}
	targets, err := captureAdoptTargets([]agent.Agent{a}, []agent.Agent{a}, "pdf-tools", digest)
	if err != nil {
		t.Fatal(err)
	}
	txn := &TxnRecord{
		Op: "adopt", Name: "pdf-tools", AdoptTargets: targets,
		Agents: []string{"claude"}, WholeDirAgents: []string{"claude"},
	}
	// An ordinary (non-conflict) failure at the build boundary: the agent is
	// isolated after abandonDirSwitch proves and removes fu's own artifacts.
	h := hooks{afterDirSwitchBuild: func() error { return errors.New("simulated build failure") }}

	if err := switchWholeDirAgentsWithHook(s, []agent.Agent{a}, "pdf-tools", txn, h); err != nil {
		t.Fatalf("an ordinary whole-directory failure must isolate, not propagate: %v", err)
	}
	if len(txn.WholeDirAgents) != 0 {
		t.Fatalf("WholeDirAgents = %v; want the isolated agent removed", txn.WholeDirAgents)
	}
	if len(txn.Agents) != 0 {
		t.Fatalf("Agents = %v; want the isolated whole-directory agent removed so recovery cannot route it through the per-entry switch", txn.Agents)
	}
}

func TestCompletedDirSwitchAbandonPersistsClearedState(t *testing.T) {
	fuHome, homeDir, _ := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	a := agent.Claude{}
	digest, err := digestDir(filepath.Join(homeDir, ".claude", "skills", "pdf-tools"))
	if err != nil {
		t.Fatal(err)
	}
	targets, err := captureAdoptTargets([]agent.Agent{a}, []agent.Agent{a}, "pdf-tools", digest)
	if err != nil {
		t.Fatal(err)
	}
	txn := &TxnRecord{
		Op: "adopt", Name: "pdf-tools", AdoptTargets: targets,
		Agents: []string{"claude"}, WholeDirAgents: []string{"claude"},
	}
	stop := errors.New("stop after replacement landing")
	h := hooks{afterDirSwitchLand: func() error { return stop }}

	if err := switchWholeDirAgentsWithHook(s, []agent.Agent{a}, "pdf-tools", txn, h); err != nil {
		t.Fatalf("landed switch must finish through abandon: %v", err)
	}
	if txn.DirSwitch != nil {
		t.Fatalf("in-memory switch state must be cleared: %+v", txn.DirSwitch)
	}
	pending, err := PendingTxns(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].DirSwitch != nil {
		t.Fatalf("durable WAL must record the cleared switch before returning: %+v", pending)
	}
}

func TestReclaimUnjournalledDirSwitchSiblingResumesAfterRetirement(t *testing.T) {
	parentPath := t.TempDir()
	parent, err := os.Open(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	name := ".fu-skills-building"
	if err := os.Mkdir(filepath.Join(parentPath, name), 0o755); err != nil {
		t.Fatal(err)
	}
	retired := unjournalledDirSwitchRetiredName(name)
	if err := os.Rename(filepath.Join(parentPath, name), filepath.Join(parentPath, retired)); err != nil {
		t.Fatal(err)
	}
	if err := reclaimUnjournalledDirSwitchSibling(parent, parentPath, name); err != nil {
		t.Fatalf("resume reclaim: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(parentPath, retired)); !os.IsNotExist(err) {
		t.Fatalf("retired residue still exists: %v", err)
	}
}

func TestReclaimUnjournalledDirSwitchSiblingDoesNotRestoreEarlierRetirement(t *testing.T) {
	parentPath := t.TempDir()
	parent, err := os.Open(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	name := ".fu-skills-building"
	retired := unjournalledDirSwitchRetiredName(name)
	if err := os.Mkdir(filepath.Join(parentPath, retired), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parentPath, retired, "foreign"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = reclaimUnjournalledDirSwitchSibling(parent, parentPath, name)
	if err == nil {
		t.Fatal("non-empty retired residue must be preserved and reported")
	}
	if _, statErr := os.Lstat(filepath.Join(parentPath, name)); !os.IsNotExist(statErr) {
		t.Fatalf("a resumed cleanup must not restore a name it did not retire: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(parentPath, retired, "foreign")); statErr != nil {
		t.Fatalf("retired residue was not preserved: %v", statErr)
	}
	if !strings.Contains(err.Error(), filepath.Join(parentPath, retired)) {
		t.Fatalf("error must name the preserved retired path: %v", err)
	}
}

func TestSwappedDirSwitchRestoresLinkWhenReplacementSiblingIsDeleted(t *testing.T) {
	fuHome, homeDir, _ := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	skillsLink := filepath.Join(homeDir, ".claude", "skills")
	fired := 0
	res, err := adopt(s, []agent.Agent{agent.Claude{}}, "", hooks{
		afterDirSwitchSwap: func() error {
			fired++
			entries, err := os.ReadDir(filepath.Dir(skillsLink))
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".fu-skills-") && !strings.HasPrefix(entry.Name(), ".fu-skills-old-") {
					return os.RemoveAll(filepath.Join(filepath.Dir(skillsLink), entry.Name()))
				}
			}
			return errors.New("replacement sibling not found")
		},
	})
	if fired == 0 {
		t.Fatal("post-swap hook did not run")
	}
	if err != nil {
		t.Fatalf("missing replacement must restore the original link and isolate the agent: %v", err)
	}
	info, statErr := os.Lstat(skillsLink)
	if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("original skills link was not restored: %v %v", info, statErr)
	}
	if len(res.Pending) != 1 || len(res.Pending[0].Agents) != 0 {
		t.Fatalf("isolated whole-directory agent was not reported as pending: %+v", res)
	}
}

// TestResumeDirSwitchReclaimsUnjournalledSibling pins round 18 finding I1.
// startDirSwitch persists the sibling *name*, then Mkdirs it, then persists
// its identity. A crash in that one-syscall window leaves .fu-skills-<hex> on
// disk with no recorded inode: the "building" resume cannot take the ENOENT
// restart branch and hits the missing-identity refusal, so finishCommittedAdopt,
// RecoverPending and every subsequent write command fail until the user deletes
// the directory by hand. Because nothing is ever created inside the sibling
// before its identity is journalled, an *empty* directory at that name is
// provably fu's own residue and must be reclaimed rather than refused.
func TestResumeDirSwitchReclaimsUnjournalledSibling(t *testing.T) {
	fuHome, homeDir, _ := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	a := agent.Claude{}
	digest, err := digestDir(filepath.Join(homeDir, ".claude", "skills", "pdf-tools"))
	if err != nil {
		t.Fatal(err)
	}
	targets, err := captureAdoptTargets([]agent.Agent{a}, []agent.Agent{a}, "pdf-tools", digest)
	if err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(homeDir, ".claude", ".fu-skills-0123456789abcdef")
	if err := os.Mkdir(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	txn := &TxnRecord{
		Op: "adopt", Name: "pdf-tools", AdoptTargets: targets,
		Agents: []string{"claude"}, WholeDirAgents: []string{"claude"},
		DirSwitch: &DirSwitchState{
			Agent: "claude", Target: targets[0].SourcePath,
			Sibling: sibling, CleanupID: "0011223344556677", Stage: "building",
		},
	}

	if err := resumeDirSwitch(s, a, "pdf-tools", txn, hooks{}); err != nil {
		t.Fatalf("an empty unjournalled sibling must be reclaimed, not wedge every write command: %v", err)
	}
	if _, err := os.Lstat(sibling); !os.IsNotExist(err) {
		t.Fatalf("the reclaimed sibling name must be free, got %v", err)
	}
}

func TestAbandonDirSwitchReclaimsUnjournalledSibling(t *testing.T) {
	_, homeDir, _ := wholeDirFixture(t)
	a := agent.Claude{}
	digest, err := digestDir(filepath.Join(homeDir, ".claude", "skills", "pdf-tools"))
	if err != nil {
		t.Fatal(err)
	}
	targets, err := captureAdoptTargets([]agent.Agent{a}, []agent.Agent{a}, "pdf-tools", digest)
	if err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(homeDir, ".claude", ".fu-skills-0123456789abcdef")
	if err := os.Mkdir(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	skillsDir := filepath.Join(homeDir, ".claude", "skills")
	if err := os.Remove(skillsDir); err != nil {
		t.Fatal(err)
	}
	replacementTarget := t.TempDir()
	if err := os.Symlink(replacementTarget, skillsDir); err != nil {
		t.Fatal(err)
	}
	txn := &TxnRecord{
		Op: "adopt", Name: "pdf-tools", AdoptTargets: targets,
		Agents: []string{"claude"}, WholeDirAgents: []string{"claude"},
		DirSwitch: &DirSwitchState{
			Agent: "claude", Target: targets[0].SourcePath,
			Sibling: sibling, CleanupID: "0011223344556677", Stage: "building",
		},
	}

	completed, err := abandonDirSwitch(a, txn)
	if err != nil {
		t.Fatalf("abandon must reclaim an empty unjournalled sibling: %v", err)
	}
	if completed {
		t.Fatal("reclaiming a building artifact must report an abandoned, not completed, switch")
	}
	if _, err := os.Lstat(sibling); !os.IsNotExist(err) {
		t.Fatalf("the unjournalled sibling must be reclaimed, got %v", err)
	}
	if got, err := os.Readlink(skillsDir); err != nil || got != replacementTarget {
		t.Fatalf("the user's replacement link must stay untouched, got %q err=%v", got, err)
	}
}

// TestResumeDirSwitchRefusesNonEmptyUnjournalledSibling is the other half of
// the same rule: only an empty directory is provably fu's residue. Anything
// with content was not created in the Mkdir-to-journal window, so it is
// foreign state and must be preserved behind a conflict.
func TestResumeDirSwitchRefusesNonEmptyUnjournalledSibling(t *testing.T) {
	fuHome, homeDir, _ := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	a := agent.Claude{}
	digest, err := digestDir(filepath.Join(homeDir, ".claude", "skills", "pdf-tools"))
	if err != nil {
		t.Fatal(err)
	}
	targets, err := captureAdoptTargets([]agent.Agent{a}, []agent.Agent{a}, "pdf-tools", digest)
	if err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(homeDir, ".claude", ".fu-skills-0123456789abcdef")
	if err := os.Mkdir(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "foreign"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	txn := &TxnRecord{
		Op: "adopt", Name: "pdf-tools", AdoptTargets: targets,
		Agents: []string{"claude"}, WholeDirAgents: []string{"claude"},
		DirSwitch: &DirSwitchState{
			Agent: "claude", Target: targets[0].SourcePath,
			Sibling: sibling, CleanupID: "0011223344556677", Stage: "building",
		},
	}

	err = resumeDirSwitch(s, a, "pdf-tools", txn, hooks{})
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("a non-empty unjournalled sibling must refuse with ErrTxnConflict, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(sibling, "foreign")); statErr != nil {
		t.Fatalf("foreign content must be preserved: %v", statErr)
	}
}

// TestLandedDirSwitchToleratesTargetSkillEdit pins round 18 finding I6. Once
// the sibling has landed, ~/.claude/skills is fu's replacement directory and
// the adopted skill is served from the store; the target's own copy is no
// longer read by the agent. Re-digesting it after landing turned a legitimate
// user edit into a permanent wedge -- ErrTxnConflict on every write command
// until the file was restored byte for byte. The target *manifest* must still
// be validated, because the sibling's passthrough links mirror it.
func TestLandedDirSwitchToleratesTargetSkillEdit(t *testing.T) {
	fuHome, homeDir, target := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	agents := []agent.Agent{agent.Claude{}}
	// Stop inside the backup removal, which runs after the replacement has
	// landed and the "done" revision is persisted: the state the finding
	// describes, left behind with the WAL still open. The hook fails on every
	// call so the abandon path cannot quietly finish the switch instead;
	// recovery below runs with no hooks at all.
	stop := errors.New("stop after landing")
	if _, err := adopt(s, agents, "", hooks{
		beforeDirSwitchBackupRetire: func(string) error { return stop },
	}); err == nil {
		t.Fatal("the injected stop must surface")
	}
	// The user, seeing their skills directory has changed shape, edits the
	// adopted skill in its original home.
	edited := filepath.Join(target, "pdf-tools", "SKILL.md")
	if err := os.WriteFile(edited, []byte("---\nname: pdf-tools\ndescription: edited by the user\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The next write command runs the mandatory recovery prologue. It must
	// finish the landed switch, not refuse it.
	if _, err := SetGlobal(s, agents, "pdf-tools", false); err != nil {
		t.Fatalf("a user edit to the no-longer-served target copy must not wedge every write command: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(homeDir, ".claude"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".fu-skills-old-") {
			t.Fatalf("the backup link must be removed once recovery completes: %s", e.Name())
		}
	}
	// The switch itself completed: the skills position is fu's real directory,
	// not the user's original symlink.
	info, err := os.Lstat(filepath.Join(homeDir, ".claude", "skills"))
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("the landed replacement directory must remain: %v %v", info, err)
	}
	if records, readErr := PendingTxns(s); readErr != nil || len(records) != 0 {
		t.Fatalf("recovery must close the transaction: %+v, %v", records, readErr)
	}
}

// TestAdoptWholeDirRootSkillIsolatesOneAgent pins round 18 finding I12.
// DESIGN §6 says the root-SKILL.md refusal 以可操作诊断拒绝整个 agent,
// 保持原链接与目标不变 -- one agent, not the run. In practice the refusal
// returned out of scanAdoptEntries and stopped the command before any agent
// was processed, so a user with claude on such a symlinked directory and codex
// on an ordinary one full of adoptable skills adopted nothing at all. That also
// contradicts DESIGN §6 (任一 skill 失败不影响其他项).
func TestAdoptWholeDirRootSkillIsolatesOneAgent(t *testing.T) {
	fuHome := t.TempDir()
	if _, err := store.Init(fuHome); err != nil {
		t.Fatal(err)
	}
	homeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(homeDir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	// claude: a symlinked skills directory whose target is itself a skill --
	// and which also holds a child named like the skill codex supplies. That
	// child is what makes the refusal reachable a second time: hasEntry follows
	// the parent symlink, so without it the classification loop never re-admits
	// claude and the capture-time copy of the refusal is invisible to the test.
	// It is also the shared-skill case adopt exists for.
	rootSkill := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootSkill, "SKILL.md"), []byte("---\nname: rooted\ndescription: d\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, rootSkill, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	if err := os.Symlink(rootSkill, filepath.Join(homeDir, ".claude", "skills")); err != nil {
		t.Fatal(err)
	}
	// codex: an ordinary directory holding an adoptable skill.
	codexDir := filepath.Join(homeDir, ".codex", "skills")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillTree(t, codexDir, "pdf-tools", "---\nname: pdf-tools\ndescription: d\n---\n")
	t.Setenv("FU_HOME", fuHome)
	t.Setenv("HOME", homeDir)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Adopt(s, []agent.Agent{agent.Claude{}, agent.Codex{}}, "")
	if err != nil {
		t.Fatalf("one agent's refusal must not abort the run: %v", err)
	}
	if len(res.Adopted) != 1 || res.Adopted[0].Name != "pdf-tools" {
		t.Fatalf("the healthy agent's skill must still be adopted: %+v", res.Adopted)
	}
	reported := false
	for _, f := range res.Failed {
		if strings.Contains(f.Err.Error(), "root SKILL.md") {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("the refused agent must be reported with an actionable diagnostic: %+v", res.Failed)
	}
	// The refused agent's link and target are untouched.
	info, err := os.Lstat(filepath.Join(homeDir, ".claude", "skills"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the refused agent's link must stay a symlink: %v %v", info, err)
	}
}

// replaceChildByRename replaces a direct child of a whole-directory target the
// way an ordinary tool does: write a new object beside it and rename over the
// old name. The bytes may even be identical -- what changes is the inode, and
// that is the point. `git pull`, an editor's atomic save and rsync all do this.
func replaceChildByRename(t *testing.T, target, name string) {
	t.Helper()
	tmp := filepath.Join(target, ".incoming-"+name)
	if err := os.WriteFile(tmp, []byte("replaced by an ordinary tool"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(target, name)); err != nil {
		t.Fatal(err)
	}
}

func replaceDirectoryWithCopy(t *testing.T, target string) {
	t.Helper()
	staged := target + ".incoming"
	if err := os.CopyFS(staged, os.DirFS(target)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, target); err != nil {
		t.Fatal(err)
	}
}

// TestBuildingDirSwitchIsolatesRecreatedTarget pins the pre-archive half of
// target-conflict recovery. The replacement sibling is fu-owned, but the
// user's skills link has not moved at stage building. Recreating the target
// with identical bytes changes an inode that can never be restored; retaining
// the WAL therefore wedges every later write command instead of protecting
// user data.
func TestBuildingDirSwitchIsolatesRecreatedTarget(t *testing.T) {
	fuHome, homeDir, target := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	a := agent.Claude{}
	digest, err := digestDir(filepath.Join(target, "pdf-tools"))
	if err != nil {
		t.Fatal(err)
	}
	targets, err := captureAdoptTargets([]agent.Agent{a}, []agent.Agent{a}, "pdf-tools", digest)
	if err != nil {
		t.Fatal(err)
	}
	txn := &TxnRecord{
		Op: "adopt", Name: "pdf-tools", AdoptTargets: targets,
		Agents: []string{"claude"}, WholeDirAgents: []string{"claude"},
	}
	h := hooks{afterDirSwitchBuild: func() error {
		replaceDirectoryWithCopy(t, target)
		return nil
	}}

	if err := switchWholeDirAgentsWithHook(s, []agent.Agent{a}, "pdf-tools", txn, h); err != nil {
		t.Fatalf("a recreated user target before archive must isolate the agent: %v", err)
	}
	if txn.DirSwitch != nil || len(txn.Agents) != 0 || len(txn.WholeDirAgents) != 0 {
		t.Fatalf("the failed agent must reach a terminal isolated state: %+v", txn)
	}
	info, err := os.Lstat(filepath.Join(homeDir, ".claude", "skills"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the user's untouched skills link must remain: %v %v", info, err)
	}
}

// TestAdoptContinuesAfterCaptureConflict pins the preflight marker used by
// the multi-candidate loop. A whole-directory target can be replaced after
// its source descriptor is opened but before it is paired to the pathname;
// that candidate has no WAL yet, so the conflict is local and must not stop
// later candidates.
func TestAdoptContinuesAfterCaptureConflict(t *testing.T) {
	fuHome, _, target := wholeDirFixture(t)
	writeSkillTree(t, target, "zeta", "---\nname: zeta\ndescription: d\n---\n")
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	fired := false
	h := hooks{beforeAdoptSourcePair: func() error {
		if !fired {
			fired = true
			replaceDirectoryWithCopy(t, target)
		}
		return nil
	}}

	res, err := adopt(s, []agent.Agent{agent.Claude{}}, "", h)
	if err != nil {
		t.Fatalf("a capture-time conflict must be isolated to its candidate: %v", err)
	}
	if len(res.PreflightConflicts) != 1 || res.PreflightConflicts[0].Action.Skill != "pdf-tools" || !errors.Is(res.PreflightConflicts[0].Err, ErrTxnConflict) {
		t.Fatalf("preflight conflicts = %+v; want only pdf-tools with ErrTxnConflict", res.PreflightConflicts)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("a capture race is not invalid skill content: %+v", res.Failed)
	}
	if len(res.Adopted) != 1 || res.Adopted[0].Name != "zeta" {
		t.Fatalf("later candidate must still be adopted, got %+v", res.Adopted)
	}
}

// TestLandedDirSwitchToleratesReplacedTargetChild is the identity half of the
// finding TestLandedDirSwitchToleratesTargetSkillEdit covers for digests. That
// test edits a *nested* file in place, so no direct child ever changes inode
// and the identity comparison it leaves behind cannot be seen. Replace a direct
// child by rename instead -- what `git pull` does -- and the landed re-entry
// refused permanently: the bytes can be restored, the inode cannot, so no user
// action could ever clear it.
func TestLandedDirSwitchToleratesReplacedTargetChild(t *testing.T) {
	fuHome, homeDir, target := wholeDirFixture(t)
	s, err := store.Open(fuHome)
	if err != nil {
		t.Fatal(err)
	}
	agents := []agent.Agent{agent.Claude{}}
	stop := errors.New("stop after landing")
	if _, err := adopt(s, agents, "", hooks{
		beforeDirSwitchBackupRetire: func(string) error { return stop },
	}); err == nil {
		t.Fatal("the injected stop must surface")
	}
	replaceChildByRename(t, target, "notes.txt")

	if _, err := SetGlobal(s, agents, "pdf-tools", false); err != nil {
		t.Fatalf("replacing a child fu does not own must not wedge every write command: %v", err)
	}
	info, err := os.Lstat(filepath.Join(homeDir, ".claude", "skills"))
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("the landed replacement directory must remain: %v %v", info, err)
	}
	if records, readErr := PendingTxns(s); readErr != nil || len(records) != 0 {
		t.Fatalf("recovery must close the transaction: %+v, %v", records, readErr)
	}
}

// TestSwappedDirSwitchRestoresLinkOnTargetChange covers the severe sub-case:
// the parent link is already archived, so the agent has *no* skills directory,
// and validation then refuses.
//
// The hook has to be afterDirSwitchSwap, not beforeDirSwitchArchive. The
// earlier version used the latter and documented itself as reaching the
// archived state, but completeDirSwitch's link-present arm validates the
// target *before* it archives, so the refusal fired with the link still in
// place and the assertion "the archived skills link must be restored" passed
// because nothing had been moved. afterDirSwitchSwap fires after the archive
// rename and after the backup is validated, which is the state this fix
// exists for. Testing the adjacent arm is how three of the four failure modes
// below stayed unreachable for a round.
//
// All three mutations are ordinary: adding a child (name set), editing the
// adopted skill (digest), and re-creating the directory (inode -- what a
// re-clone, a restore from backup, or an rsync into place produces). Only the
// first was tagged as a target conflict; the other two returned before the
// abandon and left ~/.claude/skills missing forever, the inode one
// unrecoverable by any user action.
func TestSwappedDirSwitchRestoresLinkOnTargetChange(t *testing.T) {
	mutations := map[string]func(t *testing.T, target string){
		"name set: a child appears": func(t *testing.T, target string) {
			if err := os.WriteFile(filepath.Join(target, "late.txt"), []byte("late\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"digest: the adopted skill is edited": func(t *testing.T, target string) {
			body := []byte("---\nname: pdf-tools\ndescription: edited mid-switch\n---\n")
			if err := os.WriteFile(filepath.Join(target, "pdf-tools", "SKILL.md"), body, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"inode: the target is re-created in place": func(t *testing.T, target string) {
			replaceDirectoryWithCopy(t, target)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			fuHome, homeDir, target := wholeDirFixture(t)
			s, err := store.Open(fuHome)
			if err != nil {
				t.Fatal(err)
			}
			skillsLink := filepath.Join(homeDir, ".claude", "skills")
			fired := 0
			h := hooks{afterDirSwitchSwap: func() error {
				fired++
				// The archive rename has happened: the agent has no skills
				// directory at this instant. Prove it, so the test cannot
				// silently drift back to testing the adjacent arm.
				if _, statErr := os.Lstat(skillsLink); !os.IsNotExist(statErr) {
					t.Fatalf("the hook must fire with the link archived, got %v", statErr)
				}
				mutate(t, target)
				return nil
			}}
			res, err := adopt(s, []agent.Agent{agent.Claude{}}, "", h)
			if fired == 0 {
				t.Fatal("the post-archive hook never fired; the test proves nothing")
			}
			if err != nil {
				t.Fatalf("the agent must be isolated, not the run aborted: %v", err)
			}
			info, statErr := os.Lstat(skillsLink)
			if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("the archived skills link must be restored, got %v %v", info, statErr)
			}
			if len(res.Pending) != 1 || len(res.Pending[0].Agents) != 0 {
				t.Fatalf("the committed-but-unswitched skill must be reported as pending: %+v", res)
			}
			if records, readErr := PendingTxns(s); readErr != nil || len(records) != 0 {
				t.Fatalf("the transaction must reach a terminal state: %+v, %v", records, readErr)
			}
			if _, err := SetGlobal(s, []agent.Agent{agent.Claude{}}, "pdf-tools", false); err != nil {
				t.Fatalf("later write commands must not be wedged: %v", err)
			}
		})
	}
}
