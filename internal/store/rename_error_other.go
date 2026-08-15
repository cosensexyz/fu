//go:build !darwin && !linux

package store

import (
	"fmt"
)

func mapAtomicRenameError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: platform or filesystem does not support atomic %s rename: %v", ErrAtomicRenameUnsupported, operation, err)
}
