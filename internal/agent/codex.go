package agent

import (
	"os"
	"path/filepath"
)

type Codex struct{}

func (Codex) Name() string { return "codex" }

// Detect reports false when HOME is unset or relative; see homeDir and
// Claude.Detect (finding I6, widened by round 5's finding).
func (Codex) Detect() bool {
	home := homeDir()
	if home == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(home, ".codex"))
	return err == nil
}

// SkillsDir returns "" when HOME is unset or relative, rather than
// degrading to a path relative to the process's current working directory;
// see homeDir and Claude.SkillsDir (finding I6, widened by round 5's
// finding).
func (Codex) SkillsDir() string {
	home := homeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".codex", "skills")
}

func (Codex) Reserved() []string { return []string{".system"} }
