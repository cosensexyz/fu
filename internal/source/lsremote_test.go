package source

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// initRemoteRepo builds a real on-disk repository reachable over the file
// transport, matching how source_test.go exercises git behaviour.
func initRemoteRepo(t *testing.T) (url, headCommit string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Skipf("git unavailable or failed (%v): %s", err, out)
		}
		return string(out)
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "one")
	head := strings.TrimSpace(run("rev-parse", "HEAD"))
	return "file://" + dir, head
}

func TestResolveRemoteRefReturnsTheCommitTheRefPointsAt(t *testing.T) {
	url, head := initRemoteRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got, err := Source{Kind: KindGit, URL: url}.ResolveRemoteRef(ctx, "refs/heads/main")
	if err != nil {
		t.Fatalf("ResolveRemoteRef: %v", err)
	}
	if got != head {
		t.Fatalf("commit = %q, want %q", got, head)
	}
}

func TestResolveRemoteRefReportsAMissingRefDistinctly(t *testing.T) {
	url, _ := initRemoteRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := Source{Kind: KindGit, URL: url}.ResolveRemoteRef(ctx, "refs/heads/nope")
	if !errors.Is(err, ErrRemoteRefNotFound) {
		t.Fatalf("error = %v, want ErrRemoteRefNotFound", err)
	}
}

// TestResolveRemoteRefReportsAnUnreachableRemoteAsSuch is design §7's third
// source-layer case -- "远端不可达". Fix round 2, Important #7: the
// suite shipped TestResolveRemoteRefRefusesANonGitSource in its place, which
// exercises the kind guard and never reaches the transport at all, so an
// unreachable remote was untested.
//
// The distinction under test is the one describeRemoteErr
// (internal/engine/outdated.go) renders differently on every `fu outdated`
// row: a remote that cannot be reached leaves the skill's state unknown,
// while ErrRemoteRefNotFound says the remote answered and does not have that
// ref. Collapsing the two would tell a user offline that their recorded ref
// is gone.
func TestResolveRemoteRefReportsAnUnreachableRemoteAsSuch(t *testing.T) {
	url, _ := initRemoteRepo(t)
	if err := os.RemoveAll(strings.TrimPrefix(url, "file://")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := Source{Kind: KindGit, URL: url}.ResolveRemoteRef(ctx, "refs/heads/main")
	if err == nil {
		t.Fatal("expected a failure against a repository that is no longer there")
	}
	if errors.Is(err, ErrRemoteRefNotFound) {
		t.Fatalf("an unreachable remote must not be reported as a vanished ref: %v", err)
	}
	if !strings.Contains(err.Error(), url) {
		t.Fatalf("the failure must name the remote it could not reach: %v", err)
	}
}

func TestResolveRemoteRefRefusesANonGitSource(t *testing.T) {
	_, err := Source{Kind: KindLocal, Path: t.TempDir()}.ResolveRemoteRef(context.Background(), "refs/heads/main")
	if err == nil {
		t.Fatal("expected a refusal for a local source")
	}
}

// TestResolveRemoteRefRefusesASymbolicRef pins round 2's Minor #5. A symbolic
// reference carries no hash of its own, and go-git advertises HEAD that way
// when the server supports symrefs, so Hash() answered ZeroHash and this
// function returned forty zeros with a nil error -- its stated contract broken
// silently. Downstream, a record with `ref: HEAD` and `ref_kind: branch`
// compared those zeros against the locked commit and reported the skill
// updatable forever.
//
// Not reachable through EncodeFields, which never writes HEAD; it is the same
// hand-edit class judgeGitUpdate's own empty-commit guard defends against, and
// it fails closed for the same reason.
func TestResolveRemoteRefRefusesASymbolicRef(t *testing.T) {
	url, head := initRemoteRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got, err := Source{Kind: KindGit, URL: url}.ResolveRemoteRef(ctx, "HEAD")
	if err == nil {
		t.Fatalf("a symbolic ref must not resolve; got %q", got)
	}
	if got != "" {
		t.Fatalf("a refusal must return no commit, got %q", got)
	}
	if strings.Contains(err.Error(), head) {
		t.Fatalf("the failure must not imply it resolved anything: %v", err)
	}
	// The reachable branch ref still resolves, so the guard did not simply
	// break resolution.
	if _, err := (Source{Kind: KindGit, URL: url}).ResolveRemoteRef(ctx, "refs/heads/main"); err != nil {
		t.Fatalf("a real hash reference must still resolve: %v", err)
	}
}

// TestResolveRemoteRefMarksAMalformedRecordDistinctly pins round 4's finding
// that both refusals above are facts about a hand-edited fu.yaml, not about
// the network. Without a sentinel of their own they reached the caller as
// plain errors and describeRemoteErr's fallback called them "unreachable"
// (internal/engine/outdated.go) -- telling a perfectly online user their
// network was down, which is the very misdirection the vanished-ref split
// exists to prevent.
func TestResolveRemoteRefMarksAMalformedRecordDistinctly(t *testing.T) {
	url, _ := initRemoteRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	src := Source{Kind: KindGit, URL: url}

	for _, ref := range []string{"", "HEAD"} {
		_, err := src.ResolveRemoteRef(ctx, ref)
		if !errors.Is(err, ErrRemoteRefUnusable) {
			t.Fatalf("ResolveRemoteRef(%q) = %v, want ErrRemoteRefUnusable", ref, err)
		}
		if errors.Is(err, ErrRemoteRefNotFound) {
			t.Fatalf("ResolveRemoteRef(%q): a malformed record is not a vanished ref: %v", ref, err)
		}
	}
	// A ref the remote genuinely does not advertise keeps its own sentinel, so
	// the new one did not swallow the distinction it sits beside.
	if _, err := src.ResolveRemoteRef(ctx, "refs/heads/gone"); !errors.Is(err, ErrRemoteRefNotFound) {
		t.Fatalf("a vanished ref must still be ErrRemoteRefNotFound: %v", err)
	}
}

// The empty-ref guard above had no counterpart for the URL, and go-git treats
// an empty URL as a *path*: transport.NewEndpoint("") falls into parseFile,
// which does filepath.Abs("") and returns a file endpoint on the process's
// working directory. So this function silently resolved against whatever
// repository the caller was standing in and returned a real commit with a nil
// error -- the answer depending on $PWD rather than on the record. Refused
// beside the empty ref, and with the same sentinel, because it is the same kind
// of fact: something about a hand-edited fu.yaml, not about the network.
//
// The engine's judgeGitUpdate carries the primary guard; this one is
// defence-in-depth for any future caller that reaches here another way.
func TestResolveRemoteRefRefusesAnEmptyURL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := Source{Kind: KindGit}.ResolveRemoteRef(ctx, "refs/heads/main")
	if !errors.Is(err, ErrRemoteRefUnusable) {
		t.Fatalf("err = %v, want ErrRemoteRefUnusable so describeRemoteErr calls it a record fault and not a network one", err)
	}
}
