//go:build !darwin && !linux

package store

func renameExchange(int, string, int, string) error {
	return mapAtomicRenameError("exchange", ErrAtomicRenameUnsupported)
}
