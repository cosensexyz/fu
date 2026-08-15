//go:build darwin

package store

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// Darwin's renameatx_np uses EINVAL for malformed arguments as well as
// filesystem-specific failures, so only unambiguous capability errors are
// mapped to the actionable unsupported-filesystem sentinel.
func mapAtomicRenameError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) {
		return fmt.Errorf("%w: filesystem does not support atomic %s rename: %w", ErrAtomicRenameUnsupported, operation, err)
	}
	return err
}
