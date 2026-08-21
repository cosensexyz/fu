// internal/engine/lock.go
package engine

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockAcquiredHook observes each successful fu.lock acquisition. It is nil in
// production and set only by tests that assert a command takes the lock for a
// particular stage of its work.
//
// A hook rather than a contention test, because contention cannot isolate the
// acquisition that matters. Restore takes the lock twice -- once inside
// Reconcile for the link layer, once for the destructive half added in the
// previous review round -- so holding the lock in the foreground blocks the
// first acquisition and proves nothing about the second. Removing the second
// one entirely left every test in engine, cli and store green, which is
// exactly what this closes.
var lockAcquiredHook func(displayPath string)

// withLock serializes write commands across fu processes; read commands
// never take the lock (DESIGN §6).
func withLock(root *os.Root, lockName, displayPath string, fn func() error) error {
	// O_NOFOLLOW, like every other open in the write path. os.Root.OpenFile
	// resolves a symlink whose target stays inside the root, so a link planted
	// at $FU_HOME/fu.lock would make each process flock whatever inode the name
	// resolved to -- and two processes resolving differently would both believe
	// they held the write lock.
	dir, err := root.OpenFile(".", os.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("open lock directory for %s: %w", displayPath, err)
	}
	fd, err := unix.Openat(int(dir.Fd()), lockName,
		unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	closeDirErr := dir.Close()
	if err != nil {
		return fmt.Errorf("open lock %s: %w", displayPath, err)
	}
	if closeDirErr != nil {
		_ = unix.Close(fd)
		return closeDirErr
	}
	file := os.NewFile(uintptr(fd), displayPath)
	if file == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("open lock %s: invalid file descriptor", displayPath)
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("acquire lock %s: %w", displayPath, err)
	}
	defer unix.Flock(int(file.Fd()), unix.LOCK_UN)
	if lockAcquiredHook != nil {
		lockAcquiredHook(displayPath)
	}
	return fn()
}
