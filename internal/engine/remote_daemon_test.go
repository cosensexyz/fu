package engine

import (
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"golang.org/x/sys/unix"

	"github.com/cosensexyz/fu/internal/store"
)

// startPushableGitDaemon serves base over git:// with receive-pack enabled,
// so both push and clone cross a real network transport rather than the
// file transport's subprocesses. It mirrors internal/source's
// startGitDaemon, which a test in another package cannot import; the one
// difference is --enable=receive-pack.
func startPushableGitDaemon(t *testing.T, base string) string {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git daemon integration requires git: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	cmd := exec.Command(gitPath, "daemon", "--reuseaddr", "--export-all", "--enable=receive-pack",
		"--base-path="+base, "--listen=127.0.0.1", "--port="+strconv.Itoa(port), base)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Skipf("git daemon is unavailable: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
		})
	}
	t.Cleanup(stop)
	address := "127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		select {
		case waitErr := <-done:
			t.Skipf("git daemon exited before accepting connections: %v: %s", waitErr, output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("git daemon did not accept connections: %s", output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "git://" + address + "/"
}

func TestPushAndCloneCrossTheGitProtocol(t *testing.T) {
	base := t.TempDir()
	if _, err := git.PlainInit(filepath.Join(base, "store.git"), true); err != nil {
		t.Fatal(err)
	}
	url := startPushableGitDaemon(t, base) + "store.git"

	a, _ := setupStore(t)
	if _, err := NewSkill(a, nil, "writer"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetRemote(url); err != nil {
		t.Fatal(err)
	}
	pushed, err := PushOperations(a, nil)
	if err != nil {
		t.Fatalf("push over git://: %v", err)
	}
	if pushed.UpToDate {
		t.Fatal("the first push must send something")
	}

	homeB := t.TempDir()
	cloned, err := CloneStore(homeB, url, nil)
	if err != nil {
		t.Fatalf("clone over git://: %v", err)
	}
	if cloned.Skills != 1 {
		t.Fatalf("clone must carry the skill, got %+v", cloned)
	}
	b, err := store.Open(homeB)
	if err != nil {
		t.Fatal(err)
	}
	remoteURL, configured, err := b.Remote()
	if err != nil || !configured || remoteURL != url {
		t.Fatalf("the clone must record the git:// origin, got %q %v %v", remoteURL, configured, err)
	}
}
