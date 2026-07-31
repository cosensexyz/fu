package agent

import (
	"os"
	"path/filepath"
)

// homeDir returns HOME when it is usable as the base of an agent's own
// config directory, and "" otherwise. Every adapter goes through this, so
// the rule lives in one place rather than being restated -- and eventually
// mis-restated -- once per agent.
//
// Usable means set *and absolute* (round 5 finding, widening finding I6's
// HOME=="" guard). A relative HOME is not empty, so it sailed past the
// narrower guard and got joined into ".claude/skills" all the same: the
// cwd-relative path that guard exists to refuse. Reproduced against the
// compiled binary: from a project directory carrying its own ./.claude and
// ./.codex, `env HOME=. fu new alpha` wrote links into the project's own
// agent config directories and treated them as a global installation.
//
// Detect is gated on the same rule, not just SkillsDir: it stats
// filepath.Join(home, ".claude"), so a relative HOME makes it report a
// project's own directory as an installed agent, and refusing detection is
// what keeps an agent with no resolvable home out of the scan/reconcile
// paths entirely.
func homeDir() string {
	home := os.Getenv("HOME")
	if home == "" || !filepath.IsAbs(home) {
		return ""
	}
	return home
}

// Agent abstracts one supported AI coding agent installation (DESIGN §5).
type Agent interface {
	Name() string       // stable id: "claude", "codex"
	Detect() bool       // installation marker path exists
	SkillsDir() string  // directory fu materializes links into
	Reserved() []string // entries never managed nor adopted (SPEC rule 11)
}

// All lists every known adapter; adding an agent means one new file plus
// one line here.
func All() []Agent { return []Agent{Claude{}, Codex{}} }

// Detected filters All to installed agents (detected == managed,
// SPEC rule 4).
func Detected() []Agent {
	var out []Agent
	for _, a := range All() {
		if a.Detect() {
			out = append(out, a)
		}
	}
	return out
}

func ByName(name string) (Agent, bool) {
	for _, a := range All() {
		if a.Name() == name {
			return a, true
		}
	}
	return nil, false
}
