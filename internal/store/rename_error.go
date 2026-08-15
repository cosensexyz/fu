package store

import "errors"

// ErrAtomicRenameUnsupported reports a filesystem that cannot provide the
// atomic rename primitive required for crash-safe store updates.
var ErrAtomicRenameUnsupported = errors.New("atomic rename unsupported")
