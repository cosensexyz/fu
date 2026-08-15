//go:build darwin

package store

import "golang.org/x/sys/unix"

func renameExchange(oldDirFD int, oldName string, newDirFD int, newName string) error {
	return mapAtomicRenameError("exchange", unix.RenameatxNp(oldDirFD, oldName, newDirFD, newName, unix.RENAME_SWAP))
}
