// cmd/fu/main_test.go
package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestBinarySmoke drives the actual compiled fu binary as a real user would
// invoke it from a shell: a separate OS process, real argv parsing, real
// command registration. Every other test in this module (cli, engine,
// store) calls Go functions directly -- NewRootCmd, engine.Run, store.Init
// -- and never runs main() itself, so flag parsing, command registration,
// and the process exit-code wiring in main.go have never actually been
// exercised. This test closes that gap: it builds the binary fresh into a
// throwaway temp directory (always outside the repository, via
// t.TempDir(), so it can never be committed), then runs a short
// init -> new -> list sequence through it with FU_HOME and HOME pointed at
// temporary directories, asserting on the process's real exit status and
// real stdout/stderr -- plus failure cases proving the os.Exit(1) and
// os.Exit(2) paths also fire, not just the success paths (finding I7:
// DESIGN §7's exit-code contract -- 0 success, 1 operation failure, 2
// usage error -- was entirely unimplemented; every failure returned 1).
func TestBinarySmoke(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "fu")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}

	// go test's working directory for this package is cmd/fu itself, so
	// "." below is the fu command's own package.
	build := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fu binary: %v\n%s", err, out)
	}

	fuHome, home := t.TempDir(), t.TempDir()
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "FU_HOME=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+home, "FU_HOME="+fuHome)

	run := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(binPath, args...)
		cmd.Env = env
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		if err == nil {
			return out.String(), 0
		}
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run fu %v: %v", args, err)
		}
		return out.String(), exitErr.ExitCode()
	}

	if out, code := run("init"); code != 0 || !strings.Contains(out, "initialized store") {
		t.Fatalf("fu init: exit=%d out=%q", code, out)
	}
	if out, code := run("new", "writer"); code != 0 || !strings.Contains(out, "created writer") {
		t.Fatalf("fu new writer: exit=%d out=%q", code, out)
	}
	if _, err := os.Stat(filepath.Join(fuHome, "store", "skills", "writer", "SKILL.md")); err != nil {
		t.Fatalf("skill not scaffolded on disk by the real binary: %v", err)
	}
	if out, code := run("list"); code != 0 || !strings.Contains(out, "writer") || !strings.Contains(out, "SKILL") {
		t.Fatalf("fu list: exit=%d out=%q", code, out)
	}

	// Negative path: main.go's os.Exit(1) must actually fire for a real
	// operation failure, not merely leave the success paths looking correct.
	if out, code := run("new", "writer"); code != 1 || !strings.Contains(out, "error:") {
		t.Fatalf("fu new writer (duplicate) must exit 1 with an error message: exit=%d out=%q", code, out)
	}

	// Usage-error path (finding I7): an unrecognized subcommand and a
	// missing required argument must both exit 2, distinct from the
	// operation failure's exit 1 just above. Reproduced against the
	// compiled binary pre-fix: both cases printed an error and exited 1,
	// indistinguishable from "fu new writer" above.
	if out, code := run("bogus-command"); code != 2 {
		t.Fatalf("fu bogus-command (unknown subcommand) must exit 2: exit=%d out=%q", code, out)
	}
	if out, code := run("show"); code != 2 {
		t.Fatalf("fu show (missing argument) must exit 2: exit=%d out=%q", code, out)
	}

	// Round 2 finding 1 (test blind spot: round 2 finding 2), confirmed here
	// against the real compiled binary exactly as the reviewer reproduced
	// it: help, completion, and __complete must exit 0, not be
	// misclassified as an unknown command alongside bogus-command above.
	// __complete in particular is not a convenience alias -- it is the
	// literal command every generated shell completion script invokes on
	// every Tab press, so this exiting non-zero means interactive
	// completion is entirely dead, not merely degraded.
	if out, code := run("help"); code != 0 {
		t.Fatalf("fu help must exit 0: exit=%d out=%q", code, out)
	}
	if out, code := run("help", "new"); code != 0 || !strings.Contains(out, "new") {
		t.Fatalf("fu help new must exit 0 and describe the named subcommand: exit=%d out=%q", code, out)
	}
	if out, code := run("completion", "bash"); code != 0 || !strings.Contains(out, "bash completion") {
		t.Fatalf("fu completion bash must exit 0 and emit a completion script: exit=%d out=%q", code, out)
	}
	if out, code := run("__complete", "new", ""); code != 0 {
		t.Fatalf("fu __complete must exit 0: exit=%d out=%q", code, out)
	}
	if out, code := run(); code != 0 || !strings.Contains(out, "skill manager") {
		t.Fatalf("bare fu must exit 0 and print help: exit=%d out=%q", code, out)
	}
}
