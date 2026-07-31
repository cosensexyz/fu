package agent

import (
	"os"
	"path/filepath"
)

type Claude struct{}

func (Claude) Name() string { return "claude" }

// Detect reports false when HOME is unusable -- unset (finding I6) or
// relative (round 5 finding); see homeDir for both. SkillsDir has no error
// channel to report the same condition (see SkillsDir's comment), so
// refusing detection here is what keeps an agent with no resolvable home
// directory from ever reaching the scan/reconcile paths at all through the
// normal agent.Detected() flow.
func (Claude) Detect() bool {
	home := homeDir()
	if home == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(home, ".claude"))
	return err == nil
}

// SkillsDir returns "" for a HOME that is unset or relative (see homeDir),
// rather than degrading to ".claude/skills" or "./.claude/skills" -- a
// path resolved against the process's current working directory (finding
// I6, widened by round 5's finding). Reproduced against the compiled
// binary, in both spellings: `env -u HOME FU_HOME=... fu new alpha` and
// `env HOME=. fu new alpha`, run from a project directory containing its
// own ./.claude, created a link at <cwd>/.claude/skills/alpha and treated
// the project's own Claude Code config as a global agent installation.
// store.Home returns an explicit "HOME not set" error for the unset case;
// this interface's SkillsDir() string has no error channel, so "" plus the
// scan/reconcile refusal in engine.ScanAgent is the fix available without
// changing the Agent interface itself.
func (Claude) SkillsDir() string {
	home := homeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "skills")
}

func (Claude) Reserved() []string { return nil }
