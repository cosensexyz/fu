package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestFileHandlesRequiredTracksTheEnvironment(t *testing.T) {
	t.Setenv("FU_REQUIRE_FILE_HANDLES", "")
	if FileHandlesRequired() {
		t.Fatal("empty FU_REQUIRE_FILE_HANDLES must not require handles")
	}
	t.Setenv("FU_REQUIRE_FILE_HANDLES", "1")
	if !FileHandlesRequired() {
		t.Fatal("FU_REQUIRE_FILE_HANDLES=1 must require handles")
	}
	t.Setenv("FU_REQUIRE_FILE_HANDLES", "0")
	if FileHandlesRequired() {
		t.Fatal("FU_REQUIRE_FILE_HANDLES=0 must not require handles")
	}
}

func TestLinuxWorkflowRequiresFileHandleCoverage(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate repository root")
	}
	workflow := filepath.Join(filepath.Dir(currentFile), "..", "..", ".github", "workflows", "test.yml")
	raw, err := os.ReadFile(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if err := workflowFileHandleCoverageError(raw); err != nil {
		t.Fatal(err)
	}
}

func workflowFileHandleCoverageError(raw []byte) error {
	var parsed struct {
		Jobs map[string]struct {
			Name   string            `yaml:"name"`
			RunsOn string            `yaml:"runs-on"`
			Env    map[string]string `yaml:"env"`
			Steps  []struct {
				ID               string            `yaml:"id"`
				Run              string            `yaml:"run"`
				Env              map[string]string `yaml:"env"`
				If               *string           `yaml:"if"`
				ContinueOnError  string            `yaml:"continue-on-error"`
				WorkingDirectory string            `yaml:"working-directory"`
			} `yaml:"steps"`
			Strategy struct {
				Matrix struct {
					Other   map[string]any `yaml:",inline"`
					Include []struct {
						OS       string  `yaml:"os"`
						Required *string `yaml:"require_file_handles"`
					} `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("parse workflow YAML: %w", err)
	}
	job, exists := parsed.Jobs["test"]
	if !exists || len(job.Strategy.Matrix.Include) == 0 {
		return fmt.Errorf("test matrix must explicitly declare each runner's file-handle requirement")
	}
	if len(job.Strategy.Matrix.Other) != 0 {
		return fmt.Errorf("test matrix must use include only; additional axes can create uncovered runners")
	}
	if expressionBody(job.RunsOn) != "matrix.os" {
		return fmt.Errorf("test runs-on must bind to matrix.os")
	}
	if !strings.HasPrefix(job.Name, "test (") || !strings.HasSuffix(job.Name, ")") || expressionBody(strings.TrimSuffix(strings.TrimPrefix(job.Name, "test ("), ")")) != "matrix.os" {
		return fmt.Errorf("test job must preserve its test (runner) check names")
	}
	if got := job.Env["FU_REQUIRE_FILE_HANDLES"]; expressionBody(got) != "matrix.require_file_handles" {
		return fmt.Errorf("file-handle environment must use the declared matrix field, got %q", got)
	}
	linux := 0
	for _, entry := range job.Strategy.Matrix.Include {
		if entry.Required == nil {
			return fmt.Errorf("runner %q does not declare its file-handle requirement", entry.OS)
		}
		if strings.HasPrefix(entry.OS, "ubuntu") {
			linux++
			if *entry.Required != "1" {
				return fmt.Errorf("Linux runner %q must require file handles", entry.OS)
			}
		} else if strings.HasPrefix(entry.OS, "macos") && *entry.Required != "" {
			return fmt.Errorf("macOS runner %q must not require Linux file handles", entry.OS)
		}
	}
	if linux == 0 {
		return fmt.Errorf("no Linux file-handle coverage declared")
	}
	suites := 0
	for _, step := range job.Steps {
		if step.ID != "race-tests" {
			continue
		}
		suites++
		if _, overridden := step.Env["FU_REQUIRE_FILE_HANDLES"]; overridden {
			return fmt.Errorf("race-tests must inherit the job's file-handle requirement")
		}
		if step.If != nil && strings.TrimSpace(*step.If) != "success()" && expressionBody(*step.If) != "success()" {
			return fmt.Errorf("race-tests must run after successful setup")
		}
		if step.ContinueOnError != "" && step.ContinueOnError != "false" {
			return fmt.Errorf("race-tests failures must fail CI")
		}
		if step.WorkingDirectory != "" && step.WorkingDirectory != "." {
			return fmt.Errorf("race-tests must cover packages from the repository root")
		}
		if !fullRaceTestCommand(step.Run) {
			return fmt.Errorf("race-tests must run an unfiltered, uncached go test ./... with -race")
		}
	}
	if suites != 1 {
		return fmt.Errorf("workflow must declare exactly one race-tests step")
	}
	return nil
}

func TestWorkflowFileHandleCoverageSemantics(t *testing.T) {
	const valid = `jobs:
  test:
    name: test (${{ matrix.os }})
    runs-on: ${{ matrix.os }}
    env:
      FU_REQUIRE_FILE_HANDLES: ${{ matrix.require_file_handles }}
    strategy:
      matrix:
        include:
          - os: ubuntu-latest
            require_file_handles: '1'
          - os: macos-latest
            require_file_handles: ''
    steps:
      - id: race-tests
        run: go test ./... -race -count=1 -timeout 30m
`
	for _, test := range []struct {
		name, from, to string
		wantError      bool
	}{
		{"canonical", "", "", false},
		{"expression whitespace", "${{ matrix.require_file_handles }}", "${{matrix.require_file_handles}}", false},
		{"runner expression whitespace", "${{ matrix.os }}", "${{matrix.os}}", false},
		{"numeric scalar", "require_file_handles: '1'", "require_file_handles: 1", false},
		{"fixed runner", "runs-on: ${{ matrix.os }}", "runs-on: macos-latest", true},
		{"extra runner axis", "matrix:\n", "matrix:\n        os: [ubuntu-latest, ubuntu-22.04, macos-latest]\n", true},
		{"missing requirement", "require_file_handles: '1'", "", true},
		{"disabled Linux", "require_file_handles: '1'", "require_file_handles: '0'", true},
		{"required macOS", "require_file_handles: ''", "require_file_handles: '1'", true},
		{"step disables handles", "      - id: race-tests", "      - id: race-tests\n        env: {FU_REQUIRE_FILE_HANDLES: ''}", true},
		{"single package", "go test ./...", "go test ./internal/store", true},
		{"test filtering", "go test ./...", "go test -run Smoke ./...", true},
		{"optional suite", "      - id: race-tests", "      - id: race-tests\n        if: false", true},
		{"ignored failure", "      - id: race-tests", "      - id: race-tests\n        continue-on-error: true", true},
		{"valid command spacing", "go test ./... -race", "go  test  -race  ./...", false},
		{"unstable display name", "    name: test (${{ matrix.os }})\n", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := valid
			if test.from != "" {
				raw = strings.ReplaceAll(raw, test.from, test.to)
			}
			err := workflowFileHandleCoverageError([]byte(raw))
			if (err != nil) != test.wantError {
				t.Fatalf("validation=%v, want error=%v", err, test.wantError)
			}
		})
	}
}

func expressionBody(value string) string {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "${{") || !strings.HasSuffix(value, "}}") {
		return ""
	}
	return strings.TrimSpace(value[3 : len(value)-2])
}

// The guarded step uses one direct go test command. Shell wrappers and compound
// commands need an explicit guard update rather than an incomplete shell parse.
func fullRaceTestCommand(command string) bool {
	args := strings.Fields(command)
	if len(args) < 2 || args[0] != "go" || args[1] != "test" {
		return false
	}
	packages, race, count := 0, false, false
	for i := 2; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "./...":
			packages++
		case arg == "-race":
			race = true
		case arg == "-v":
		case arg == "-count" || strings.HasPrefix(arg, "-count="):
			value := strings.TrimPrefix(arg, "-count=")
			if arg == "-count" {
				i++
				if i >= len(args) {
					return false
				}
				value = args[i]
			}
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return false
			}
			count = true
		case arg == "-timeout" || strings.HasPrefix(arg, "-timeout="):
			value := strings.TrimPrefix(arg, "-timeout=")
			if arg == "-timeout" {
				i++
				if i >= len(args) {
					return false
				}
				value = args[i]
			}
			duration, err := time.ParseDuration(value)
			if err != nil || duration <= 0 {
				return false
			}
		default:
			return false
		}
	}
	return packages == 1 && race && count
}
