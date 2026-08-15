//go:build darwin || linux

package store

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAtomicRenameUnsupportedErrorIsActionable(t *testing.T) {
	errnos := []error{unix.ENOTSUP, unix.EOPNOTSUPP, unix.ENOSYS}
	if runtime.GOOS == "linux" {
		errnos = append(errnos, unix.EINVAL)
	}
	for _, errno := range errnos {
		err := mapAtomicRenameError("no-replace", errno)
		if !errors.Is(err, ErrAtomicRenameUnsupported) {
			t.Fatalf("%v mapped to %v, want ErrAtomicRenameUnsupported", errno, err)
		}
		if !errors.Is(err, errno) {
			t.Fatalf("mapped error %v must preserve its underlying errno %v", err, errno)
		}
		if !strings.Contains(err.Error(), "filesystem") || !strings.Contains(err.Error(), "atomic") {
			t.Fatalf("unsupported rename diagnostic is not actionable: %v", err)
		}
	}
	if err := mapAtomicRenameError("exchange", unix.EIO); !errors.Is(err, unix.EIO) || errors.Is(err, ErrAtomicRenameUnsupported) {
		t.Fatalf("unrelated errno must pass through unchanged: %v", err)
	}
}

func TestMapAtomicRenameErrorDoesNotMisclassifyDarwinEINVAL(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("renameatx_np is Darwin-specific")
	}
	err := mapAtomicRenameError("exchange", unix.EINVAL)
	if !errors.Is(err, unix.EINVAL) || errors.Is(err, ErrAtomicRenameUnsupported) {
		t.Fatalf("Darwin EINVAL must remain a programming/argument error, got %v", err)
	}
}
