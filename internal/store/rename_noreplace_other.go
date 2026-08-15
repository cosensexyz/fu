//go:build !darwin && !linux

package store

func renameNoReplace(int, string, int, string) error {
	return mapAtomicRenameError("no-replace", ErrAtomicRenameUnsupported)
}
