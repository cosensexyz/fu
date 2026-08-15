//go:build linux

package store

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func mapAtomicRenameError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) ||
		errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) {
		return fmt.Errorf("%w: filesystem does not support atomic %s rename: %w", ErrAtomicRenameUnsupported, operation, err)
	}
	return err
}
