//go:build darwin

package store

import "golang.org/x/sys/unix"

func renameNoReplace(oldDirFD int, oldName string, newDirFD int, newName string) error {
	return mapAtomicRenameError("no-replace", unix.RenameatxNp(oldDirFD, oldName, newDirFD, newName, unix.RENAME_EXCL))
}
